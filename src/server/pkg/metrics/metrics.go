package metrics

import (
	"fmt"
	"time"

	"github.com/pachyderm/pachyderm/src/client/pkg/uuid"
	"github.com/pachyderm/pachyderm/src/client/version"
	db "github.com/pachyderm/pachyderm/src/server/pfs/db"

	"github.com/dancannon/gorethink"
	"go.pedge.io/lion/proto"
	kube "k8s.io/kubernetes/pkg/client/unversioned"
)

type Reporter struct {
	clusterID  string
	kubeClient *kube.Client
	dbClient   *gorethink.Session
	pfsDbName  string
	ppsDbName  string
}

func NewReporter(clusterID string, kubeClient *kube.Client, address string, pfsDbName string, ppsDbName string) (*Reporter, error) {
	dbClient, err := db.DbConnect(address)
	if err != nil {
		return nil, fmt.Errorf("Error connected to DB when reporting metrics: %v\n", err)
	}
	return &Reporter{
		clusterID:  clusterID,
		kubeClient: kubeClient,
		dbClient:   dbClient,
		pfsDbName:  pfsDbName,
		ppsDbName:  ppsDbName,
	}, nil
}

// Segment API allows for map[string]interface{} for a single user's traits
// But we only care about things that are countable for the moment
// map userID -> action name -> count
type countableActions map[string]interface{}
type countableUserActions map[string]countableActions

var userActions = make(countableUserActions)

type incrementUserAction struct {
	action string
	user   string
}

var incrementActionChannel = make(chan *incrementUserAction, 0)

type readBatchOfActions struct {
	channel chan countableUserActions
}

var readBatchOfActionsChannel = make(chan *readBatchOfActions, 0)

//IncrementUserAction updates a counter per user per action for an API method by name
func IncrementUserAction(userID string, action string) {
	incrementActionChannel <- &incrementUserAction{
		action: action,
		user:   userID,
	}
}

func (r *Reporter) dbMetrics(metrics *Metrics) {
	cursor, err := gorethink.Object(
		"Repos",
		gorethink.DB(r.pfsDbName).Table("Repos").Count(),
		"Commits",
		gorethink.DB(r.pfsDbName).Table("Commits").Count(),
		"ArchivedCommits",
		gorethink.DB(r.pfsDbName).Table("Commits").Filter(
			map[string]interface{}{
				"Archived": true,
			},
		).Count(),
		"CancelledCommits",
		gorethink.DB(r.pfsDbName).Table("Commits").Filter(
			map[string]interface{}{
				"Cancelled": true,
			},
		).Count(),
		"Files",
		gorethink.DB(r.pfsDbName).Table("Diffs").Group("Path").Ungroup().Count(),
		"Jobs",
		gorethink.DB(r.ppsDbName).Table("JobInfos").Count(),
		"Pipelines",
		gorethink.DB(r.ppsDbName).Table("PipelineInfos").Count(),
	).Run(r.dbClient)
	if err != nil {
		protolion.Errorf("Error Fetching Metrics:%+v", err)
	}
	cursor.One(&metrics)
}

// ReportMetrics blocks and reports metrics every 15 seconds
func (r *Reporter) ReportMetrics() {
	reportingTicker := time.NewTicker(time.Second * 15)
	for {
		select {
		case incrementAction := <-incrementActionChannel:
			if userActions[incrementAction.user] == nil {
				userActions[incrementAction.user] = make(countableActions)
			}
			val, ok := userActions[incrementAction.user][incrementAction.action]
			if ok {
				userActions[incrementAction.user][incrementAction.action] = val.(uint64) + uint64(1)
			} else {
				userActions[incrementAction.user][incrementAction.action] = uint64(0)
			}
			break
		case batchOfActions := <-readBatchOfActionsChannel:
			batchOfActions.channel <- userActions
			userActions = make(countableUserActions)
		case <-reportingTicker.C:
			r.reportClusterMetrics()
			r.reportUserMetrics()
		}
	}
}

func (r *Reporter) reportUserMetrics() {
	read := &readBatchOfActions{
		channel: make(chan countableUserActions, 0),
	}
	readBatchOfActionsChannel <- read
	batchOfUserActions := <-read.channel
	if len(batchOfUserActions) > 0 {
		reportUserMetricsToSegment(batchOfUserActions)
	}
}

func (r *Reporter) reportClusterMetrics() {
	metrics := &Metrics{}
	r.dbMetrics(metrics)
	externalMetrics(r.kubeClient, metrics)
	metrics.ID = r.clusterID
	metrics.PodID = uuid.NewWithoutDashes()
	metrics.Version = version.PrettyPrintVersion(version.Version)
	reportClusterMetricsToSegment(metrics)
}

package workflow

import (
	"context"

	"github.com/go-gorp/gorp"
	"github.com/rockbears/log"

	"github.com/ovh/cds/engine/cache"
	"github.com/ovh/cds/sdk"
	cdslog "github.com/ovh/cds/sdk/log"
	"github.com/ovh/cds/sdk/telemetry"
)

// Resync a workflow in the given workflow run
func Resync(ctx context.Context, db gorp.SqlExecutor, store cache.Store, proj sdk.Project, wr *sdk.WorkflowRun) error {
	options := LoadOptions{
		DeepPipeline:     true,
		WithIntegrations: true,
	}
	wf, errW := LoadByID(ctx, db, store, proj, wr.Workflow.ID, options)
	if errW != nil {
		return sdk.WrapError(errW, "Resync> Cannot load workflow")
	}

	// Resync new model
	oldNode := wr.Workflow.WorkflowData.Array()
	for i := range oldNode {
		nodeToUpdate := oldNode[i]
		for _, n := range wf.WorkflowData.Array() {
			if nodeToUpdate.Name == n.Name {
				nodeToUpdate.Context = n.Context
				break
			}
		}
	}

	//Resync map
	wr.Workflow.Pipelines = wf.Pipelines
	wr.Workflow.Applications = wf.Applications
	wr.Workflow.Environments = wf.Environments
	wr.Workflow.ProjectIntegrations = wf.ProjectIntegrations
	wr.Workflow.Integrations = wf.Integrations
	wr.Workflow.HookModels = wf.HookModels
	wr.Workflow.OutGoingHookModels = wf.OutGoingHookModels

	return UpdateWorkflowRun(ctx, db, wr)
}

// ResyncWorkflowRunStatus resync the status of workflow if you stop a node run when workflow run is building
func ResyncWorkflowRunStatus(ctx context.Context, db gorp.SqlExecutor, wr *sdk.WorkflowRun) (*ProcessorReport, error) {
	report := new(ProcessorReport)
	var counterStatus statusCounter
	for _, wnrs := range wr.WorkflowNodeRuns {
		for _, wnr := range wnrs {
			if wr.LastSubNumber == wnr.SubNumber {
				computeRunStatus(wnr.Status, &counterStatus)
			}
		}
	}

	var isInError bool
	var newStatus string
	for _, info := range wr.Infos {
		if info.Type == sdk.RunInfoTypeError && info.SubNumber == wr.LastSubNumber {
			isInError = true
			break
		}
	}

	if !isInError {
		newStatus = getRunStatus(counterStatus)
	}

	log.Debug(ctx, "ResyncWorkflowRunStatus> %s/%s %+v", newStatus, wr.Status, counterStatus)

	if newStatus != wr.Status {
		wr.Status = newStatus
		report.Add(ctx, *wr)
		return report, UpdateWorkflowRunStatus(db, wr)
	}

	return report, nil
}

// ResyncNodeRunsWithCommits load commits build in this node run and save it into node run
func ResyncNodeRunsWithCommits(ctx context.Context, db *gorp.DbMap, store cache.Store, proj sdk.Project, report *ProcessorReport) {
	ctx, end := telemetry.Span(ctx, "workflow.ResyncNodeRunsWithCommits")
	defer end()

	if report == nil {
		return
	}

	nodeRuns := report.nodes
	for _, nodeRun := range nodeRuns {
		if len(nodeRun.Commits) > 0 || nodeRun.ApplicationID == 0 {
			continue
		}

		go func(nrID int64) {
			tx, err := db.Begin()
			if err != nil {
				ctx := log.ContextWithStackTrace(ctx, err)
				log.Error(ctx, "ResyncNodeRuns> Cannot begin db tx: %v", sdk.WithStack(err))
				return
			}
			defer tx.Rollback() // nolint

			nr, err := LoadAndLockNodeRunByID(ctx, tx, nrID)
			if err != nil {
				ctx := log.ContextWithStackTrace(ctx, err)
				log.Error(ctx, "ResyncNodeRuns> Unable to load workflowNodeRun by id %d: %v", nrID, err)
				return
			}

			wr, err := LoadRunByID(ctx, tx, nr.WorkflowRunID, LoadRunOptions{})
			if err != nil {
				ctx := log.ContextWithStackTrace(ctx, err)
				log.Error(ctx, "ResyncNodeRuns> Unable to load workflowRun by id %d: %v", nr.WorkflowRunID, err)
				return
			}

			ctx := context.WithValue(ctx, cdslog.Project, wr.Workflow.ProjectKey)
			ctx = context.WithValue(ctx, cdslog.Workflow, wr.Workflow.Name)

			var nodeName string
			var app sdk.Application
			var env *sdk.Environment

			n := wr.Workflow.WorkflowData.NodeByID(nr.WorkflowNodeID)
			if n == nil {
				ctx := log.ContextWithStackTrace(ctx, err)
				log.Error(ctx, "ResyncNodeRuns> Unable to find node data by id %d in a workflow run id %d", nr.WorkflowNodeID, nr.WorkflowRunID)
				return
			}
			nodeName = n.Name
			if n.Context == nil || n.Context.ApplicationID == 0 {
				return
			}
			app = wr.Workflow.Applications[n.Context.ApplicationID]
			if n.Context.EnvironmentID != 0 {
				e := wr.Workflow.Environments[n.Context.EnvironmentID]
				env = &e
			}

			//New context because we are in goroutine
			log.Info(ctx, "ResyncNodeRuns> Retrieve node run commits on node run %s for workflow %s/%s#%d", proj.Key, wr.Workflow.Name, wr.Number, nodeName)
			commits, curVCSInfos, err := GetNodeRunBuildCommits(ctx, tx, store, proj, wr.Workflow, nodeName, wr.Number, nr, &app, env)
			if err != nil {
				ctx := log.ContextWithStackTrace(ctx, err)
				log.Error(ctx, "ResyncNodeRuns> cannot get build commits on a node run %s for workflow %s/%s#%d: %v", proj.Key, wr.Workflow.Name, wr.Number, nodeName, err)
			} else if commits != nil {
				nr.Commits = commits
			}

			if len(commits) > 0 {
				if err := updateNodeRunCommits(ctx, tx, nr.ID, commits); err != nil {
					ctx := log.ContextWithStackTrace(ctx, err)
					log.Error(ctx, "ResyncNodeRuns> Unable to update node run commits %v", err)
				}
			}

			tagsUpdated := false
			if curVCSInfos.Branch != "" && curVCSInfos.Tag == "" {
				tagsUpdated = wr.Tag(tagGitBranch, curVCSInfos.Branch)
			}
			if curVCSInfos.Hash != "" {
				tagsUpdated = wr.Tag(tagGitHash, curVCSInfos.Hash)
			}
			if curVCSInfos.Remote != "" {
				tagsUpdated = wr.Tag(tagGitRepository, curVCSInfos.Remote)
			}
			if curVCSInfos.Tag != "" {
				tagsUpdated = wr.Tag(tagGitTag, curVCSInfos.Tag)
			}

			if tagsUpdated {
				if err := UpdateWorkflowRunTags(tx, wr); err != nil {
					ctx := log.ContextWithStackTrace(ctx, err)
					log.Error(ctx, "ResyncNodeRuns> Unable to update workflow run tags %v", err)
				}
			}

			if err := tx.Commit(); err != nil {
				ctx := log.ContextWithStackTrace(ctx, err)
				log.Error(ctx, "ResyncNodeRuns> Cannot commit db tx: %v", sdk.WithStack(err))
			}
		}(nodeRun.ID)
	}
}

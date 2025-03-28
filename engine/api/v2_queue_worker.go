package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/rockbears/log"

	"github.com/ovh/cds/engine/api/authentication"
	workerauth "github.com/ovh/cds/engine/api/authentication/worker"
	"github.com/ovh/cds/engine/api/database/gorpmapping"
	"github.com/ovh/cds/engine/api/event_v2"
	"github.com/ovh/cds/engine/api/integration"
	"github.com/ovh/cds/engine/api/project"
	"github.com/ovh/cds/engine/api/vcs"
	"github.com/ovh/cds/engine/api/worker_v2"
	"github.com/ovh/cds/engine/api/workflow_v2"
	"github.com/ovh/cds/engine/gorpmapper"
	"github.com/ovh/cds/engine/service"
	"github.com/ovh/cds/sdk"
)

func (api *API) getJobRunProjectV2KeyHandler() ([]service.RbacChecker, service.Handler) {
	return []service.RbacChecker{api.jobRunUpdate, api.isWorker},
		func(ctx context.Context, w http.ResponseWriter, req *http.Request) error {
			vars := mux.Vars(req)

			runJobID := vars["runJobID"]
			keyName := vars["keyName"]

			clearKey := service.FormBool(req, "clearKey")

			runJob, err := workflow_v2.LoadRunJobByID(ctx, api.mustDB(), runJobID)
			if err != nil {
				return err
			}

			service.TrackActionMetadataFromFields(w, runJob)

			opts := make([]project.LoadOptionFunc, 0, 1)
			if clearKey {
				opts = append(opts, project.LoadOptions.WithClearKeys)
			}
			p, err := project.Load(ctx, api.mustDB(), runJob.ProjectKey, opts...)
			if err != nil {
				return err
			}
			for _, k := range p.Keys {
				if k.Name == keyName {
					return service.WriteJSON(w, k, http.StatusOK)
				}
			}
			return sdk.NewErrorFrom(sdk.ErrNotFound, "unable to find key %s", keyName)
		}
}

func (api *API) postV2WorkerTakeJobHandler() ([]service.RbacChecker, service.Handler) {
	return []service.RbacChecker{api.jobRunUpdate, api.isWorker}, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		jobRunID := vars["runJobID"]

		wk := getWorker(ctx)
		wrkWithSecret, err := worker_v2.LoadByID(ctx, api.mustDB(), wk.ID, gorpmapper.GetOptions.WithDecryption)
		if err != nil {
			return err
		}
		workerKey := wrkWithSecret.PrivateKey

		if wrkWithSecret.Status != sdk.StatusWaiting {
			return sdk.WithStack(sdk.ErrForbidden)
		}

		jobRun, err := workflow_v2.LoadRunJobByID(ctx, api.mustDB(), jobRunID)
		if err != nil {
			return err
		}

		if jobRun.Status != sdk.V2WorkflowRunJobStatusScheduling {
			return sdk.NewErrorFrom(sdk.ErrForbidden, "unable take the job %s, current status %s", jobRunID, jobRun.Status)
		}

		run, err := workflow_v2.LoadRunByID(ctx, api.mustDB(), jobRun.WorkflowRunID)
		if err != nil {
			return err
		}

		projWithSecrets, err := project.Load(ctx, api.mustDB(), run.ProjectKey, project.LoadOptions.WithClearKeys)
		if err != nil {
			return err
		}
		vcsWithSecrets, err := vcs.LoadVCSByIDAndProjectKey(ctx, api.mustDB(), projWithSecrets.Key, run.VCSServerID, gorpmapping.GetOptions.WithDecryption)
		if err != nil {
			return err
		}

		vss := make([]sdk.ProjectVariableSet, 0, len(jobRun.Job.VariableSets))
		for _, vsName := range jobRun.Job.VariableSets {
			vsDB, err := project.LoadVariableSetByName(ctx, api.mustDB(), projWithSecrets.Key, vsName)
			if err != nil {
				return err
			}
			vsDB.Items, err = project.LoadVariableSetAllItem(ctx, api.mustDB(), vsDB.ID, gorpmapper.GetAllOptions.WithDecryption)
			if err != nil {
				return err
			}
			vss = append(vss, *vsDB)
		}

		tx, err := api.mustDB().Begin()
		if err != nil {
			return sdk.WithStack(err)
		}
		defer tx.Rollback() // nolint

		now := time.Now()

		contexts, sensitiveDatas, err := computeRunJobContext(ctx, tx, projWithSecrets, vcsWithSecrets, vss, *run, *jobRun)
		if err != nil {
			info := sdk.V2WorkflowRunJobInfo{
				Level:            sdk.WorkflowRunInfoLevelError,
				IssuedAt:         now,
				WorkflowRunJobID: jobRun.ID,
				WorkflowRunID:    jobRun.WorkflowRunID,
				Message:          fmt.Sprintf("Worker %q is unable to take job %q: %v", wk.Name, jobRun.JobID, err.Error()),
			}
			if err := workflow_v2.InsertRunJobInfo(ctx, tx, &info); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return sdk.WithStack(err)
			}
			return err
		}

		// Change worker status
		wrkWithSecret.Status = sdk.StatusBuilding
		if err := worker_v2.Update(ctx, tx, wrkWithSecret); err != nil {
			return err
		}

		jobRun.Status = sdk.V2WorkflowRunJobStatusBuilding
		jobRun.Started = &now
		jobRun.WorkerName = wrkWithSecret.Name
		if err := workflow_v2.UpdateJobRun(ctx, tx, jobRun); err != nil {
			return err
		}

		info := sdk.V2WorkflowRunJobInfo{
			Level:            sdk.WorkflowRunInfoLevelInfo,
			IssuedAt:         now,
			WorkflowRunJobID: jobRun.ID,
			WorkflowRunID:    jobRun.WorkflowRunID,
			Message:          fmt.Sprintf("Worker %q is starting for job %q", wk.Name, jobRun.JobID),
		}
		if err := workflow_v2.InsertRunJobInfo(ctx, tx, &info); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return sdk.WithStack(err)
		}

		takeResponse := sdk.V2TakeJobResponse{
			RunJob:         *jobRun,
			AsCodeActions:  run.WorkflowData.Actions,
			SigningKey:     base64.StdEncoding.EncodeToString(workerKey),
			Contexts:       *contexts,
			SensitiveDatas: sensitiveDatas,
		}

		event_v2.PublishRunJobEvent(ctx, api.Cache, sdk.EventRunJobBuilding, *run, *jobRun)
		return service.WriteJSON(w, takeResponse, http.StatusOK)
	}
}

func buildSensitiveData(value string) []string {
	datas := make([]string, 0)

	// If multiline, add all lines as sensitive data
	datas = append(datas, strings.Split(value, "\n")...)
	datas = append(datas, strings.Split(value, "\\n")...)
	datas = append(datas, sdk.OneLineValue(value))
	return datas
}

func computeRunJobContext(ctx context.Context, db gorpmapper.SqlExecutorWithTx, proj *sdk.Project, vcs *sdk.VCSProject, vss []sdk.ProjectVariableSet, run sdk.V2WorkflowRun, jobRun sdk.V2WorkflowRunJob) (*sdk.WorkflowRunJobsContext, []string, error) {
	contexts := &sdk.WorkflowRunJobsContext{}
	contexts.CDS = run.Contexts.CDS
	contexts.CDS.Job = jobRun.JobID
	contexts.CDS.Stage = jobRun.Job.Stage
	contexts.Git = run.Contexts.Git
	contexts.Gate = jobRun.GateInputs
	contexts.Matrix = jobRun.Matrix

	sensitiveDatas := sdk.StringSlice{}

	if vcs.Auth.Token != "" {
		contexts.Git.Token = vcs.Auth.Token
		sensitiveDatas = append(sensitiveDatas, buildSensitiveData(vcs.Auth.Token)...)
	}

	// Build var context
	varCtx, varSecret, err := buildVarsContext(ctx, vss)
	if err != nil {
		return nil, nil, err
	}
	contexts.Vars = varCtx
	sensitiveDatas = append(sensitiveDatas, varSecret...)

	contexts.Env = make(map[string]string)
	for k, v := range run.Contexts.Env {
		contexts.Env[k] = v
	}
	// override with job env
	for k, v := range jobRun.Job.Env {
		contexts.Env[k] = v
	}

	runResults, err := workflow_v2.LoadRunResultsByRunIDAttempt(ctx, db, run.ID, run.RunAttempt)
	if err != nil {
		return nil, nil, err
	}

	runJobs, err := workflow_v2.LoadRunJobsByRunIDAndStatus(ctx, db, run.ID, []string{sdk.StatusFail, sdk.StatusSkipped, sdk.StatusSuccess, sdk.StatusStopped}, run.RunAttempt)
	if err != nil {
		return nil, nil, err
	}
	contexts.Jobs = sdk.JobsResultContext{}
	for _, rj := range runJobs {
		jobResult := sdk.JobResultContext{
			Result:  rj.Status,
			Outputs: sdk.JobResultOutput{},
		}
		for _, r := range runResults {
			if r.WorkflowRunJobID != rj.ID {
				continue
			}
			log.Debug(ctx, "computeRunJobContext> processing run result %s %s", r.Type, r.Name())
			switch r.Type {
			case sdk.V2WorkflowRunResultTypeVariable, sdk.V2WorkflowRunResultVariableDetailType:
				x, err := sdk.GetConcreteDetail[*sdk.V2WorkflowRunResultVariableDetail](&r)
				if err != nil {
					log.ErrorWithStackTrace(ctx, err)
					continue
				}
				jobResult.Outputs[x.Name] = x.Value
			default:
				if jobResult.JobRunResults == nil {
					jobResult.JobRunResults = sdk.JobRunResults{}
				}
				jobResult.JobRunResults[r.Name()], _ = r.GetDetail()
			}
		}
		contexts.Jobs[rj.JobID] = jobResult
	}

	contexts.Needs = sdk.NeedsContext{}
	for _, n := range jobRun.Job.Needs {
		if j, has := contexts.Jobs[n]; has {
			needContext := sdk.NeedContext{
				Result:  j.Result,
				Outputs: contexts.Jobs[n].Outputs,
			}
			if j.Result == sdk.V2WorkflowRunJobStatusFail && run.WorkflowData.Workflow.Jobs[n].ContinueOnError {
				needContext.Result = sdk.V2WorkflowRunJobStatusSuccess
			}
			contexts.Needs[n] = needContext
		}
	}

	contexts.Integrations = &sdk.JobIntegrationsContexts{}
	integs, err := integration.LoadIntegrationsByProjectIDWithClearPassword(ctx, db, proj.ID) // Here
	if err != nil {
		return nil, nil, sdk.NewErrorFrom(sdk.ErrNotFound, "unable to load integration")
	}

	// this private function is called on job integrations to set the integration on the context, and then on the workflow integration
	// the job integration are always predominant on workflow integration
	var matchIntegration = func(i string) error {
		var integ *sdk.ProjectIntegration
		for j := range integs {
			if integs[j].Name == i {
				integ = &integs[j]
				break
			}
		}
		if integ == nil {
			return sdk.NewErrorFrom(sdk.ErrNotFound, "integration %q not found", i)
		}
		switch {
		case integ.Model.ArtifactManager:
			if contexts.Integrations.ArtifactManager.Name != "" {
				return nil // If it's already set, it's by job integration
			}
			contexts.Integrations.ArtifactManager = sdk.JobIntegrationsContext{
				Name:      integ.Name,
				Config:    integ.ToJobRunContextConfig(),
				ModelName: integ.Model.Name,
			}
		case integ.Model.Deployment:
			if contexts.Integrations.Deployment.Name != "" {
				return nil // If it's already set, it's by job integration
			}
			contexts.Integrations.Deployment = sdk.JobIntegrationsContext{
				Name:      integ.Name,
				Config:    integ.ToJobRunContextConfig(),
				ModelName: integ.Model.Name,
			}
		default:
			return sdk.NewErrorFrom(sdk.ErrNotFound, "integration %q not supported", i)
		}
		if err := workflow_v2.InsertRunJobInfo(ctx, db, &sdk.V2WorkflowRunJobInfo{
			IssuedAt:         time.Now(),
			Level:            sdk.WorkflowRunInfoLevelInfo,
			WorkflowRunID:    run.ID,
			WorkflowRunJobID: jobRun.ID,
			Message:          fmt.Sprintf("Integration %q enabled on job %q", integ.Name, jobRun.JobID),
		}); err != nil {
			return err
		}

		// Reload integration with secret
		currentInteg, err := integration.LoadProjectIntegrationByIDWithClearPassword(ctx, db, integ.ID)
		if err != nil {
			return err
		}
		for _, v := range currentInteg.Config {
			if v.Type == sdk.IntegrationConfigTypePassword {
				sensitiveDatas = append(sensitiveDatas, v.Value)
			}
		}

		if _, has := currentInteg.Model.PublicConfigurations[currentInteg.Name]; has {
			for _, publicConfigValue := range currentInteg.Model.PublicConfigurations[currentInteg.Name] {
				if publicConfigValue.Type == sdk.IntegrationConfigTypePassword {
					sensitiveDatas = append(sensitiveDatas, publicConfigValue.Value)
				}
			}
		}
		return nil
	}

	// Load integration from the job level
	// This must be done before the workflow level
	for _, i := range jobRun.Job.Integrations {
		if err := matchIntegration(i); err != nil {
			return nil, nil, err
		}
	}

	// Load integration from the workflow level
	for _, i := range run.WorkflowData.Workflow.Integrations {
		if err := matchIntegration(i); err != nil {
			return nil, nil, err
		}
	}

	sensitiveDatas.Unique()
	return contexts, sensitiveDatas, nil
}

func buildVarsContext(ctx context.Context, vss []sdk.ProjectVariableSet) (map[string]interface{}, sdk.StringSlice, error) {
	varCtx := make(map[string]interface{})
	sensitiveDatas := sdk.StringSlice{}
	for _, vs := range vss {
		vsMap := make(map[string]interface{})
		for _, item := range vs.Items {
			if strings.HasPrefix(item.Value, "{") && strings.HasSuffix(item.Value, "}") {
				var jsonValue map[string]interface{}
				if err := json.Unmarshal([]byte(item.Value), &jsonValue); err != nil {
					vsMap[item.Name] = item.Value
					if item.Type == sdk.ProjectVariableTypeSecret {
						sensitiveDatas = append(sensitiveDatas, buildSensitiveData(item.Value)...)
					}
				} else {
					vsMap[item.Name] = jsonValue

					if item.Type == sdk.ProjectVariableTypeSecret {
						datas, err := getAllSensitiveDataFromJson(ctx, jsonValue)
						if err != nil {
							return nil, nil, err
						}
						sensitiveDatas = append(sensitiveDatas, datas...)
					}

				}
			} else if strings.HasPrefix(item.Value, "[") && strings.HasSuffix(item.Value, "]") {
				var jsonArrayValue []interface{}
				if err := json.Unmarshal([]byte(item.Value), &jsonArrayValue); err != nil {
					if item.Type == sdk.ProjectVariableTypeSecret {
						sensitiveDatas = append(sensitiveDatas, buildSensitiveData(item.Value)...)
					}
				} else {
					vsMap[item.Name] = jsonArrayValue

					if item.Type == sdk.ProjectVariableTypeSecret {
						datas, err := getAllSensitiveDataFromJsonArray(ctx, jsonArrayValue)
						if err != nil {
							return nil, nil, err
						}
						sensitiveDatas = append(sensitiveDatas, datas...)
					}
				}
			} else {
				vsMap[item.Name] = item.Value
			}
			if item.Type == sdk.ProjectVariableTypeSecret {
				sensitiveDatas = append(sensitiveDatas, buildSensitiveData(item.Value)...)
			}
		}
		varCtx[vs.Name] = vsMap
	}
	return varCtx, sensitiveDatas, nil
}

func getAllSensitiveDataFromJsonArray(ctx context.Context, secretJsonValue []interface{}) ([]string, error) {
	datas := make([]string, 0)
	bts, err := json.Marshal(secretJsonValue)
	if err != nil {
		return nil, sdk.NewErrorFrom(sdk.ErrInvalidData, "unable to unmarshal json secret value: %v", err)
	}
	// Add JSON value with indent in sensitive data in case of user using toJson function.
	datas = append(datas, buildSensitiveData(string(bts))...)

	// Retrieve sensitive value
	for _, arrayItem := range secretJsonValue {
		if itemMap, ok := arrayItem.(map[string]interface{}); ok {
			dataFromMap, err := getAllSensitiveDataFromJson(ctx, itemMap)
			if err != nil {
				return nil, err
			}
			datas = append(datas, dataFromMap...)
		} else {
			datas = append(datas, buildSensitiveData(fmt.Sprintf("%v", arrayItem))...)
		}
	}
	return datas, nil
}

func getAllSensitiveDataFromJson(ctx context.Context, secretJsonValue map[string]interface{}) ([]string, error) {
	datas := make([]string, 0)
	bts, err := json.Marshal(secretJsonValue)
	if err != nil {
		return nil, sdk.NewErrorFrom(sdk.ErrInvalidData, "unable to unmarshal json secret value: %v", err)
	}
	// Add JSON value with indent in sensitive data in case of user using toJson function.
	datas = append(datas, buildSensitiveData(string(bts))...)

	// browse all keys in value
	for _, value := range secretJsonValue {
		if valueMap, ok := value.(map[string]interface{}); ok { // if value is map
			dataFromMap, err := getAllSensitiveDataFromJson(ctx, valueMap)
			if err != nil {
				return nil, err
			}
			datas = append(datas, dataFromMap...)
		} else if valueArray, ok := value.([]interface{}); ok { // if value is array
			dataFromArray, err := getAllSensitiveDataFromJsonArray(ctx, valueArray)
			if err != nil {
				return nil, err
			}
			datas = append(datas, dataFromArray...)
		} else { // if string and numbers and other ...
			datas = append(datas, buildSensitiveData(fmt.Sprintf("%v", value))...)
		}
	}
	return datas, nil
}

func (api *API) postV2RefreshWorkerHandler() ([]service.RbacChecker, service.Handler) {
	return []service.RbacChecker{api.jobRunUpdate, api.isWorker}, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		wk := getWorker(ctx)
		vars := mux.Vars(r)
		jobRunID := vars["runJobID"]

		jobRun, err := workflow_v2.LoadRunJobByID(ctx, api.mustDB(), jobRunID)
		if err != nil {
			return err
		}
		if jobRun.Status.IsTerminated() {
			return sdk.NewErrorFrom(sdk.ErrAlreadyEnded, "job ended: %s", jobRun.Status)
		}

		wk.LastBeat = time.Now()
		tx, err := api.mustDB().Begin()
		if err != nil {
			return sdk.WithStack(err)
		}
		if err := worker_v2.Update(ctx, tx, wk); err != nil {
			return err
		}
		return sdk.WithStack(tx.Commit())
	}
}

func (api *API) postV2RegisterWorkerHandler() ([]service.RbacChecker, service.Handler) {
	return nil, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		vars := mux.Vars(r)
		jobRunID := vars["runJobID"]
		regionName := vars["regionName"]

		// First get the jwt token to checks where this registration is coming from
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if jwt == "" {
			return sdk.WithStack(sdk.ErrUnauthorized)
		}

		var registrationForm sdk.WorkerRegistrationForm
		if err := service.UnmarshalBody(r, &registrationForm); err != nil {
			return err
		}

		// Check that the worker can authentify on CDS API
		workerTokenFromHatchery, hatch, err := workerauth.VerifyTokenV2(ctx, api.mustDB(), jwt)
		if err != nil {
			return sdk.NewErrorWithStack(sdk.WrapError(err, "unauthorized worker jwt token %s", jwt), sdk.ErrUnauthorized)
		}

		if err := hatcheryHasRoleOnRegion(ctx, api.mustDB(), hatch.ID, regionName, sdk.HatcheryRoleSpawn); err != nil {
			return err
		}

		hatcheryConsumer, err := authentication.LoadHatcheryConsumerByName(ctx, api.mustDB(), hatch.Name)
		if err != nil {
			return sdk.WrapError(err, "unable to load hatchery %s consumer", hatch.ID)
		}

		// Check runjob status
		runJob, err := workflow_v2.LoadRunJobByID(ctx, api.mustDB(), workerTokenFromHatchery.Worker.RunJobID)
		if err != nil {
			return err
		}

		service.TrackActionMetadataFromFields(w, runJob)

		if runJob.Status != sdk.V2WorkflowRunJobStatusScheduling || runJob.HatcheryName != hatch.Name || runJob.ID != jobRunID || runJob.Region != regionName {
			return sdk.WrapError(sdk.ErrForbidden, "unable to take job %s, current status: %s, hatchery: %s, region: %s", runJob.ID, runJob.Status, runJob.HatcheryName, runJob.Region)
		}

		tx, err := api.mustDB().Begin()
		if err != nil {
			return sdk.WithStack(err)
		}
		defer tx.Rollback() // nolint

		// We have to issue a new consumer for the worker
		workerConsumer, err := authentication.NewConsumerWorkerV2(ctx, tx, workerTokenFromHatchery.Subject, hatcheryConsumer)
		if err != nil {
			return err
		}

		// Try to register worker
		wk, err := worker_v2.RegisterWorker(ctx, tx, workerTokenFromHatchery.Worker, *hatch, workerConsumer, registrationForm)
		if err != nil {
			return sdk.NewErrorWithStack(
				sdk.WrapError(err, "[%s] Registering failed", workerTokenFromHatchery.Worker.WorkerName),
				sdk.ErrUnauthorized,
			)
		}

		log.Debug(ctx, "New worker: [%s] - %s", wk.ID, wk.Name)

		workerSession, err := authentication.NewSession(ctx, tx, &workerConsumer.AuthConsumer, workerauth.SessionDuration)
		if err != nil {
			return sdk.NewErrorWithStack(
				sdk.WrapError(err, "[%s] Registering failed", workerTokenFromHatchery.Worker.WorkerName),
				sdk.ErrUnauthorized,
			)
		}

		// Store the last authentication date on the consumer
		now := time.Now()
		workerConsumer.LastAuthentication = &now
		if err := authentication.UpdateConsumerLastAuthentication(ctx, tx, &workerConsumer.AuthConsumer); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return sdk.WithStack(err)
		}

		jwt, err = authentication.NewSessionJWT(workerSession, "")
		if err != nil {
			return sdk.NewErrorWithStack(
				sdk.WrapError(err, "[%s] Registering failed", workerTokenFromHatchery.Worker.WorkerName),
				sdk.ErrUnauthorized,
			)
		}

		// Set the JWT token as a header
		log.Debug(ctx, "worker.registerWorkerHandler> X-CDS-JWT:%s", sdk.StringFirstN(jwt, 12))
		w.Header().Add("X-CDS-JWT", jwt)

		// Return worker info to worker itself
		return service.WriteJSON(w, wk, http.StatusOK)
	}
}

func (api *API) postV2UnregisterWorkerHandler() ([]service.RbacChecker, service.Handler) {
	return []service.RbacChecker{api.jobRunUpdate, api.isWorker}, func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		wk := getWorker(ctx)

		tx, err := api.mustDB().Begin()
		if err != nil {
			return sdk.WithStack(err)
		}
		wk.Status = sdk.StatusDisabled
		if err := worker_v2.Update(ctx, tx, wk); err != nil {
			return err
		}
		return sdk.WithStack(tx.Commit())
	}
}

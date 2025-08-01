package hatchery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	"github.com/rockbears/log"
	"github.com/rockbears/yaml"
	"go.opencensus.io/stats"
	"go.opencensus.io/trace"

	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/cdsclient"
	"github.com/ovh/cds/sdk/telemetry"
)

var (
	// Client is a CDS Client
	Client                                cdsclient.HTTPClient
	defaultMaxProvisioning                = 10
	models                                []sdk.Model
	defaultMaxAttemptsNumberBeforeFailure = 5
	CacheNbAttemptsIDsTTL                 = 1 * time.Hour
)

type CacheNbAttemptsJobIDs struct {
	cache *cache.Cache
}

func (c *CacheNbAttemptsJobIDs) Key(id int64) string {
	return strconv.FormatInt(id, 10)
}

func (c *CacheNbAttemptsJobIDs) NewAttempt(key string) int {
	nbAttempt, err := c.cache.IncrementInt(key, 1)
	if err != nil {
		c.cache.SetDefault(key, 1)
	}
	return nbAttempt
}

func (c *CacheNbAttemptsJobIDs) GetAttempt(key string) int {
	nbAttempt, has := c.cache.Get(key)
	if !has {
		return 0
	}
	nbAttemptInt, ok := nbAttempt.(int)
	if !ok {
		return 0
	}
	return nbAttemptInt
}

// Create creates hatchery
func Create(ctx context.Context, h Interface) error {
	ctx = telemetry.ContextWithTag(ctx,
		telemetry.TagServiceName, h.Name(),
		telemetry.TagServiceType, h.Type(),
	)

	if err := InitMetrics(ctx); err != nil {
		return err
	}

	// Init call hatchery.Register()
	if err := h.InitHatchery(ctx); err != nil {
		return sdk.WrapError(err, "init error")
	}

	var chanRegister, chanGetModels <-chan time.Time
	var modelType string

	hWithModels, hatcheryIsWithModels := h.(InterfaceWithModels)
	if hatcheryIsWithModels {
		// Call WorkerModel Enabled first
		var errwm error
		models, errwm = hWithModels.WorkerModelsEnabled()
		if errwm != nil {
			log.Error(ctx, "error on h.WorkerModelsEnabled() (init call): %v", errwm)
			return errwm
		}

		// using time.Tick leaks the underlying ticker but we don't care about it because it is an endless function
		chanRegister = time.Tick(time.Duration(h.Configuration().Provision.RegisterFrequency) * time.Second) // nolint
		chanGetModels = time.Tick(10 * time.Second)                                                          // nolint

		modelType = hWithModels.ModelType()
	}

	wjobs := make(chan int64, h.Configuration().Provision.MaxConcurrentProvisioning)
	v2Runjobs := make(chan string, h.Configuration().Provision.MaxConcurrentProvisioning)
	errs := make(chan error, 1)

	// Create a cache to only process each jobID only a number of attempts before force to fail the job
	cacheNbAttemptsIDs := &CacheNbAttemptsJobIDs{
		cache: cache.New(CacheNbAttemptsIDsTTL, 2*CacheNbAttemptsIDsTTL),
	}

	if h.CDSClientV2() != nil {
		h.GetGoRoutines().Run(ctx, "V2QueuePolling", func(ctx context.Context) {
			log.Debug(ctx, "starting v2 queue polling")

			if err := h.CDSClientV2().V2QueuePolling(ctx, h.GetRegion(), h.Configuration().OSArch, h.GetGoRoutines(), GetMetrics(), h.GetMapPendingWorkerCreation(), v2Runjobs, errs, 20*time.Second); err != nil {
				log.Error(ctx, "V2 Queues polling stopped: %v", err)
			}
		})
	}

	h.GetGoRoutines().Run(ctx, "queuePolling", func(ctx context.Context) {
		log.Debug(ctx, "starting queue polling")

		var ms []cdsclient.RequestModifier
		if modelType != "" {
			ms = append(ms, cdsclient.ModelType(modelType))
		}
		region := h.Configuration().Provision.Region
		if region != "" {
			regions := []string{region}
			if !h.Configuration().Provision.IgnoreJobWithNoRegion {
				regions = append(regions, "")
			}
			ms = append(ms, cdsclient.Region(regions...))
		}

		filters := []sdk.WebsocketFilter{
			{
				HatcheryType: modelType,
				Type:         sdk.WebsocketFilterTypeQueue,
			},
		}
		if err := h.CDSClient().QueuePolling(ctx, h.GetGoRoutines(), GetMetrics(), h.GetMapPendingWorkerCreation(), wjobs, errs, filters, 20*time.Second, ms...); err != nil {
			log.Error(ctx, "Queues polling stopped: %v", err)
		}
	})

	// run the starters pool
	workersStartChan := startWorkerStarters(ctx, h)

	hostname, err := os.Hostname()
	if err != nil {
		return sdk.WrapError(err, "cannot retrieve hostname")
	}

	// read the errs channel in another goroutine too
	h.GetGoRoutines().Run(ctx, "checkErrs", func(ctx context.Context) {
		for err := range errs {
			log.Error(ctx, "%v", err)
		}
	})

	h.GetGoRoutines().Run(ctx, "mainRoutine", func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				log.Error(ctx, "cancel hatchery main routine: %v", ctx.Err())
				return

			case <-chanGetModels:
				var errwm error
				models, errwm = hWithModels.WorkerModelsEnabled()
				if errwm != nil {
					log.Error(ctx, "error on h.WorkerModelsEnabled(): %v", errwm)
				}
			case j := <-v2Runjobs:
				if err := handleJobV2(ctx, h, j, cacheNbAttemptsIDs, workersStartChan); err != nil {
					log.ErrorWithStackTrace(ctx, err)
				}
			case j := <-wjobs:
				if err := handleJob(ctx, hostname, h, j, cacheNbAttemptsIDs, workersStartChan); err != nil {
					log.ErrorWithStackTrace(ctx, err)
				}
			case <-chanRegister:
				if err := workerRegister(ctx, hWithModels, workersStartChan); err != nil {
					log.Warn(ctx, "error on workerRegister: %v", err)
				}
			}
		}
	})
	return nil
}

func handleJob(ctx context.Context, hostname string, h Interface, jobID int64, cacheAttempts *CacheNbAttemptsJobIDs, workersStartChan chan<- workerStarterRequest) error {
	if jobID == 0 {
		return nil
	}

	j, err := h.CDSClient().QueueJobInfo(ctx, strconv.FormatInt(jobID, 10))
	if sdk.ErrorIs(err, sdk.ErrWorkflowNodeRunJobNotFound) {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "unable to get job %v info", jobID)
	}

	currentCtx, currentCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	currentCtx = telemetry.ContextWithTag(currentCtx,
		telemetry.TagServiceName, h.Name(),
		telemetry.TagServiceType, h.Type(),
	)
	fields := log.FieldValues(ctx)
	for k, v := range fields {
		currentCtx = context.WithValue(currentCtx, k, v)
	}
	currentCtx = context.WithValue(currentCtx, LogFieldJobID, strconv.Itoa(int(j.ID)))
	currentCtx = context.WithValue(currentCtx, LogFieldProjectID, j.ProjectID)
	currentCtx = context.WithValue(currentCtx, LogFieldNodeRunID, j.WorkflowNodeRunID)
	logStepInfo(currentCtx, "dequeue", j.Queued)

	var endCurrentCtx context.CancelFunc
	if val, has := j.Header.Get(telemetry.SampledHeader); has && val == "1" {
		r, _ := j.Header.Get(sdk.WorkflowRunHeader)
		w, _ := j.Header.Get(sdk.WorkflowHeader)
		p, _ := j.Header.Get(sdk.ProjectKeyHeader)

		currentCtx = telemetry.New(currentCtx, h, "hatchery.JobReceive", trace.AlwaysSample(), trace.SpanKindServer)
		currentCtx, endCurrentCtx = telemetry.Span(currentCtx, "hatchery.JobReceive", telemetry.Tag(telemetry.TagWorkflow, w),
			telemetry.Tag(telemetry.TagWorkflowRun, r),
			telemetry.Tag(telemetry.TagProjectKey, p),
			telemetry.Tag(telemetry.TagWorkflowNodeJobRun, j.ID))
	}
	endTrace := func(reason string, jobID string) {
		if jobID != "" {
			h.GetMapPendingWorkerCreation().RemoveJobFromPendingWorkerCreation(jobID)
		}
		if currentCancel != nil {
			currentCancel()
		}
		if reason != "" {
			telemetry.Current(currentCtx,
				telemetry.Tag("reason", reason),
			)
		}
		if endCurrentCtx != nil {
			endCurrentCtx()
		}
		telemetry.End(ctx, nil, nil)
	}
	go func() {
		<-currentCtx.Done()
		endTrace(currentCtx.Err().Error(), "")
	}()

	stats.Record(currentCtx, GetMetrics().Jobs.M(1))

	// Check bookedBy current hatchery
	if j.Status != sdk.StatusWaiting {
		log.Debug(currentCtx, "hatchery> job %d is not waiting: %s", jobID, j.Status)
		endTrace("job is not waiting", strconv.FormatInt(jobID, 10))
		return nil
	}
	if j.BookedBy.ID != 0 {
		log.Debug(currentCtx, "hatchery> job %d is already booked", jobID)
		endTrace("booked by someone", strconv.FormatInt(jobID, 10))
		return nil
	}

	//Check if hatchery is able to start a new worker
	if !checkCapacities(currentCtx, h) {
		log.Info(currentCtx, "hatchery %s is not able to provision new worker for job %v", h.Service().Name)
		endTrace("no capacities", strconv.FormatInt(j.ID, 10))
		return nil
	}

	workerRequest := workerStarterRequest{
		ctx:               currentCtx,
		cancel:            currentCancel,
		id:                strconv.FormatInt(j.ID, 10),
		execGroups:        j.ExecGroups,
		requirements:      j.Job.Action.Requirements,
		hostname:          hostname,
		queued:            j.Queued,
		workflowNodeRunID: j.WorkflowNodeRunID,
		model:             sdk.WorkerStarterWorkerModel{},
	}

	// Check at least one worker model can match
	var chosenModel *sdk.Model
	var workerStarterModelWithModelv2 *sdk.WorkerStarterWorkerModel
	var canTakeJob bool

	var containsRegionRequirement bool
	var jobModel string
	for _, r := range workerRequest.requirements {
		switch r.Type {
		case sdk.RegionRequirement:
			containsRegionRequirement = true
		case sdk.ModelRequirement:
			jobModel = r.Value
		}
	}

	hWithModels, hatcheryIsWithModels := h.(InterfaceWithModels)

	if !containsRegionRequirement && h.Configuration().Provision.IgnoreJobWithNoRegion {
		log.Debug(currentCtx, "cannot launch this job because it does not contains a region prerequisite and IgnoreJobWithNoRegion=true in hatchery configuration")
		canTakeJob = false
	} else if hatcheryIsWithModels {
		// Test ascode model
		modelPath := strings.Split(jobModel, "/")
		if len(modelPath) >= 5 {
			if h.CDSClientV2() == nil {
				endTrace("no clientv2", strconv.FormatInt(j.ID, 10))
				return nil
			}
			var err error
			chosenModel, workerStarterModelWithModelv2, err = canRunJobWithModelV2(currentCtx, hWithModels, workerRequest, jobModel)
			if err != nil {
				log.Error(currentCtx, "%v", err)
				endTrace("err on chosenModel", strconv.FormatInt(j.ID, 10))
				return nil
			}
		} else {
			if jobModel == "" {
				// Here, we found a modelv1. Check if we can use a model v2
				var err error
				workerStarterModelWithModelv2, err = checkDefaultModelV2(ctx, hWithModels, workerRequest, jobModel)
				if err != nil {
					log.Error(currentCtx, "%v", err)
				}
			}

			// if we didn't find a model v2 that can run the job v1, search a model v1
			if workerStarterModelWithModelv2 == nil {
				for i := range models {
					// find the first model matching with the pre-requisite
					if can := canRunJobWithModel(currentCtx, hWithModels, workerRequest, &models[i]); can {
						chosenModel = &models[i]
						break
					}
				}
			}
		}

		// No model has been found, let's send a failing result
		if chosenModel == nil && workerStarterModelWithModelv2 == nil {
			log.Debug(currentCtx, "hatchery> no model")
			endTrace("no model", strconv.FormatInt(j.ID, 10))
			return nil
		}
		canTakeJob = true
	} else {
		if can := canRunJob(currentCtx, h, workerRequest); can {
			log.Debug(currentCtx, "hatchery %s can try to spawn a worker for job %d", h.Name(), j.ID)
			canTakeJob = true
		}
	}

	if !canTakeJob {
		log.Info(currentCtx, "hatchery %s is not able to run the job %d", h.Name(), j.ID)
		endTrace("cannot run job", strconv.FormatInt(j.ID, 10))
		return nil
	}

	if workerStarterModelWithModelv2 != nil {
		workerRequest.model = *workerStarterModelWithModelv2
	} else if chosenModel != nil {
		// We got a model, let's start a worker
		workerRequest.model.ModelV1 = chosenModel

		// Interpolate model secrets
		if err := ModelInterpolateSecrets(hWithModels, chosenModel); err != nil {
			log.Error(currentCtx, "%v", err)
			endTrace("error on secret interpolation", strconv.FormatInt(j.ID, 10))
			return nil
		}
	}

	if hatcheryIsWithModels {
		can, err := hWithModels.CanAllocateResources(ctx, workerRequest.model, workerRequest.id, workerRequest.requirements)
		if err != nil {
			log.Error(ctx, "hatchery %q failed to check canAllocateResources: %v", h.Configuration().Name, err)
			endTrace(fmt.Sprintf("%v", err.Error()), strconv.FormatInt(j.ID, 10))
			return err
		}
		if !can {
			log.Warn(ctx, "cannot allocate resource for worker")
			endTrace("cannot allocate resource", strconv.FormatInt(j.ID, 10))
			return nil
		}
	}

	// Check if we already try to start a worker for this job
	maxAttemptsNumberBeforeFailure := h.Configuration().Provision.MaxAttemptsNumberBeforeFailure
	if maxAttemptsNumberBeforeFailure > -1 {
		nbAttempts := cacheAttempts.NewAttempt(cacheAttempts.Key(j.ID))
		if maxAttemptsNumberBeforeFailure == 0 {
			maxAttemptsNumberBeforeFailure = defaultMaxAttemptsNumberBeforeFailure
		}
		if nbAttempts > maxAttemptsNumberBeforeFailure {
			if err := h.CDSClient().
				QueueSendResult(currentCtx,
					j.ID,
					sdk.Result{
						ID:         j.ID,
						BuildID:    j.ID,
						Status:     sdk.StatusFail,
						RemoteTime: time.Now(),
						Reason:     fmt.Sprintf("hatchery %q failed to start worker after %d attempts", h.Configuration().Name, maxAttemptsNumberBeforeFailure),
					}); err != nil {
				log.ErrorWithStackTrace(currentCtx, err)
			}
			log.Info(currentCtx, "hatchery %q failed to start worker after %d attempts", h.Configuration().Name, maxAttemptsNumberBeforeFailure)
			endTrace("maximum attempts", strconv.FormatInt(j.ID, 10))
			return nil
		}
	}

	logStepInfo(currentCtx, "processed", j.Queued)
	stats.Record(currentCtx, GetMetrics().JobsProcessed.M(1))
	workersStartChan <- workerRequest

	return nil
}

func handleJobV2(ctx context.Context, h Interface, jobRunID string, cacheAttempts *CacheNbAttemptsJobIDs, workersStartChan chan<- workerStarterRequest) error {
	jobInfo, err := h.CDSClientV2().V2QueueGetJobRun(ctx, h.GetRegion(), jobRunID)
	if sdk.ErrorIs(err, sdk.ErrNotFound) {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "unable to get job %s info", jobRunID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	ctx = telemetry.ContextWithTag(ctx,
		telemetry.TagServiceName, h.Name(),
		telemetry.TagServiceType, h.Type(),
	)
	ctx = telemetry.New(ctx, h, "hatchery.V2JobReceive", trace.AlwaysSample(), trace.SpanKindServer)
	ctx, end := telemetry.Span(ctx, "hatchery.V2JobReceive", telemetry.Tag(telemetry.TagWorkflow, jobInfo.RunJob.WorkflowName),
		telemetry.Tag(telemetry.TagWorkflowRunNumber, jobInfo.RunJob.RunNumber),
		telemetry.Tag(telemetry.TagProjectKey, jobInfo.RunJob.ProjectKey),
		telemetry.Tag(telemetry.TagJob, jobInfo.RunJob.ID))

	endTrace := func(reason string, runJobID string) {
		if runJobID != "" {
			h.GetMapPendingWorkerCreation().RemoveJobFromPendingWorkerCreation(runJobID)
		}
		if cancel != nil {
			cancel()
		}
		if reason != "" {
			telemetry.Current(ctx,
				telemetry.Tag("reason", reason),
			)
		}
		if end != nil {
			end()
		}
		telemetry.End(ctx, nil, nil)
	}
	go func() {
		<-ctx.Done()
		endTrace(ctx.Err().Error(), jobInfo.RunJob.ID)
	}()

	if jobInfo.RunJob.Status != sdk.StatusWaiting {
		log.Debug(ctx, "hatchery> job %q is not waiting: %s", jobRunID, jobInfo.RunJob.Status)
		endTrace("job is not waiting", jobRunID)
		return nil
	}

	fields := log.FieldValues(ctx)
	for k, v := range fields {
		ctx = context.WithValue(ctx, k, v)
	}
	ctx = context.WithValue(ctx, LogFieldJobID, jobInfo.RunJob.ID)
	ctx = context.WithValue(ctx, LogFieldProject, jobInfo.RunJob.ProjectKey)
	logStepInfo(ctx, "dequeue", jobInfo.RunJob.Queued)

	stats.Record(ctx, GetMetrics().Jobs.M(1))

	//Check if hatchery is able to start a new worker
	if !checkCapacities(ctx, h) {
		log.Info(ctx, "hatchery %s is not able to provision new worker", h.Service().Name)
		endTrace("no capacities", jobInfo.RunJob.ID)
	}

	workerRequest := workerStarterRequest{
		ctx:          ctx,
		cancel:       cancel,
		id:           jobInfo.RunJob.ID,
		region:       jobInfo.RunJob.Region,
		requirements: nil,
		queued:       jobInfo.RunJob.Queued,
	}

	// Check at least one worker model can match
	hWithModels, hatcheryIsWithModels := h.(InterfaceWithModels)
	if hatcheryIsWithModels && jobInfo.RunJob.Job.RunsOn.Model == "" {
		endTrace("no model", jobInfo.RunJob.ID)
		return nil
	}

	// Check if we already try to start a worker for this job
	maxAttemptsNumberBeforeFailure := h.Configuration().Provision.MaxAttemptsNumberBeforeFailure
	if maxAttemptsNumberBeforeFailure > -1 {
		nbAttempts := cacheAttempts.GetAttempt(jobInfo.RunJob.ID)
		if maxAttemptsNumberBeforeFailure == 0 {
			maxAttemptsNumberBeforeFailure = defaultMaxAttemptsNumberBeforeFailure
		}
		if nbAttempts > maxAttemptsNumberBeforeFailure {
			log.Error(ctx, "hatchery %q failed to start worker after %d attempts", h.Configuration().Name, maxAttemptsNumberBeforeFailure)
			endTrace("maximum attempts", jobInfo.RunJob.ID)
			return nil
		}
	}

	if hWithModels != nil {
		workerModel, err := getWorkerModelV2(ctx, hWithModels, *jobInfo)
		if err != nil {
			cacheAttempts.NewAttempt(jobInfo.RunJob.ID)
			log.Error(ctx, "hatchery %q failed to get worker model %s: %v", h.Configuration().Name, jobInfo.Model.Name, err)
			endTrace(fmt.Sprintf("%v", err.Error()), jobInfo.RunJob.ID)
			return err
		}
		workerRequest.model = *workerModel
		if can := h.CanSpawn(ctx, *workerModel, jobInfo.RunJob.ID, nil); !can {
			log.Warn(ctx, "cannot spawn worker")
			endTrace("cannot spawn", jobInfo.RunJob.ID)
			return nil
		}
		can, err := hWithModels.CanAllocateResources(ctx, *workerModel, jobInfo.RunJob.ID, nil)
		if err != nil {
			log.Error(ctx, "hatchery %q failed to check canAllocateResources: %v", h.Configuration().Name, err)
			endTrace(fmt.Sprintf("%v", err.Error()), jobInfo.RunJob.ID)
			return err
		}
		if !can {
			log.Warn(ctx, "cannot allocate resource for worker")
			endTrace("cannot allocate resource", jobInfo.RunJob.ID)
			return nil
		}
	}

	cacheAttempts.NewAttempt(jobInfo.RunJob.ID)

	logStepInfo(ctx, "processed", jobInfo.RunJob.Queued)
	stats.Record(ctx, GetMetrics().JobsProcessed.M(1))
	workersStartChan <- workerRequest
	return nil
}

func canRunJob(ctx context.Context, h Interface, j workerStarterRequest) bool {
	for _, r := range j.requirements {
		// If requirement is an hostname requirement, it's for a specific worker
		if r.Type == sdk.HostnameRequirement && r.Value != j.hostname {
			log.Debug(ctx, "hostname requirement r.Value(%s) != hostname(%s)", r.Value, j.hostname)
			return false
		}

		if r.Type == sdk.RegionRequirement && r.Value != h.Configuration().Provision.Region {
			log.Debug(ctx, "job with region requirement: cannot spawn. hatchery-region: %s prerequisite: %s", h.Configuration().Provision.Region, r.Value)
			return false
		}

		// Skip others requirement as we can't check it
		if r.Type == sdk.PluginRequirement || r.Type == sdk.ServiceRequirement || r.Type == sdk.MemoryRequirement || r.Type == sdk.FlavorRequirement {
			log.Debug(ctx, "job with service, plugin, memory or flavor requirement. Skip these check as we can't check it on hatchery routine")
			continue
		}
	}
	return h.CanSpawn(ctx, j.model, j.id, j.requirements)
}

// MemoryRegisterContainer is the RAM used for spawning
// a docker container for register a worker model. 128 Mo
const MemoryRegisterContainer int64 = 128

func canRunJobWithModelV2(ctx context.Context, h InterfaceWithModels, j workerStarterRequest, workerModelV2 string) (*sdk.Model, *sdk.WorkerStarterWorkerModel, error) {
	ctx, end := telemetry.Span(ctx, "hatchery.canRunJobWithModelV2", telemetry.Tag(telemetry.TagWorker, workerModelV2))
	defer end()

	branchSplit := strings.Split(workerModelV2, "@")

	modelPath := strings.Split(branchSplit[0], "/")
	if len(modelPath) < 4 {
		return nil, nil, sdk.WrapError(sdk.ErrInvalidData, "wrong model value %v", modelPath)
	}
	projKey := modelPath[0]
	vcsName := modelPath[1]
	modelName := modelPath[len(modelPath)-1]
	repoName := strings.Join(modelPath[2:len(modelPath)-1], "/")
	branch := "master"
	if len(branchSplit) == 2 {
		branch = branchSplit[1]
	}

	model, err := h.CDSClientV2().GetWorkerModel(ctx, projKey, vcsName, repoName, modelName, cdsclient.WithQueryParameter("branch", branch))
	if err != nil {
		return nil, nil, err
	}
	if model.Type != h.ModelType() {
		return nil, nil, nil
	}

	oldModel := sdk.Model{
		ID:          0,
		Type:        model.Type,
		Name:        modelName + "/" + branch,
		Description: model.Description,
		// Fake group for naming
		Group: &sdk.Group{
			Name: projKey + "/" + vcsName + "/" + repoName,
		},
	}

	preCmd := `#!/bin/sh
if [ ! -z ` + "`which curl`" + ` ]; then
	curl -L "{{.API}}/download/worker/linux/$(uname -m)" -o /usr/local/bin/worker --retry 10 --retry-max-time 120 >> /tmp/cds-worker-setup.log 2>&1 && chmod +x /usr/local/bin/worker
elif [ ! -z ` + "`which wget`" + ` ]; then
	wget "{{.API}}/download/worker/linux/$(uname -m)" -O /usr/local/bin/worker >> /tmp/cds-worker-setup.log 2>&1 && chmod +x /usr/local/bin/worker
else
	echo "Missing requirements to download CDS worker binary.";
	exit 1;
fi`

	ap := sdk.NewActionParser(map[string]interface{}{
		"git": map[string]interface{}{
			"ref_name": branch,
		},
	}, nil)

	switch model.Type {
	case sdk.WorkerModelTypeDocker:
		var dockerSpec sdk.V2WorkerModelDockerSpec
		if err := yaml.Unmarshal(model.Spec, &dockerSpec); err != nil {
			return nil, nil, sdk.WithStack(err)
		}
		oldModel.ModelDocker = sdk.ModelDocker{
			Image:    dockerSpec.Image,
			Username: dockerSpec.Username,
			Password: dockerSpec.Password,
			Envs:     dockerSpec.Envs,
		}
		if model.OSArch == "windows/amd64" {
			oldModel.ModelDocker.Cmd = "curl {{.API}}/download/worker/windows/amd64 -o worker.exe && worker.exe"
			oldModel.ModelDocker.Shell = "cmd.exe /C"
		} else {
			oldModel.ModelDocker.Cmd = "curl {{.API}}/download/worker/linux/$(uname -m) -o worker --retry 10 --retry-max-time 120 && chmod +x worker && exec ./worker"
			oldModel.ModelDocker.Shell = "sh -c"
		}
		oldModel.ModelDocker.Image, err = ap.InterpolateToString(ctx, oldModel.ModelDocker.Image)
		if err != nil {
			return nil, nil, sdk.WithStack(err)
		}
	case sdk.WorkerModelTypeVSphere:
		var vsphereSpec sdk.V2WorkerModelVSphereSpec
		if err := yaml.Unmarshal(model.Spec, &vsphereSpec); err != nil {
			return nil, nil, sdk.WithStack(err)
		}

		workerStarterModelWithModelv2 := sdk.WorkerStarterWorkerModel{
			Cmd:         "PATH=$PATH worker",
			PreCmd:      preCmd,
			PostCmd:     "sudo shutdown -h now",
			ModelV2:     model,
			VSphereSpec: vsphereSpec,
		}
		if can := h.CanSpawn(ctx, workerStarterModelWithModelv2, j.id, j.requirements); !can {
			return nil, nil, nil
		}
		return nil, &workerStarterModelWithModelv2, nil

	case sdk.WorkerModelTypeOpenstack:
		var openstackSpec sdk.V2WorkerModelOpenstackSpec
		if err := yaml.Unmarshal(model.Spec, &openstackSpec); err != nil {
			return nil, nil, sdk.WithStack(err)
		}
		oldModel.ModelVirtualMachine = sdk.ModelVirtualMachine{
			Cmd:     "worker",
			PreCmd:  preCmd,
			PostCmd: "sudo shutdown -h now",
			Image:   openstackSpec.Image,
		}
		for _, r := range j.requirements {
			if r.Type == sdk.FlavorRequirement {
				oldModel.ModelVirtualMachine.Flavor = r.Value
				break
			}
		}
		oldModel.ModelVirtualMachine.Image, err = ap.InterpolateToString(ctx, oldModel.ModelVirtualMachine.Image)
		if err != nil {
			return nil, nil, sdk.WithStack(err)
		}
	}

	if can := h.CanSpawn(ctx, sdk.WorkerStarterWorkerModel{ModelV1: &oldModel}, j.id, j.requirements); !can {
		return nil, nil, nil
	}
	return &oldModel, nil, nil
}

func getWorkerModelV2(ctx context.Context, h InterfaceWithModels, jobInf sdk.V2QueueJobInfo) (*sdk.WorkerStarterWorkerModel, error) {
	_, end := telemetry.Span(ctx, "hatchery.getWorkerModelV2", telemetry.Tag(telemetry.TagWorker, jobInf.RunJob.Job.RunsOn))
	defer end()

	if jobInf.Model.Type != h.ModelType() {
		return nil, nil
	}

	workerStarterModel := &sdk.WorkerStarterWorkerModel{ModelV2: &jobInf.Model}

	// No check need on jobInf.Model.OSArch as the hatchery can only take osarch that match herself

	preCmd := fmt.Sprintf(`#!/bin/sh
    if [ ! -z `+"`which curl`"+` ]; then
      curl -L "{{.API}}/download/worker/%s" -o /usr/local/bin/worker --retry 10 --retry-max-time 120 >> /tmp/cds-worker-setup.log 2>&1 && chmod +x /usr/local/bin/worker
    elif [ ! -z `+"`which wget`"+` ]; then
      wget "{{.API}}/download/worker/%s" -O /usr/local/bin/worker >> /tmp/cds-worker-setup.log 2>&1 && chmod +x /usr/local/bin/worker
    else
      echo "Missing requirements to download CDS worker binary.";
      exit 1;
    fi
  `, jobInf.Model.OSArch, jobInf.Model.OSArch)

	switch jobInf.Model.Type {
	case sdk.WorkerModelTypeDocker:
		if jobInf.Model.OSArch == "windows/amd64" {
			workerStarterModel.Cmd = "curl {{.API}}/download/worker/windows/amd64 -o worker.exe && worker.exe"
			workerStarterModel.Shell = "cmd.exe /C"
		} else {
			workerStarterModel.Cmd = "curl {{.API}}/download/worker/" + jobInf.Model.OSArch + " -o worker --retry 10 --retry-max-time 120 && chmod +x worker && exec ./worker"
			workerStarterModel.Shell = "sh -c"
		}
		var dockerSpec sdk.V2WorkerModelDockerSpec
		if err := json.Unmarshal(jobInf.Model.Spec, &dockerSpec); err != nil {
			return nil, sdk.WrapError(err, "unable to get docker spec")
		}
		workerStarterModel.DockerSpec = dockerSpec
	case sdk.WorkerModelTypeVSphere:
		workerStarterModel.Cmd = "PATH=$PATH worker"
		workerStarterModel.PreCmd = preCmd
		workerStarterModel.PostCmd = "sudo shutdown -h now"
		var vsphereSpec sdk.V2WorkerModelVSphereSpec
		if err := json.Unmarshal(jobInf.Model.Spec, &vsphereSpec); err != nil {
			return nil, sdk.WrapError(err, "unable to get vsphere spec")
		}
		workerStarterModel.VSphereSpec = vsphereSpec
	case sdk.WorkerModelTypeOpenstack:
		workerStarterModel.Cmd = "worker"
		workerStarterModel.PreCmd = preCmd
		workerStarterModel.PostCmd = "sudo shutdown -h now"
		var openstackSpec sdk.V2WorkerModelOpenstackSpec
		if err := json.Unmarshal(jobInf.Model.Spec, &openstackSpec); err != nil {
			return nil, sdk.WrapError(err, "unable to get openstack spec")
		}
		workerStarterModel.OpenstackSpec = openstackSpec
	}

	var mem int64
	if jobInf.RunJob.Job.RunsOn.Memory != "" {
		var err error
		mem, err = strconv.ParseInt(jobInf.RunJob.Job.RunsOn.Memory, 10, 64)
		if err != nil {
			return nil, sdk.NewErrorFrom(sdk.ErrInvalidData, "%s is not an integer", jobInf.RunJob.Job.RunsOn.Memory)
		}
	}

	workerStarterModel.Memory = mem
	workerStarterModel.Flavor = jobInf.RunJob.Job.RunsOn.Flavor
	return workerStarterModel, nil
}

// only used by vshpere hatchery
func checkDefaultModelV2(ctx context.Context, h InterfaceWithModels, workerRequest workerStarterRequest, modelInPrerequisite string) (*sdk.WorkerStarterWorkerModel, error) {
	if h.ModelType() != sdk.VSphere {
		return nil, nil
	}
	if modelInPrerequisite != "" {
		return nil, nil
	}

	hWithDefault, hatcheryIsWithDefault := h.(InterfaceWithDetaultWorkerModelV2)
	if !hatcheryIsWithDefault {
		return nil, nil
	}
	workerModelV2 := hWithDefault.GetDetaultModelV2Name(ctx, workerRequest.requirements)

	if workerModelV2 == "" {
		return nil, nil
	}

	_, workerStarterModelWithModelv2, err := canRunJobWithModelV2(ctx, h, workerRequest, workerModelV2)
	return workerStarterModelWithModelv2, err
}

func canRunJobWithModel(ctx context.Context, h InterfaceWithModels, j workerStarterRequest, model *sdk.Model) bool {
	if model.Type != h.ModelType() {
		log.Debug(ctx, "model %s type:%s current hatchery modelType: %s", model.Name, model.Type, h.ModelType())
		return false
	}

	// If the model needs registration, don't spawn for now
	if h.NeedRegistration(ctx, model) {
		log.Debug(ctx, "model %s needs registration", model.Name)
		return false
	}

	if model.NbSpawnErr > 5 {
		log.Warn(ctx, "too many errors on spawn with model %s, please check this worker model", model.Name)
		return false
	}

	if len(j.execGroups) > 0 {
		checkGroup := false
		for _, g := range j.execGroups {
			if g.ID == model.GroupID {
				checkGroup = true
				break
			}
		}
		if !checkGroup {
			log.Debug(ctx, "model %s attached to group %d can't run this job", model.Name, model.GroupID)
			return false
		}
	}

	var containsModelRequirement, containsHostnameRequirement bool
	for _, r := range j.requirements {
		switch r.Type {
		case sdk.ModelRequirement:
			containsModelRequirement = true
		case sdk.HostnameRequirement:
			containsHostnameRequirement = true
		}
	}

	if model.IsDeprecated && !containsModelRequirement {
		log.Debug(ctx, "cannot launch this model because it is deprecated")
		return false
	}

	// Common check
	for _, r := range j.requirements {
		// If requirement is a Model requirement, it's easy. It's either can or can't run
		// r.Value could be: theModelName --port=8888:9999, so we take strings.Split(r.Value, " ")[0] to compare
		// only modelName
		if r.Type == sdk.ModelRequirement {
			modelName := strings.Split(r.Value, " ")[0]
			isGroupModel := modelName == fmt.Sprintf("%s/%s", model.Group.Name, model.Name)
			isSharedInfraModel := model.Group.Name == sdk.SharedInfraGroupName && modelName == model.Name
			isSameName := modelName == model.Name // for backward compatibility with runs, if only the name match we considered that the model can be used, keep this condition until the workflow runs were not migrated.
			if !isGroupModel && !isSharedInfraModel && !isSameName {
				log.Debug(ctx, "model requirement r.Value(%s) do not match model.Name(%s) and model.Group(%s)", strings.Split(r.Value, " ")[0], model.Name, model.Group.Name)
				return false
			}
		}

		// service and memory requirements are only supported by docker model
		if model.Type != sdk.Docker && (r.Type == sdk.ServiceRequirement || r.Type == sdk.MemoryRequirement) {
			log.Debug(ctx, "job with service requirement or memory requirement: only for model docker. current model: %s", model.Type)
			return false
		}

		// flavor requirement is only supported by openstack model
		if model.Type != sdk.Openstack && r.Type == sdk.FlavorRequirement {
			log.Debug(ctx, "job with flavor requirement: only for model openstack. current model: %s", model.Type)
			return false
		}

		// Skip other requirement as we can't check it
		if r.Type == sdk.PluginRequirement || r.Type == sdk.ServiceRequirement || r.Type == sdk.MemoryRequirement || r.Type == sdk.FlavorRequirement {
			log.Debug(ctx, "job with service, plugin, memory or flavor requirement. Skip these check as we can't check it on hatchery routine")
			continue
		}

		if r.Type == sdk.OSArchRequirement && model.RegisteredOS != nil && *model.RegisteredOS != "" && model.RegisteredArch != nil && *model.RegisteredArch != "" && r.Value != (*model.RegisteredOS+"/"+*model.RegisteredArch) {
			log.Debug(ctx, "job with OSArch requirement: cannot spawn on this OSArch. current model: %s/%s", *model.RegisteredOS, *model.RegisteredArch)
			return false
		}

		if r.Type == sdk.RegionRequirement && r.Value != h.Configuration().Provision.Region {
			log.Debug(ctx, "job with region requirement: cannot spawn. hatchery-region: %s prerequisite: %s", h.Configuration().Provision.Region, r.Value)
			return false
		}

		if !containsModelRequirement && !containsHostnameRequirement {
			if r.Type == sdk.BinaryRequirement {
				found := false
				// Check binary requirement against worker model capabilities
				for _, c := range model.RegisteredCapabilities {
					if r.Value == c.Value || r.Value == c.Name {
						found = true
						break
					}
				}

				if !found {
					log.Debug(ctx, "model(%s) does not have binary %s(%s) for this job.", model.Name, r.Name, r.Value)
					return false
				}
			}
		}
	}

	return h.CanSpawn(ctx, sdk.WorkerStarterWorkerModel{ModelV1: model}, j.id, j.requirements)
}

// SendSpawnInfo sends a spawnInfo
func SendSpawnInfo(ctx context.Context, h Interface, jobID string, spawnMsg sdk.SpawnMsg) {
	if h.CDSClient() == nil || sdk.IsJobIDForRegister(jobID) {
		return
	}
	infos := []sdk.SpawnInfo{{RemoteTime: time.Now(), Message: spawnMsg}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.CDSClient().QueueJobSendSpawnInfo(ctx, jobID, infos); err != nil {
		log.Warn(ctx, "SendSpawnInfo> cannot client.sendSpawnInfo for job %d: %s", jobID, err)
	}
}

func logStepInfo(ctx context.Context, step string, queued time.Time) {
	if id := ctx.Value(LogFieldJobID); id != nil {
		ctx = context.WithValue(ctx, LogFieldStep, step)
		ctx = context.WithValue(ctx, LogFieldStepDelay, time.Since(queued).Nanoseconds())
		log.Info(ctx, "step: %s job: %s", step, id)
	}
}

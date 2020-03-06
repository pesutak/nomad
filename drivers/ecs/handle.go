package ecs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/fifo"
	"github.com/hashicorp/nomad/client/stats"
	"github.com/hashicorp/nomad/plugins/drivers"
)

type taskHandle struct {
	arn       string
	logger    hclog.Logger
	ecsClient ecsClientInterface

	totalCpuStats  *stats.CpuStats
	userCpuStats   *stats.CpuStats
	systemCpuStats *stats.CpuStats

	// stateLock syncs access to all fields below
	stateLock sync.RWMutex

	taskConfig  *drivers.TaskConfig
	procState   drivers.TaskState
	startedAt   time.Time
	completedAt time.Time
	exitResult  *drivers.ExitResult
	doneCh      chan struct{}
	detach      bool // detach if true; kill if false

	ctx    context.Context
	cancel context.CancelFunc
}

func newTaskHandle(logger hclog.Logger, ts TaskState, taskConfig *drivers.TaskConfig, ecsClient ecsClientInterface) *taskHandle {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named("handle").With("arn", ts.ARN)

	h := &taskHandle{
		arn:       ts.ARN,
		ecsClient: ecsClient,
		//FIXME(schmichael) this originally used a TaskConfig persisted
		//in the TaskState which broke logging as it pointed to the old
		//logmon path. can we remove the TaskConfig from the driver
		//state or does that break agent restart?!
		taskConfig: taskConfig,
		procState:  drivers.TaskStateRunning,
		startedAt:  ts.StartedAt,
		exitResult: &drivers.ExitResult{},
		logger:     logger,
		doneCh:     make(chan struct{}),
		detach:     false,
		ctx:        ctx,
		cancel:     cancel,
	}

	return h
}

func (h *taskHandle) TaskStatus() *drivers.TaskStatus {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()

	return &drivers.TaskStatus{
		ID:          h.taskConfig.ID,
		Name:        h.taskConfig.Name,
		State:       h.procState,
		StartedAt:   h.startedAt,
		CompletedAt: h.completedAt,
		ExitResult:  h.exitResult,
		DriverAttributes: map[string]string{
			"arn": h.arn,
		},
	}
}

func (h *taskHandle) IsRunning() bool {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()
	return h.procState == drivers.TaskStateRunning
}

func (h *taskHandle) run() {
	defer close(h.doneCh)
	h.stateLock.Lock()
	if h.exitResult == nil {
		h.exitResult = &drivers.ExitResult{}
	}
	h.stateLock.Unlock()

	h.logger.Info("-----> OpenWriter()", "stdout_path", h.taskConfig.StdoutPath)
	f, err := fifo.OpenWriter(h.taskConfig.StdoutPath)
	if err != nil {
		h.logger.Info("-----> OpenWriter() ERROR 1", "error", err, "stdout_path", h.taskConfig.StdoutPath)
		h.stateLock.Lock()
		defer h.stateLock.Unlock()
		h.completedAt = time.Now()
		h.exitResult.ExitCode = 1
		h.exitResult.Err = fmt.Errorf("failed to create stdout: %v", err)
		return
	}
	defer f.Close()

	// Block until stopped.
	for h.ctx.Err() == nil {
		select {
		case <-time.After(5 * time.Second):

			status, err := h.ecsClient.DescribeTaskStatus(h.ctx, h.arn)
			if err != nil {
				h.handleRunError(err, "failed to find ECS task")
				return
			}

			// Write the health status before checking what it is ensures the
			// alloc logs include the health during the ECS tasks terminal
			// phase.
			now := time.Now().Format(time.RFC3339)
			if _, err := fmt.Fprintf(f, "[%s] - client is remotely monitoring ECS task: %v with status %v\n",
				now, h.arn, status); err != nil {
				h.handleRunError(err, "failed to write to stdout")
			}

			// ECS task has terminal status phase, meaning the task is going to
			// stop. If we are in this phase, the driver should exit and pass
			// this to the servers so that a new allocation, and ECS task can
			// be started.
			if status == "DEACTIVATING" || status == "STOPPING" || status == "DEPROVISIONING" || status == "STOPPED" {
				h.handleRunError(fmt.Errorf("ECS task status in terminal phase"), "task status: "+status)
				return
			}

		case <-h.ctx.Done():
		}
	}

	h.logger.Info("-----> handle.run DONE", "ctx_error", h.ctx.Err(), "stdout_path", h.taskConfig.StdoutPath)

	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	// Only stop task if we're not detaching
	if !h.detach {
		// Do not pass h.ctx, it is cancelled at this point
		if err := h.ecsClient.StopTask(context.TODO(), h.arn, "terminated by Nomad"); err != nil {
			h.handleRunError(err, "error stopping ECS task")
			return
		}
	}

	h.procState = drivers.TaskStateExited
	h.exitResult.ExitCode = 0
	h.exitResult.Signal = 0
	h.completedAt = time.Now()
}

func (h *taskHandle) stop(detach bool) {
	h.logger.Info("handle.stop()")
	h.stateLock.Lock()
	defer h.stateLock.Unlock()
	// Only allow transitioning from not-detaching to detaching
	if !h.detach && detach {
		h.detach = detach
	}
	h.cancel()
}

func (h *taskHandle) handleRunError(err error, context string) {
	h.stateLock.Lock()
	h.completedAt = time.Now()
	h.exitResult.ExitCode = 2
	h.exitResult.Err = fmt.Errorf("%s: %v", context, err)
	h.stateLock.Unlock()
}
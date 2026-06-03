package scheduler

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/qmuntal/gobuilder/internal/githubactions"
)

type fakeJobQueue struct {
	jobs int
}

func (queue fakeJobQueue) JobsInQueue(_ context.Context) (int, error) {
	return queue.jobs, nil
}

type recordingDispatcher struct {
	mutex    sync.Mutex
	requests []githubactions.DispatchRequest
}

func (dispatcher *recordingDispatcher) DispatchWorkflow(_ context.Context, dispatchRequest githubactions.DispatchRequest) error {
	dispatcher.mutex.Lock()
	defer dispatcher.mutex.Unlock()

	dispatcher.requests = append(dispatcher.requests, dispatchRequest)
	return nil
}

func (dispatcher *recordingDispatcher) recordedRequests() []githubactions.DispatchRequest {
	dispatcher.mutex.Lock()
	defer dispatcher.mutex.Unlock()

	requests := make([]githubactions.DispatchRequest, len(dispatcher.requests))
	copy(requests, dispatcher.requests)
	return requests
}

type blockingDispatcher struct {
	started chan string
	release <-chan struct{}
}

func (dispatcher blockingDispatcher) DispatchWorkflow(ctx context.Context, dispatchRequest githubactions.DispatchRequest) error {
	dispatcher.started <- dispatchRequest.Inputs["job_index"]

	select {
	case <-dispatcher.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRunCapsDispatchesAtMaximum(testingContext *testing.T) {
	output := new(bytes.Buffer)
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		MaxJobs:    2,
		Workflow:   "builder.yml",
		Output:     output,
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.QueueDepth != 5 {
		testingContext.Fatalf("QueueDepth = %d, want 5", result.QueueDepth)
	}
	if result.Dispatched != 2 {
		testingContext.Fatalf("Dispatched = %d, want 2", result.Dispatched)
	}
	requests := dispatcher.recordedRequests()
	if len(requests) != 2 {
		testingContext.Fatalf("dispatcher requests = %d, want 2", len(requests))
	}
	sort.Slice(requests, func(leftIndex, rightIndex int) bool {
		return requests[leftIndex].Inputs["job_index"] < requests[rightIndex].Inputs["job_index"]
	})
	if requests[1].Inputs["job_index"] != "2" {
		testingContext.Fatalf("second job_index = %q, want 2", requests[1].Inputs["job_index"])
	}
}

func TestRunDryRunDoesNotRequireDispatcher(testingContext *testing.T) {
	result, err := Run(context.Background(), Config{
		Queue:   fakeJobQueue{jobs: 2},
		MaxJobs: 5,
		DryRun:  true,
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 2 {
		testingContext.Fatalf("Dispatched = %d, want 2", result.Dispatched)
	}
}

func TestRunDispatchesInParallel(testingContext *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	errChannel := make(chan error, 1)

	go func() {
		_, err := Run(context.Background(), Config{
			Queue:      fakeJobQueue{jobs: 2},
			Dispatcher: blockingDispatcher{started: started, release: release},
			MaxJobs:    2,
			Workflow:   "builder.yml",
		})
		errChannel <- err
	}()

	startedJobs := map[string]bool{}
	for range 2 {
		select {
		case jobIndex := <-started:
			startedJobs[jobIndex] = true
		case <-time.After(2 * time.Second):
			close(release)
			testingContext.Fatal("timed out waiting for parallel dispatches to start")
		}
	}

	if !startedJobs["1"] || !startedJobs["2"] {
		close(release)
		testingContext.Fatalf("started jobs = %v, want jobs 1 and 2", startedJobs)
	}

	close(release)
	select {
	case err := <-errChannel:
		if err != nil {
			testingContext.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		testingContext.Fatal("timed out waiting for Run() to finish")
	}
}

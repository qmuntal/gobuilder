package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qmuntal/gobuilder/internal/githubactions"
)

type fakeJobQueue struct {
	jobs    int
	bots    map[string]time.Time
	busyErr error
	onBusy  func()
}

func (queue fakeJobQueue) JobsInQueue(_ context.Context) (int, error) {
	return queue.jobs, nil
}

func (queue fakeJobQueue) BusyBots(_ context.Context) (map[string]time.Time, error) {
	if queue.onBusy != nil {
		queue.onBusy()
	}
	return queue.bots, queue.busyErr
}

type recordingDispatcher struct {
	mutex    sync.Mutex
	requests []githubactions.DispatchRequest
}

type fakeRunCounter struct {
	activeRuns githubactions.ActiveWorkflowRuns
	err        error
}

func (counter fakeRunCounter) ActiveWorkflowRuns(_ context.Context, _ string) (githubactions.ActiveWorkflowRuns, error) {
	return counter.activeRuns, counter.err
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
	started chan struct{}
	release <-chan struct{}
}

func (dispatcher blockingDispatcher) DispatchWorkflow(ctx context.Context, dispatchRequest githubactions.DispatchRequest) error {
	dispatcher.started <- struct{}{}

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
		Workflow:   "builder-windows-arm64.yml",
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
	wantBotIndexes := map[string]bool{"01": true, "02": true}
	for requestIndex, request := range requests {
		if request.Workflow != "builder-windows-arm64.yml" {
			testingContext.Fatalf("request %d workflow = %q, want builder-windows-arm64.yml", requestIndex, request.Workflow)
		}
		botIndex := request.Inputs[botIndexInput]
		if !wantBotIndexes[botIndex] {
			testingContext.Fatalf("request %d bot_index = %q, want one of %v", requestIndex, botIndex, wantBotIndexes)
		}
		delete(wantBotIndexes, botIndex)
	}
}

func TestRunDispatchesStableBotIndexes(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	_, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 3},
		Dispatcher: dispatcher,
		MaxJobs:    3,
		Workflow:   "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}

	requests := dispatcher.recordedRequests()
	if len(requests) != 3 {
		testingContext.Fatalf("dispatcher requests = %d, want 3", len(requests))
	}
	wantBotIndexes := map[string]bool{"01": true, "02": true, "03": true}
	for requestIndex, request := range requests {
		botIndex := request.Inputs[botIndexInput]
		if !wantBotIndexes[botIndex] {
			testingContext.Fatalf("request %d bot_index = %q, want one of %v", requestIndex, botIndex, wantBotIndexes)
		}
		delete(wantBotIndexes, botIndex)
	}
}

func TestRunOffsetsBotIndexesByActiveRuns(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
			Count:      2,
			BotIndexes: map[int]int{1: 1, 2: 1},
		}},
		MaxJobs:  5,
		Workflow: "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 3 {
		testingContext.Fatalf("Dispatched = %d, want 3", result.Dispatched)
	}

	requests := dispatcher.recordedRequests()
	wantBotIndexes := map[string]bool{"03": true, "04": true, "05": true}
	for requestIndex, request := range requests {
		botIndex := request.Inputs[botIndexInput]
		if !wantBotIndexes[botIndex] {
			testingContext.Fatalf("request %d bot_index = %q, want one of %v", requestIndex, botIndex, wantBotIndexes)
		}
		delete(wantBotIndexes, botIndex)
	}
}

func TestRunUsesFirstFreeBotIndexes(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
			Count:      2,
			BotIndexes: map[int]int{2: 1, 4: 1},
		}},
		MaxJobs:  5,
		Workflow: "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 3 {
		testingContext.Fatalf("Dispatched = %d, want 3", result.Dispatched)
	}

	requests := dispatcher.recordedRequests()
	wantBotIndexes := map[string]bool{"01": true, "03": true, "05": true}
	for requestIndex, request := range requests {
		botIndex := request.Inputs[botIndexInput]
		if !wantBotIndexes[botIndex] {
			testingContext.Fatalf("request %d bot_index = %q, want one of %v", requestIndex, botIndex, wantBotIndexes)
		}
		delete(wantBotIndexes, botIndex)
	}
}

func TestRunDoesNotDispatchWhenActiveRunsFillSlots(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
			Count:      5,
			BotIndexes: map[int]int{1: 1, 2: 1, 3: 1, 4: 1, 5: 1},
		}},
		MaxJobs:  5,
		Workflow: "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 0 {
		testingContext.Fatalf("Dispatched = %d, want 0", result.Dispatched)
	}
	if requests := dispatcher.recordedRequests(); len(requests) != 0 {
		testingContext.Fatalf("dispatcher requests = %d, want 0", len(requests))
	}
}

func TestRunDoesNotDispatchWithUnknownActiveRunSlot(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
			Count:        1,
			UnknownCount: 1,
			BotIndexes:   map[int]int{},
		}},
		MaxJobs:  5,
		Workflow: "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 0 {
		testingContext.Fatalf("Dispatched = %d, want 0", result.Dispatched)
	}
	if requests := dispatcher.recordedRequests(); len(requests) != 0 {
		testingContext.Fatalf("dispatcher requests = %d, want 0", len(requests))
	}
}

func TestRunSkipsCapacityLookupForEmptyQueue(testingContext *testing.T) {
	_, err := Run(context.Background(), Config{
		Queue: fakeJobQueue{}, MaxJobs: 5,
		RunCounter: fakeRunCounter{err: errors.New("unexpected GitHub lookup")},
	})
	if err != nil {
		testingContext.Fatal(err)
	}
}

func TestRunAccountsForIncomingBots(testingContext *testing.T) {
	start := time.Unix(1000, 0)
	inProgress := map[int]time.Time{1: start}
	for _, tc := range []struct {
		name       string
		inProgress map[int]time.Time
		bots       map[string]time.Time
		busyErr    error
		wantErr    bool
		wantSlot   string
	}{
		{name: "awaiting approval", busyErr: errors.New("unexpected busy lookup")},
		{name: "bootstrapping", inProgress: inProgress},
		{name: "serving a task", inProgress: inProgress, bots: map[string]time.Time{"fleet--01": start.Add(time.Minute)}, wantSlot: "02"},
		{name: "previous use of same slot", inProgress: inProgress, bots: map[string]time.Time{"fleet--01": start.Add(-time.Hour)}},
		{name: "different fleet", inProgress: inProgress, bots: map[string]time.Time{"other--01": start.Add(time.Minute)}},
		{name: "busy lookup failed", inProgress: inProgress, busyErr: errors.New("unavailable"), wantErr: true},
	} {
		testingContext.Run(tc.name, func(t *testing.T) {
			dispatcher := &recordingDispatcher{}
			_, err := Run(context.Background(), Config{
				Queue: fakeJobQueue{jobs: 1, bots: tc.bots, busyErr: tc.busyErr},
				RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
					Count: 1, BotIndexes: map[int]int{1: 1}, InProgressSince: tc.inProgress,
				}},
				Dispatcher: dispatcher, MaxJobs: 5, BotHostnamePrefix: "fleet",
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run() error = %v, want error %t", err, tc.wantErr)
			}
			requests := dispatcher.recordedRequests()
			if tc.wantSlot == "" && len(requests) == 0 {
				return
			}
			if len(requests) != 1 || requests[0].Inputs[botIndexInput] != tc.wantSlot {
				t.Fatalf("requests = %v, want slot %q", requests, tc.wantSlot)
			}
		})
	}
}

func TestRunDoesNotReplaceNewlyClaimedJob(t *testing.T) {
	start := time.Unix(1000, 0)
	queue := &fakeJobQueue{jobs: 1, bots: map[string]time.Time{"fleet--01": start.Add(time.Minute)}}
	queue.onBusy = func() { queue.jobs = 0 }
	result, err := Run(context.Background(), Config{
		Queue: queue, MaxJobs: 5, BotHostnamePrefix: "fleet",
		RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
			Count: 1, BotIndexes: map[int]int{1: 1}, InProgressSince: map[int]time.Time{1: start},
		}},
	})
	if err != nil || result.QueueDepth != 0 || result.Dispatched != 0 {
		t.Fatalf("Run() = %+v, %v; want no dispatch after the only job was claimed", result, err)
	}
}

func TestRunAccountsForGitHubWorkflowRuns(testingContext *testing.T) {
	for _, testCase := range []struct {
		name            string
		workflow        string
		displayTitle    string
		status          string
		transitionQuery string
		nextStatus      string
		deepHistory     bool
		maxJobs         int
		wantBotIndex    string
	}{
		{
			name:         "old approval behind 10001 completed runs",
			workflow:     "builder-windows-arm64.yml",
			displayTitle: "builder-windows-arm64 bot 01",
			status:       "waiting",
			deepHistory:  true,
			maxJobs:      2,
			wantBotIndex: "02",
		},
		{
			name:         "workflow ID with occupied capacity",
			workflow:     "12345",
			displayTitle: "builder-windows-arm64 bot 01",
			status:       "in_progress",
			maxJobs:      1,
		},
		{
			name:         "unparseable title blocks allocation",
			workflow:     "builder-windows-arm64.yml",
			displayTitle: "legacy builder run",
			status:       "in_progress",
			maxJobs:      2,
		},
		{
			name:            "status transition leaves free capacity",
			workflow:        "builder-windows-arm64.yml",
			displayTitle:    "builder-windows-arm64 bot 01",
			status:          "queued",
			transitionQuery: "queued",
			maxJobs:         2,
			wantBotIndex:    "02",
		},
		{
			name:            "waiting run resumes between status queries",
			workflow:        "builder-windows-arm64.yml",
			displayTitle:    "builder-windows-arm64 bot 01",
			status:          "waiting",
			transitionQuery: "in_progress",
			maxJobs:         1,
		},
		{
			name:            "pending run resumes between status queries",
			workflow:        "builder-windows-arm64.yml",
			displayTitle:    "builder-windows-arm64 bot 01",
			status:          "pending",
			transitionQuery: "in_progress",
			maxJobs:         1,
		},
		{
			name:            "run enters a previously queried state",
			workflow:        "builder-windows-arm64.yml",
			displayTitle:    "builder-windows-arm64 bot 01",
			status:          "queued",
			transitionQuery: "pending",
			nextStatus:      "waiting",
			maxJobs:         1,
		},
		{
			name:         "completed run does not occupy a slot",
			workflow:     "builder-windows-arm64.yml",
			displayTitle: "builder-windows-arm64 bot 01",
			status:       "completed",
			maxJobs:      1,
			wantBotIndex: "01",
		},
	} {
		testingContext.Run(testCase.name, func(testingContext *testing.T) {
			var transitioned atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				statusAtSnapshot := testCase.status
				if transitioned.Load() {
					statusAtSnapshot = testCase.nextStatus
					if statusAtSnapshot == "" {
						statusAtSnapshot = "in_progress"
					}
				}
				queriedStatus := request.URL.Query().Get("status")
				if testCase.deepHistory {
					if queriedStatus == "" {
						_, _ = responseWriter.Write([]byte(`{"total_count":10002,"workflow_runs":[{"id":10002,"status":"completed"}]}`))
						return
					}
					if request.URL.Query().Has("created") {
						_, _ = responseWriter.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
						return
					}
				}
				if testCase.transitionQuery != "" && (queriedStatus == "" || queriedStatus == testCase.transitionQuery) {
					transitioned.Store(true)
				}
				responseWriter.Header().Set("Content-Type", "application/json")
				if queriedStatus == "" || queriedStatus == statusAtSnapshot {
					_ = json.NewEncoder(responseWriter).Encode(map[string]any{
						"total_count": 1,
						"workflow_runs": []map[string]any{
							{"id": 1, "display_title": testCase.displayTitle, "status": statusAtSnapshot},
						},
					})
					return
				}
				_, _ = responseWriter.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
			}))
			defer server.Close()

			dispatcher := &recordingDispatcher{}
			result, err := Run(context.Background(), Config{
				Queue:      fakeJobQueue{jobs: 5},
				Dispatcher: dispatcher,
				RunCounter: githubactions.Dispatcher{
					Token:      "token",
					APIURL:     server.URL,
					Repository: "owner/repo",
					HTTPClient: server.Client(),
				},
				MaxJobs:  testCase.maxJobs,
				Workflow: testCase.workflow,
			})
			if err != nil {
				testingContext.Fatalf("Run() error = %v", err)
			}
			wantDispatches := 0
			if testCase.wantBotIndex != "" {
				wantDispatches = 1
			}
			requests := dispatcher.recordedRequests()
			if result.Dispatched != wantDispatches || len(requests) != wantDispatches {
				testingContext.Fatalf("dispatched = %d, requests = %d, want %d", result.Dispatched, len(requests), wantDispatches)
			}
			if wantDispatches != 0 && requests[0].Inputs[botIndexInput] != testCase.wantBotIndex {
				testingContext.Fatalf("bot index = %q, want %q", requests[0].Inputs[botIndexInput], testCase.wantBotIndex)
			}
		})
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

func TestRunRejectsMaxJobsOutsideBotIndexRange(testingContext *testing.T) {
	_, err := Run(context.Background(), Config{
		Queue:   fakeJobQueue{jobs: 1},
		MaxJobs: 100,
	})
	if err == nil {
		testingContext.Fatal("Run() error = nil, want error")
	}
}

func TestRunDispatchesInParallel(testingContext *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	errChannel := make(chan error, 1)

	go func() {
		_, err := Run(context.Background(), Config{
			Queue:      fakeJobQueue{jobs: 2},
			Dispatcher: blockingDispatcher{started: started, release: release},
			MaxJobs:    2,
			Workflow:   "builder-windows-arm64.yml",
		})
		errChannel <- err
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			testingContext.Fatal("timed out waiting for parallel dispatches to start")
		}
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

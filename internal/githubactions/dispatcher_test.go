package githubactions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDispatchWorkflowSendsExpectedRequest(testingContext *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			testingContext.Fatalf("request method = %s, want POST", request.Method)
		}
		if request.URL.Path != "/repos/owner/repo/actions/workflows/builder.yml/dispatches" {
			testingContext.Fatalf("request path = %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer token" {
			testingContext.Fatalf("Authorization header = %q", request.Header.Get("Authorization"))
		}

		var payload struct {
			Ref    string            `json:"ref"`
			Inputs map[string]string `json:"inputs"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			testingContext.Fatalf("Decode() error = %v", err)
		}
		if payload.Ref != "main" {
			testingContext.Fatalf("payload ref = %q, want main", payload.Ref)
		}
		if payload.Inputs["job_index"] != "1" {
			testingContext.Fatalf("payload job_index = %q, want 1", payload.Inputs["job_index"])
		}

		responseWriter.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	dispatcher := Dispatcher{
		Token:      "token",
		APIURL:     server.URL,
		Repository: "owner/repo",
		Ref:        "main",
		HTTPClient: server.Client(),
	}
	err := dispatcher.DispatchWorkflow(context.Background(), DispatchRequest{
		Workflow: "builder.yml",
		Inputs:   map[string]string{"job_index": "1"},
	})
	if err != nil {
		testingContext.Fatalf("DispatchWorkflow() error = %v", err)
	}
}

func TestDispatchWorkflowRequiresToken(testingContext *testing.T) {
	dispatcher := Dispatcher{APIURL: "https://example.test", Repository: "owner/repo", Ref: "main"}
	err := dispatcher.DispatchWorkflow(context.Background(), DispatchRequest{
		Workflow: "builder.yml",
	})
	if err == nil {
		testingContext.Fatal("DispatchWorkflow() error = nil, want error")
	}
}

func TestDispatchWorkflowRejectsUnsafeWorkflow(testingContext *testing.T) {
	dispatcher := Dispatcher{Token: "token", APIURL: "https://example.test", Repository: "owner/repo", Ref: "main"}
	err := dispatcher.DispatchWorkflow(context.Background(), DispatchRequest{Workflow: "../builder.yml"})
	if err == nil {
		testingContext.Fatal("DispatchWorkflow() error = nil, want error")
	}
}

func TestDispatchWorkflowRejectsInvalidAPIURL(testingContext *testing.T) {
	dispatcher := Dispatcher{Token: "token", APIURL: "https://token@example.test", Repository: "owner/repo", Ref: "main"}
	err := dispatcher.DispatchWorkflow(context.Background(), DispatchRequest{Workflow: "builder.yml"})
	if err == nil {
		testingContext.Fatal("DispatchWorkflow() error = nil, want error")
	}
}

func TestActiveWorkflowRunsRejectsUnsafeWorkflow(testingContext *testing.T) {
	dispatcher := Dispatcher{Token: "token", APIURL: "https://example.test", Repository: "owner/repo"}
	_, err := dispatcher.ActiveWorkflowRuns(context.Background(), "builder/windows-arm64.yml")
	if err == nil {
		testingContext.Fatal("ActiveWorkflowRuns() error = nil, want error")
	}
}

func TestActiveWorkflowRunsCountsActiveStatusesFromOneListing(testingContext *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			testingContext.Fatalf("request method = %s, want GET", request.Method)
		}
		if request.URL.Path != "/repos/owner/repo/actions/workflows/builder-windows-arm64.yml/runs" {
			testingContext.Fatalf("request path = %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer token" {
			testingContext.Fatalf("Authorization header = %q", request.Header.Get("Authorization"))
		}

		requestCount.Add(1)
		if request.URL.Query().Has("status") {
			testingContext.Error("workflow runs request must not filter by status")
		}
		if request.URL.Query().Get("per_page") != "100" {
			testingContext.Errorf("per_page = %q, want 100", request.URL.Query().Get("per_page"))
		}
		responseWriter.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(responseWriter).Encode(workflowRunsResponse{
			TotalCount: 6,
			Runs: []workflowRun{
				{ID: 1, Status: "requested", DisplayTitle: "builder-windows-arm64 bot 01"},
				{ID: 2, Status: "queued", DisplayTitle: "builder-windows-arm64 bot 02"},
				{ID: 3, Status: "in_progress", DisplayTitle: "custom builder bot 03"},
				{ID: 4, Status: "waiting", DisplayTitle: "builder-windows-arm64 bot 04"},
				{ID: 5, Status: "pending", DisplayTitle: "builder-windows-arm64 bot 05"},
				{ID: 6, Status: "completed", DisplayTitle: "legacy builder run"},
			},
		})
	}))
	defer server.Close()

	dispatcher := Dispatcher{
		Token:      "token",
		APIURL:     server.URL,
		Repository: "owner/repo",
		HTTPClient: server.Client(),
	}
	activeRuns, err := dispatcher.ActiveWorkflowRuns(context.Background(), "builder-windows-arm64.yml")
	if err != nil {
		testingContext.Fatalf("ActiveWorkflowRuns() error = %v", err)
	}
	if activeRuns.Count != 5 {
		testingContext.Fatalf("active runs = %d, want 5", activeRuns.Count)
	}
	if activeRuns.UnknownCount != 0 {
		testingContext.Fatalf("unknown active runs = %d, want 0", activeRuns.UnknownCount)
	}
	for botIndex := 1; botIndex <= 5; botIndex++ {
		if activeRuns.BotIndexes[botIndex] != 1 {
			testingContext.Fatalf("slot %02d count = %d, want 1", botIndex, activeRuns.BotIndexes[botIndex])
		}
	}

	if got := requestCount.Load(); got != 1 {
		testingContext.Fatalf("request count = %d, want 1", got)
	}
}

func TestActiveWorkflowRunsCountsUnknownRuns(testingContext *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"total_count":3,"workflow_runs":[{"id":1,"status":"queued","display_title":"builder-windows-arm64 bot unknown"},{"id":2,"status":"queued","display_title":"builder-windows-arm64"},{"id":3,"status":"queued","display_title":""}]}`))
	}))
	defer server.Close()

	dispatcher := Dispatcher{
		Token:      "token",
		APIURL:     server.URL,
		Repository: "owner/repo",
		HTTPClient: server.Client(),
	}
	activeRuns, err := dispatcher.ActiveWorkflowRuns(context.Background(), "builder-windows-arm64.yml")
	if err != nil {
		testingContext.Fatalf("ActiveWorkflowRuns() error = %v", err)
	}
	if activeRuns.Count != 3 {
		testingContext.Fatalf("active runs = %d, want 3", activeRuns.Count)
	}
	if activeRuns.UnknownCount != 3 {
		testingContext.Fatalf("unknown active runs = %d, want 3", activeRuns.UnknownCount)
	}
	if len(activeRuns.BotIndexes) != 0 {
		testingContext.Fatalf("bot indexes = %v, want empty", activeRuns.BotIndexes)
	}
}

func TestActiveWorkflowRunsAcceptsWorkflowID(testingContext *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/actions/workflows/12345/runs" {
			testingContext.Errorf("request path = %s", request.URL.Path)
		}
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":1,"status":"in_progress","display_title":"builder-windows-arm64 bot 01"}]}`))
	}))
	defer server.Close()

	dispatcher := Dispatcher{
		Token:      "token",
		APIURL:     server.URL,
		Repository: "owner/repo",
		HTTPClient: server.Client(),
	}
	activeRuns, err := dispatcher.ActiveWorkflowRuns(context.Background(), "12345")
	if err != nil {
		testingContext.Fatalf("ActiveWorkflowRuns() error = %v", err)
	}
	if activeRuns.Count != 1 || activeRuns.UnknownCount != 0 || activeRuns.BotIndexes[1] != 1 {
		testingContext.Fatalf("active runs = %+v, want one known run in slot 01", activeRuns)
	}
}

func TestActiveWorkflowRunsDeduplicatesRunIDs(testingContext *testing.T) {
	for _, testCase := range []struct {
		name         string
		displayTitle string
		firstStatus  string
		unknownCount int
	}{
		{name: "known slot", displayTitle: "builder-windows-arm64 bot 01", firstStatus: "queued"},
		{name: "unknown slot", displayTitle: "builder-windows-arm64 bot unknown", firstStatus: "waiting", unknownCount: 1},
		{name: "restarted run", displayTitle: "builder-windows-arm64 bot 01", firstStatus: "completed"},
	} {
		testingContext.Run(testCase.name, func(testingContext *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Header().Set("Content-Type", "application/json")
				switch request.URL.Query().Get("page") {
				case "1":
					runs := make([]workflowRun, 100)
					for runIndex := range runs {
						runs[runIndex] = workflowRun{ID: int64(runIndex + 1), Status: "completed"}
					}
					runs[99] = workflowRun{ID: 100, Status: testCase.firstStatus, DisplayTitle: testCase.displayTitle}
					_ = json.NewEncoder(responseWriter).Encode(workflowRunsResponse{TotalCount: 101, Runs: runs})
				case "2":
					_ = json.NewEncoder(responseWriter).Encode(workflowRunsResponse{
						TotalCount: 102,
						Runs: []workflowRun{
							{ID: 100, Status: "in_progress", DisplayTitle: testCase.displayTitle},
							{ID: 101, Status: "completed"},
						},
					})
				default:
					http.Error(responseWriter, "unexpected page", http.StatusBadRequest)
				}
			}))
			defer server.Close()

			dispatcher := Dispatcher{
				Token:      "token",
				APIURL:     server.URL,
				Repository: "owner/repo",
				HTTPClient: server.Client(),
			}
			activeRuns, err := dispatcher.ActiveWorkflowRuns(context.Background(), "builder-windows-arm64.yml")
			if err != nil {
				testingContext.Fatalf("ActiveWorkflowRuns() error = %v", err)
			}
			if activeRuns.Count != 1 || activeRuns.UnknownCount != testCase.unknownCount {
				testingContext.Fatalf("active runs = %+v, want one run with unknown count %d", activeRuns, testCase.unknownCount)
			}
			if testCase.unknownCount == 0 && activeRuns.BotIndexes[1] != 1 {
				testingContext.Fatalf("slot 01 count = %d, want 1", activeRuns.BotIndexes[1])
			}
		})
	}
}

func TestActiveWorkflowRunsRejectsInvalidRunIDs(testingContext *testing.T) {
	for _, run := range []string{
		`{"status":"in_progress","display_title":"builder-windows-arm64 bot 01"}`,
		`{"id":0,"status":"in_progress","display_title":"builder-windows-arm64 bot 01"}`,
		`{"id":-1,"status":"in_progress","display_title":"builder-windows-arm64 bot 01"}`,
	} {
		testingContext.Run(run, func(testingContext *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Header().Set("Content-Type", "application/json")
				_, _ = responseWriter.Write([]byte(`{"total_count":1,"workflow_runs":[` + run + `]}`))
			}))
			defer server.Close()

			dispatcher := Dispatcher{
				Token:      "token",
				APIURL:     server.URL,
				Repository: "owner/repo",
				HTTPClient: server.Client(),
			}
			if _, err := dispatcher.ActiveWorkflowRuns(context.Background(), "builder-windows-arm64.yml"); err == nil {
				testingContext.Fatal("ActiveWorkflowRuns() error = nil, want error")
			}
		})
	}
}

func TestActiveWorkflowRunsRejectsUnknownStatuses(testingContext *testing.T) {
	for _, status := range []string{`""`, `null`, `"future_status"`} {
		testingContext.Run(status, func(testingContext *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Header().Set("Content-Type", "application/json")
				_, _ = responseWriter.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":1,"status":` + status + `,"display_title":"builder-windows-arm64 bot 01"}]}`))
			}))
			defer server.Close()

			dispatcher := Dispatcher{
				Token:      "token",
				APIURL:     server.URL,
				Repository: "owner/repo",
				HTTPClient: server.Client(),
			}
			_, err := dispatcher.ActiveWorkflowRuns(context.Background(), "builder-windows-arm64.yml")
			if err == nil || !strings.Contains(err.Error(), "unexpected status") {
				testingContext.Fatalf("ActiveWorkflowRuns() error = %v, want unexpected status error", err)
			}
		})
	}
}

func TestActiveWorkflowRunsPaginatesGrowingHistory(testingContext *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		responseWriter.Header().Set("Content-Type", "application/json")
		response := workflowRunsResponse{TotalCount: 201}
		switch request.URL.Query().Get("page") {
		case "1", "2":
			if request.URL.Query().Get("page") == "1" {
				response.TotalCount = 101
			}
			for runID := 1; runID <= 100; runID++ {
				response.Runs = append(response.Runs, workflowRun{ID: int64(runID), Status: "completed"})
			}
		case "3":
			response.Runs = []workflowRun{{ID: 101, Status: "waiting", DisplayTitle: "builder-windows-arm64 bot 01"}}
		default:
			http.Error(responseWriter, "unexpected page", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(responseWriter).Encode(response)
	}))
	defer server.Close()

	dispatcher := Dispatcher{
		Token:      "token",
		APIURL:     server.URL,
		Repository: "owner/repo",
		HTTPClient: server.Client(),
	}
	activeRuns, err := dispatcher.ActiveWorkflowRuns(context.Background(), "builder-windows-arm64.yml")
	if err != nil {
		testingContext.Fatalf("ActiveWorkflowRuns() error = %v", err)
	}
	if activeRuns.Count != 1 || activeRuns.UnknownCount != 0 || activeRuns.BotIndexes[1] != 1 {
		testingContext.Fatalf("active runs = %+v, want the waiting run from the final page", activeRuns)
	}
	if got := requestCount.Load(); got != 3 {
		testingContext.Fatalf("request count = %d, want 3", got)
	}
}

func TestActiveWorkflowRunsRejectsIncompleteHistory(testingContext *testing.T) {
	for _, testCase := range []struct {
		name       string
		firstCount int
		nextCount  int
		wantError  string
	}{
		{name: "empty page", firstCount: 101, nextCount: 101, wantError: "ended before all runs"},
		{name: "shrinking history", firstCount: 101, nextCount: 100, wantError: "count decreased"},
		{name: "invalid total count", firstCount: 99, wantError: "invalid total count"},
	} {
		testingContext.Run(testCase.name, func(testingContext *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Header().Set("Content-Type", "application/json")
				response := workflowRunsResponse{TotalCount: testCase.nextCount}
				if request.URL.Query().Get("page") == "1" {
					response.TotalCount = testCase.firstCount
					for runID := 1; runID <= 100; runID++ {
						response.Runs = append(response.Runs, workflowRun{ID: int64(runID), Status: "completed"})
					}
				}
				_ = json.NewEncoder(responseWriter).Encode(response)
			}))
			defer server.Close()

			dispatcher := Dispatcher{
				Token:      "token",
				APIURL:     server.URL,
				Repository: "owner/repo",
				HTTPClient: server.Client(),
			}
			_, err := dispatcher.ActiveWorkflowRuns(context.Background(), "builder-windows-arm64.yml")
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				testingContext.Fatalf("ActiveWorkflowRuns() error = %v, want %q", err, testCase.wantError)
			}
		})
	}
}

func TestActiveWorkflowRunsHistoryLimit(testingContext *testing.T) {
	for _, totalCount := range []int{10000, 10001} {
		testingContext.Run(strconv.Itoa(totalCount), func(testingContext *testing.T) {
			var requestCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				requestCount.Add(1)
				page, err := strconv.Atoi(request.URL.Query().Get("page"))
				if err != nil {
					http.Error(responseWriter, "invalid page", http.StatusBadRequest)
					return
				}
				response := workflowRunsResponse{TotalCount: totalCount}
				for runIndex := 0; runIndex < 100; runIndex++ {
					response.Runs = append(response.Runs, workflowRun{ID: int64((page-1)*100 + runIndex + 1), Status: "completed"})
				}
				responseWriter.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(responseWriter).Encode(response)
			}))
			defer server.Close()

			dispatcher := Dispatcher{
				Token:      "token",
				APIURL:     server.URL,
				Repository: "owner/repo",
				HTTPClient: server.Client(),
			}
			activeRuns, err := dispatcher.ActiveWorkflowRuns(context.Background(), "builder-windows-arm64.yml")
			if totalCount == 10001 {
				if err == nil || !strings.Contains(err.Error(), "history exceeds 10000 runs") {
					testingContext.Fatalf("ActiveWorkflowRuns() error = %v, want history limit error", err)
				}
				if got := requestCount.Load(); got != 1 {
					testingContext.Fatalf("request count = %d, want immediate rejection after one page", got)
				}
				return
			}
			if err != nil {
				testingContext.Fatalf("ActiveWorkflowRuns() error = %v", err)
			}
			if activeRuns.Count != 0 || activeRuns.UnknownCount != 0 {
				testingContext.Fatalf("active runs = %+v, want no active runs in completed history", activeRuns)
			}
			if got := requestCount.Load(); got != 100 {
				testingContext.Fatalf("request count = %d, want 100", got)
			}
		})
	}
}

func TestParseWorkflowRunBotIndex(testingContext *testing.T) {
	for _, testCase := range []struct {
		title    string
		botIndex int
		ok       bool
	}{
		{title: "builder-windows-arm64 bot 01", botIndex: 1, ok: true},
		{title: "custom builder bot 99", botIndex: 99, ok: true},
		{title: "builder bot pool bot 02", botIndex: 2, ok: true},
		{title: " builder-windows-arm64 bot 03 ", botIndex: 3, ok: true},
		{title: ""},
		{title: "builder-windows-arm64"},
		{title: "bot 01"},
		{title: "builder-windows-arm64 bot unknown"},
		{title: "builder-windows-arm64 bot 00"},
		{title: "builder-windows-arm64 bot 1"},
		{title: "builder-windows-arm64 bot 100"},
		{title: "builder-windows-arm64 bot -1"},
		{title: "builder-windows-arm64 bot \u0660\u0661"},
		{title: "builder-windows-arm64 bot 01 extra"},
	} {
		testingContext.Run(testCase.title, func(testingContext *testing.T) {
			botIndex, ok := parseWorkflowRunBotIndex(testCase.title)
			if botIndex != testCase.botIndex || ok != testCase.ok {
				testingContext.Fatalf("parseWorkflowRunBotIndex(%q) = (%d, %t), want (%d, %t)", testCase.title, botIndex, ok, testCase.botIndex, testCase.ok)
			}
		})
	}
}

func TestDispatchWorkflowAcceptsSuccessfulResponseWithBody(testingContext *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "application/json")
		responseWriter.WriteHeader(http.StatusOK)
		_, _ = responseWriter.Write([]byte(`{"workflow_run_id":12345}`))
	}))
	defer server.Close()

	dispatcher := Dispatcher{
		Token:      "token",
		APIURL:     server.URL,
		Repository: "owner/repo",
		Ref:        "main",
		HTTPClient: server.Client(),
	}
	err := dispatcher.DispatchWorkflow(context.Background(), DispatchRequest{Workflow: "builder.yml"})
	if err != nil {
		testingContext.Fatalf("DispatchWorkflow() error = %v", err)
	}
}

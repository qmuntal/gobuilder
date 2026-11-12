package githubactions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
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

func TestActiveWorkflowRunsCountsActiveStatuses(testingContext *testing.T) {
	requestedStatuses := map[string]bool{}
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

		status := request.URL.Query().Get("status")
		requestedStatuses[status] = true
		responseWriter.Header().Set("Content-Type", "application/json")
		if status == "in_progress" {
			_, _ = responseWriter.Write([]byte(`{"total_count":3,"workflow_runs":[{"id":1,"display_title":"builder-windows-arm64 bot 02"},{"id":2,"display_title":"custom builder bot 03"},{"id":3,"display_title":"builder-windows-arm64 bot 04"}]}`))
			return
		}
		_, _ = responseWriter.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
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
	if activeRuns.UnknownCount != 0 {
		testingContext.Fatalf("unknown active runs = %d, want 0", activeRuns.UnknownCount)
	}
	if activeRuns.BotIndexes[2] != 1 || activeRuns.BotIndexes[3] != 1 || activeRuns.BotIndexes[4] != 1 {
		testingContext.Fatalf("bot indexes = %v, want 2, 3, and 4", activeRuns.BotIndexes)
	}

	var missingStatuses []string
	for _, status := range activeWorkflowRunStatuses {
		if !requestedStatuses[status] {
			missingStatuses = append(missingStatuses, status)
		}
	}
	sort.Strings(missingStatuses)
	if len(missingStatuses) != 0 {
		testingContext.Fatalf("missing status queries: %v", missingStatuses)
	}
}

func TestActiveWorkflowRunsCountsUnknownRuns(testingContext *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("status") == "queued" {
			_, _ = responseWriter.Write([]byte(`{"total_count":3,"workflow_runs":[{"id":1,"display_title":"builder-windows-arm64 bot unknown"},{"id":2,"display_title":"builder-windows-arm64"},{"id":3,"display_title":""}]}`))
			return
		}
		_, _ = responseWriter.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
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
		if request.URL.Query().Get("status") == "in_progress" {
			_, _ = responseWriter.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":1,"display_title":"builder-windows-arm64 bot 01"}]}`))
			return
		}
		_, _ = responseWriter.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
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
		unknownCount int
	}{
		{name: "known slot", displayTitle: "builder-windows-arm64 bot 01"},
		{name: "unknown slot", displayTitle: "builder-windows-arm64 bot unknown", unknownCount: 1},
	} {
		testingContext.Run(testCase.name, func(testingContext *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				responseWriter.Header().Set("Content-Type", "application/json")
				switch request.URL.Query().Get("status") {
				case "queued", "in_progress":
					_ = json.NewEncoder(responseWriter).Encode(map[string]any{
						"total_count": 1,
						"workflow_runs": []map[string]any{
							{"id": 1, "display_title": testCase.displayTitle},
						},
					})
				default:
					_, _ = responseWriter.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
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
		`{"display_title":"builder-windows-arm64 bot 01"}`,
		`{"id":0,"display_title":"builder-windows-arm64 bot 01"}`,
		`{"id":-1,"display_title":"builder-windows-arm64 bot 01"}`,
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

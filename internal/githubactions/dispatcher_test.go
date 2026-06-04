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
			_, _ = responseWriter.Write([]byte(`{"total_count":3,"workflow_runs":[{"display_title":"builder-windows-arm64 bot 02"},{"display_title":"builder-linux-amd64 bot 03"},{"display_title":"builder-windows-arm64 bot 04"}]}`))
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
	if activeRuns.Count != 2 {
		testingContext.Fatalf("active runs = %d, want 2", activeRuns.Count)
	}
	if activeRuns.UnknownCount != 0 {
		testingContext.Fatalf("unknown active runs = %d, want 0", activeRuns.UnknownCount)
	}
	if activeRuns.BotIndexes[2] != 1 || activeRuns.BotIndexes[4] != 1 {
		testingContext.Fatalf("bot indexes = %v, want 2 and 4", activeRuns.BotIndexes)
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
			_, _ = responseWriter.Write([]byte(`{"total_count":2,"workflow_runs":[{"display_title":"builder-windows-arm64 bot unknown"},{"display_title":"builder-linux-amd64 bot 01"}]}`))
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
	if activeRuns.Count != 1 {
		testingContext.Fatalf("active runs = %d, want 1", activeRuns.Count)
	}
	if activeRuns.UnknownCount != 1 {
		testingContext.Fatalf("unknown active runs = %d, want 1", activeRuns.UnknownCount)
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

package githubactions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

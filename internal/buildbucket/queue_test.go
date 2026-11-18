package buildbucket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueCountsMatchingScheduledBuildsAcrossPages(testingContext *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		requestCount++

		if request.Method != http.MethodPost {
			testingContext.Fatalf("request method = %s, want POST", request.Method)
		}
		if request.URL.Path != "/prpc/buildbucket.v2.Builds/SearchBuilds" {
			testingContext.Fatalf("request path = %s", request.URL.Path)
		}
		if request.Header.Get("Accept") != "application/json" {
			testingContext.Fatalf("Accept header = %q, want application/json", request.Header.Get("Accept"))
		}

		var payload searchBuildsRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			testingContext.Fatalf("Decode() error = %v", err)
		}
		if payload.Predicate.Builder.Project != "golang" {
			testingContext.Fatalf("project = %q, want golang", payload.Predicate.Builder.Project)
		}
		if payload.Predicate.Builder.Bucket != "ci" {
			testingContext.Fatalf("bucket = %q, want ci", payload.Predicate.Builder.Bucket)
		}
		if payload.Predicate.Status != "SCHEDULED" {
			testingContext.Fatalf("status = %q, want SCHEDULED", payload.Predicate.Status)
		}
		if payload.PageSize != defaultPageSize {
			testingContext.Fatalf("page size = %d, want %d", payload.PageSize, defaultPageSize)
		}
		if payload.Mask.Fields != "id,builder" {
			testingContext.Fatalf("mask fields = %q, want id,builder", payload.Mask.Fields)
		}

		responseWriter.Header().Set("Content-Type", "application/json")
		switch requestCount {
		case 1:
			if payload.PageToken != "" {
				testingContext.Fatalf("first page token = %q, want empty", payload.PageToken)
			}
			_, _ = responseWriter.Write([]byte(")]}'\n{" + `"builds":[{"id":"1","builder":{"builder":"gotip-windows-arm64"}},{"id":"2","builder":{"builder":"gotip-linux-amd64"}}],"nextPageToken":"next"}`))
		case 2:
			if payload.PageToken != "next" {
				testingContext.Fatalf("second page token = %q, want next", payload.PageToken)
			}
			_, _ = responseWriter.Write([]byte(")]}'\n{" + `"builds":[{"id":"3","builder":{"builder":"x_tools-go1.25-windows-arm64"}}]}`))
		default:
			testingContext.Fatalf("unexpected request count %d", requestCount)
		}
	}))
	defer server.Close()

	queue, err := NewQueue(QueueConfig{
		BaseURL:     server.URL,
		Project:     "golang",
		Bucket:      "ci",
		BuilderName: "windows-arm64",
		HTTPClient:  server.Client(),
	})
	if err != nil {
		testingContext.Fatalf("NewQueue() error = %v", err)
	}

	jobs, err := queue.JobsInQueue(context.Background())
	if err != nil {
		testingContext.Fatalf("JobsInQueue() error = %v", err)
	}
	if jobs != 2 {
		testingContext.Fatalf("JobsInQueue() = %d, want 2", jobs)
	}
}

func TestQueueCountsAllBuildsWithoutBuilderFilter(testingContext *testing.T) {
	queue := Queue{}
	builds := []searchBuild{
		{Builder: builderID{Builder: "gotip-windows-arm64"}},
		{Builder: builderID{Builder: "gotip-linux-amd64"}},
	}

	if count := queue.countMatchingBuilds(builds); count != 2 {
		testingContext.Fatalf("countMatchingBuilds() = %d, want 2", count)
	}
}

func TestBusyBotsUsesActualBotAndLatestStartedBuild(testingContext *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var query searchBuildsRequest
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			testingContext.Error(err)
		}
		if query.Predicate.Status != "STARTED" || query.Predicate.Builder != (builderID{Project: "golang", Bucket: "ci"}) ||
			query.Mask.Fields != "startTime,infra.swarming.botDimensions" {
			testingContext.Errorf("unexpected running-build query: %+v", query)
		}
		switch query.PageToken {
		case "":
			_, _ = w.Write([]byte(`{"builds":[
					{"startTime":"2026-01-01T10:00:00Z","infra":{"swarming":{"botDimensions":[{"key":"id","value":"fleet--01"}]}}},
					{"startTime":"2026-01-01T10:05:00Z","infra":{"swarming":{"taskDimensions":[{"key":"id","value":"requested-only"}]}}}
				],"nextPageToken":"next"}`))
		case "next":
			_, _ = w.Write([]byte(`{"builds":[
					{"startTime":"2026-01-01T10:04:00Z","infra":{"swarming":{"botDimensions":[{"key":"os","value":"Windows"},{"key":"id","value":"fleet--01"}]}}}
				]}`))
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	queue, err := NewQueue(QueueConfig{
		BaseURL: server.URL, Project: "golang", Bucket: "ci", Builder: "specific-builder", HTTPClient: server.Client(),
	})
	if err != nil {
		testingContext.Fatal(err)
	}
	bots, err := queue.BusyBots(context.Background())
	if err != nil {
		testingContext.Fatal(err)
	}
	want := time.Date(2026, 1, 1, 10, 4, 0, 0, time.UTC)
	if len(bots) != 1 || !bots["fleet--01"].Equal(want) {
		testingContext.Fatalf("busy bots = %v, want only fleet--01 started at %v", bots, want)
	}
}

func TestQueueRejectsRepeatedPageTokens(testingContext *testing.T) {
	for _, testCase := range []struct {
		name   string
		tokens []string
	}{
		{name: "immediate repeat", tokens: []string{"a", "a"}},
		{name: "two-token cycle", tokens: []string{"a", "b", "a"}},
		{name: "three-token cycle", tokens: []string{"a", "b", "c", "a"}},
	} {
		testingContext.Run(testCase.name, func(testingContext *testing.T) {
			var requestCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
				requestIndex := int(requestCount.Add(1)) - 1
				if requestIndex >= len(testCase.tokens) {
					http.Error(responseWriter, "unexpected extra page request", http.StatusInternalServerError)
					return
				}
				responseWriter.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(responseWriter).Encode(searchBuildsResponse{
					NextPageToken: testCase.tokens[requestIndex],
				})
			}))
			defer server.Close()

			queue, err := NewQueue(QueueConfig{
				BaseURL:    server.URL,
				Project:    "golang",
				HTTPClient: server.Client(),
			})
			if err != nil {
				testingContext.Fatalf("NewQueue() error = %v", err)
			}
			_, err = queue.JobsInQueue(context.Background())
			if err == nil || !strings.Contains(err.Error(), "repeated page token") {
				testingContext.Fatalf("JobsInQueue() error = %v, want repeated page token error", err)
			}
			if got := int(requestCount.Load()); got != len(testCase.tokens) {
				testingContext.Fatalf("request count = %d, want %d", got, len(testCase.tokens))
			}
		})
	}
}

func TestQueueRequiresProject(testingContext *testing.T) {
	_, err := NewQueue(QueueConfig{})
	if err == nil {
		testingContext.Fatal("NewQueue() error = nil, want error")
	}
}

func TestQueueRejectsInvalidBaseURL(testingContext *testing.T) {
	_, err := NewQueue(QueueConfig{
		BaseURL: "https://token@example.test",
		Project: "golang",
	})
	if err == nil {
		testingContext.Fatal("NewQueue() error = nil, want error")
	}
}

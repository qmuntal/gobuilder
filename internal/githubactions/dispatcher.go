package githubactions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultAPIURL = "https://api.github.com"

type Dispatcher struct {
	Token      string
	APIURL     string
	Repository string
	Ref        string
	HTTPClient *http.Client
}

type DispatchRequest struct {
	Workflow string
	Inputs   map[string]string
}

var activeWorkflowRunStatuses = []string{"requested", "queued", "in_progress", "waiting", "pending"}
var workflowRunBotIndexPattern = regexp.MustCompile(`\bbot (\d{2})$`)

type ActiveWorkflowRuns struct {
	Count        int
	BotIndexes   map[int]int
	UnknownCount int
}

func (dispatcher Dispatcher) ActiveWorkflowRuns(ctx context.Context, workflow string) (ActiveWorkflowRuns, error) {
	if strings.TrimSpace(workflow) == "" {
		return ActiveWorkflowRuns{}, fmt.Errorf("workflow is required")
	}
	if strings.TrimSpace(dispatcher.Token) == "" {
		return ActiveWorkflowRuns{}, fmt.Errorf("github token is required")
	}

	activeRuns := ActiveWorkflowRuns{BotIndexes: map[int]int{}}
	for _, status := range activeWorkflowRunStatuses {
		runs, err := dispatcher.workflowRuns(ctx, workflow, status)
		if err != nil {
			return ActiveWorkflowRuns{}, err
		}
		activeRuns.Count += runs.TotalCount
		activeRuns.UnknownCount += runs.TotalCount - len(runs.Runs)
		for _, run := range runs.Runs {
			botIndex, ok := parseWorkflowRunBotIndex(run.DisplayTitle)
			if !ok {
				activeRuns.UnknownCount++
				continue
			}
			activeRuns.BotIndexes[botIndex]++
		}
	}
	return activeRuns, nil
}

type workflowRunsResponse struct {
	TotalCount int           `json:"total_count"`
	Runs       []workflowRun `json:"workflow_runs"`
}

type workflowRun struct {
	DisplayTitle string `json:"display_title"`
}

func (dispatcher Dispatcher) workflowRuns(ctx context.Context, workflow, status string) (workflowRunsResponse, error) {
	owner, repositoryName, err := splitRepository(dispatcher.Repository)
	if err != nil {
		return workflowRunsResponse{}, err
	}

	apiURL := strings.TrimRight(dispatcher.APIURL, "/")
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}

	requestURL := fmt.Sprintf(
		"%s/repos/%s/%s/actions/workflows/%s/runs",
		apiURL,
		url.PathEscape(owner),
		url.PathEscape(repositoryName),
		url.PathEscape(workflow),
	)
	query := url.Values{}
	query.Set("status", status)
	query.Set("per_page", "100")
	requestURL += "?" + query.Encode()

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return workflowRunsResponse{}, fmt.Errorf("create workflow runs request: %w", err)
	}
	dispatcher.setGitHubHeaders(httpRequest)

	httpClient := dispatcher.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return workflowRunsResponse{}, fmt.Errorf("send workflow runs request: %w", err)
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(io.LimitReader(httpResponse.Body, 4096))
		trimmedBody := strings.TrimSpace(string(responseBody))
		if trimmedBody != "" {
			return workflowRunsResponse{}, fmt.Errorf("github workflow runs query failed with status %d: %s", httpResponse.StatusCode, trimmedBody)
		}
		return workflowRunsResponse{}, fmt.Errorf("github workflow runs query failed with status %d", httpResponse.StatusCode)
	}

	var payload workflowRunsResponse
	if err := json.NewDecoder(httpResponse.Body).Decode(&payload); err != nil {
		return workflowRunsResponse{}, fmt.Errorf("decode workflow runs response: %w", err)
	}
	return payload, nil
}

func parseWorkflowRunBotIndex(displayTitle string) (int, bool) {
	matches := workflowRunBotIndexPattern.FindStringSubmatch(strings.TrimSpace(displayTitle))
	if matches == nil {
		return 0, false
	}
	botIndex, err := strconv.Atoi(matches[1])
	if err != nil || botIndex < 1 {
		return 0, false
	}
	return botIndex, true
}

func (dispatcher Dispatcher) DispatchWorkflow(ctx context.Context, dispatchRequest DispatchRequest) error {
	owner, repositoryName, err := splitRepository(dispatcher.Repository)
	if err != nil {
		return err
	}
	if strings.TrimSpace(dispatchRequest.Workflow) == "" {
		return fmt.Errorf("workflow is required")
	}
	if strings.TrimSpace(dispatcher.Ref) == "" {
		return fmt.Errorf("ref is required")
	}
	if strings.TrimSpace(dispatcher.Token) == "" {
		return fmt.Errorf("github token is required")
	}

	payloadBody, err := json.Marshal(struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs,omitempty"`
	}{
		Ref:    dispatcher.Ref,
		Inputs: dispatchRequest.Inputs,
	})
	if err != nil {
		return fmt.Errorf("encode workflow dispatch payload: %w", err)
	}

	apiURL := strings.TrimRight(dispatcher.APIURL, "/")
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}

	requestURL := fmt.Sprintf(
		"%s/repos/%s/%s/actions/workflows/%s/dispatches",
		apiURL,
		url.PathEscape(owner),
		url.PathEscape(repositoryName),
		url.PathEscape(dispatchRequest.Workflow),
	)

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payloadBody))
	if err != nil {
		return fmt.Errorf("create workflow dispatch request: %w", err)
	}
	httpRequest.Header.Set("Accept", "application/vnd.github+json")
	httpRequest.Header.Set("Content-Type", "application/json")
	dispatcher.setGitHubHeaders(httpRequest)

	httpClient := dispatcher.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	httpResponse, err := httpClient.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("send workflow dispatch request: %w", err)
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(io.LimitReader(httpResponse.Body, 4096))
		trimmedBody := strings.TrimSpace(string(responseBody))
		if trimmedBody != "" {
			return fmt.Errorf("github workflow dispatch failed with status %d: %s", httpResponse.StatusCode, trimmedBody)
		}
		return fmt.Errorf("github workflow dispatch failed with status %d", httpResponse.StatusCode)
	}

	return nil
}

func (dispatcher Dispatcher) setGitHubHeaders(httpRequest *http.Request) {
	httpRequest.Header.Set("Accept", "application/vnd.github+json")
	httpRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(dispatcher.Token))
	httpRequest.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func splitRepository(repository string) (string, string, error) {
	trimmedRepository := strings.Trim(strings.TrimSpace(repository), "/")
	owner, repositoryName, found := strings.Cut(trimmedRepository, "/")
	if !found || owner == "" || repositoryName == "" || strings.Contains(repositoryName, "/") {
		return "", "", fmt.Errorf("repository must be in owner/name format")
	}
	return owner, repositoryName, nil
}

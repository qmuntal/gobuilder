package githubactions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultAPIURL       = "https://api.github.com"
	maxWorkflowRunPages = 100
)

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

type ActiveWorkflowRuns struct {
	Count        int
	BotIndexes   map[int]int
	UnknownCount int
}

func (dispatcher Dispatcher) ActiveWorkflowRuns(ctx context.Context, workflow string) (ActiveWorkflowRuns, error) {
	if err := validateWorkflow(workflow); err != nil {
		return ActiveWorkflowRuns{}, err
	}
	if strings.TrimSpace(dispatcher.Token) == "" {
		return ActiveWorkflowRuns{}, fmt.Errorf("github token is required")
	}

	workflow = strings.TrimSpace(workflow)
	targetRunNamePrefix := workflowRunNamePrefix(workflow)
	activeRuns := ActiveWorkflowRuns{BotIndexes: map[int]int{}}
	for _, status := range activeWorkflowRunStatuses {
		runs, err := dispatcher.workflowRuns(ctx, workflow, status)
		if err != nil {
			return ActiveWorkflowRuns{}, err
		}
		for _, run := range runs.Runs {
			botIndex, isTargetRun, ok := parseWorkflowRunBotIndex(run.DisplayTitle, targetRunNamePrefix)
			if !isTargetRun {
				continue
			}
			activeRuns.Count++
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

	apiURL, err := apiBaseURL(dispatcher.APIURL)
	if err != nil {
		return workflowRunsResponse{}, err
	}

	requestURL := fmt.Sprintf(
		"%s/repos/%s/%s/actions/workflows/%s/runs",
		apiURL,
		url.PathEscape(owner),
		url.PathEscape(repositoryName),
		url.PathEscape(workflow),
	)
	var allRuns workflowRunsResponse
	for page := 1; page <= maxWorkflowRunPages; page++ {
		query := url.Values{}
		query.Set("status", status)
		query.Set("per_page", "100")
		query.Set("page", strconv.Itoa(page))

		pageResponse, err := dispatcher.workflowRunsPage(ctx, requestURL+"?"+query.Encode())
		if err != nil {
			return workflowRunsResponse{}, err
		}
		if page == 1 {
			allRuns.TotalCount = pageResponse.TotalCount
		}
		allRuns.Runs = append(allRuns.Runs, pageResponse.Runs...)
		if len(pageResponse.Runs) == 0 || len(allRuns.Runs) >= allRuns.TotalCount {
			return allRuns, nil
		}
	}
	return workflowRunsResponse{}, fmt.Errorf("github workflow runs query exceeded %d pages", maxWorkflowRunPages)
}

func (dispatcher Dispatcher) workflowRunsPage(ctx context.Context, requestURL string) (workflowRunsResponse, error) {
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

func workflowRunNamePrefix(workflow string) string {
	workflow = strings.TrimSpace(workflow)
	if index := strings.LastIndexAny(workflow, `/\`); index >= 0 {
		workflow = workflow[index+1:]
	}
	workflow = strings.TrimSuffix(strings.TrimSuffix(workflow, ".yaml"), ".yml")
	return workflow + " bot "
}

func parseWorkflowRunBotIndex(displayTitle, targetRunNamePrefix string) (botIndex int, isTargetRun bool, ok bool) {
	displayTitle = strings.TrimSpace(displayTitle)
	if !strings.HasPrefix(displayTitle, targetRunNamePrefix) {
		return 0, false, false
	}

	rawBotIndex := strings.TrimSpace(strings.TrimPrefix(displayTitle, targetRunNamePrefix))
	if len(rawBotIndex) != 2 {
		return 0, true, false
	}
	for _, digit := range rawBotIndex {
		if digit < '0' || digit > '9' {
			return 0, true, false
		}
	}

	botIndex, err := strconv.Atoi(rawBotIndex)
	if err != nil || botIndex < 1 {
		return 0, true, false
	}
	return botIndex, true, true
}

func (dispatcher Dispatcher) DispatchWorkflow(ctx context.Context, dispatchRequest DispatchRequest) error {
	owner, repositoryName, err := splitRepository(dispatcher.Repository)
	if err != nil {
		return err
	}
	if err := validateWorkflow(dispatchRequest.Workflow); err != nil {
		return err
	}
	if strings.TrimSpace(dispatcher.Ref) == "" {
		return fmt.Errorf("ref is required")
	}
	if strings.TrimSpace(dispatcher.Token) == "" {
		return fmt.Errorf("github token is required")
	}

	workflow := strings.TrimSpace(dispatchRequest.Workflow)
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

	apiURL, err := apiBaseURL(dispatcher.APIURL)
	if err != nil {
		return err
	}

	requestURL := fmt.Sprintf(
		"%s/repos/%s/%s/actions/workflows/%s/dispatches",
		apiURL,
		url.PathEscape(owner),
		url.PathEscape(repositoryName),
		url.PathEscape(workflow),
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

func validateWorkflow(workflow string) error {
	workflow = strings.TrimSpace(workflow)
	if workflow == "" {
		return fmt.Errorf("workflow is required")
	}
	if len(workflow) > 128 {
		return fmt.Errorf("workflow is too long")
	}
	for _, character := range workflow {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '.', character == '_', character == '-':
		default:
			return fmt.Errorf("workflow must contain only letters, digits, '.', '_' or '-'")
		}
	}
	return nil
}

func apiBaseURL(rawAPIURL string) (string, error) {
	rawAPIURL = strings.TrimSpace(rawAPIURL)
	if rawAPIURL == "" {
		rawAPIURL = DefaultAPIURL
	}
	parsedURL, err := url.Parse(rawAPIURL)
	if err != nil {
		return "", fmt.Errorf("parse github api url: %w", err)
	}
	if parsedURL.Scheme != "https" && parsedURL.Scheme != "http" {
		return "", fmt.Errorf("github api url must use http or https")
	}
	if parsedURL.Host == "" {
		return "", fmt.Errorf("github api url must include a host")
	}
	if parsedURL.User != nil {
		return "", fmt.Errorf("github api url must not include user info")
	}
	if parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return "", fmt.Errorf("github api url must not include query or fragment")
	}
	return strings.TrimRight(parsedURL.String(), "/"), nil
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

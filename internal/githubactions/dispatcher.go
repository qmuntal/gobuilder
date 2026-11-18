package githubactions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultAPIURL          = "https://api.github.com"
	maxWorkflowRunPages    = 10 // GitHub caps filtered searches at 1,000 results.
	workflowRunPageSize    = 100
	maxWorkflowRunAttempts = 3
)

var errUnstableRunListing = errors.New("inconsistent workflow run listing")

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

type ActiveWorkflowRuns struct {
	Count           int
	BotIndexes      map[int]int
	UnknownCount    int
	InProgressSince map[int]time.Time
}

func (dispatcher Dispatcher) ActiveWorkflowRuns(ctx context.Context, workflow string) (ActiveWorkflowRuns, error) {
	if err := validateWorkflow(workflow); err != nil {
		return ActiveWorkflowRuns{}, err
	}
	if strings.TrimSpace(dispatcher.Token) == "" {
		return ActiveWorkflowRuns{}, fmt.Errorf("github token is required")
	}

	// Status queries are not an atomic snapshot. Look for in-progress runs
	// last, then repeat to catch transitions back into an already-read state
	// (e.g. a job returning to the queue after approval). Reserve the union.
	statuses := [...]string{"requested", "waiting", "pending", "queued", "in_progress"}
	seen := make(map[int64]workflowRun)
	for range 2 {
		for _, status := range statuses {
			if err := dispatcher.collectStatusRuns(ctx, strings.TrimSpace(workflow), status, seen); err != nil {
				return ActiveWorkflowRuns{}, fmt.Errorf("list %s runs: %w", status, err)
			}
		}
	}

	activeRuns := ActiveWorkflowRuns{
		Count: len(seen), BotIndexes: make(map[int]int), InProgressSince: make(map[int]time.Time),
	}
	for _, run := range seen {
		botIndex, ok := parseWorkflowRunBotIndex(run.DisplayTitle)
		if !ok {
			activeRuns.UnknownCount++
			continue
		}
		activeRuns.BotIndexes[botIndex]++
		if run.Status == "in_progress" && run.StartedAt.After(activeRuns.InProgressSince[botIndex]) {
			activeRuns.InProgressSince[botIndex] = run.StartedAt
		}
	}
	return activeRuns, nil
}

func (dispatcher Dispatcher) collectStatusRuns(ctx context.Context, workflow, status string, seen map[int64]workflowRun) error {
	for attempt := 1; ; attempt++ {
		runs, err := dispatcher.workflowRuns(ctx, workflow, status)
		// Even an incomplete read can identify occupied slots. Keep those
		// observations, but require a complete read before allowing dispatch.
		for _, run := range runs.Runs {
			switch run.Status {
			case "completed":
			case "requested", "waiting", "pending", "queued", "in_progress":
				seen[run.ID] = run
			default:
				return fmt.Errorf("github workflow run %d has unexpected status %q", run.ID, run.Status)
			}
		}
		if !errors.Is(err, errUnstableRunListing) || attempt == maxWorkflowRunAttempts {
			return err
		}
		if err := waitForWorkflowRunRetry(ctx, attempt); err != nil {
			return err
		}
	}
}

type workflowRunsResponse struct {
	TotalCount int           `json:"total_count"`
	Runs       []workflowRun `json:"workflow_runs"`
}

type workflowRun struct {
	ID           int64     `json:"id"`
	DisplayTitle string    `json:"display_title"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"run_started_at"`
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
	seen := make(map[int64]bool)
	for page := 1; page <= maxWorkflowRunPages; page++ {
		query := url.Values{}
		query.Set("status", status)
		query.Set("per_page", strconv.Itoa(workflowRunPageSize))
		query.Set("page", strconv.Itoa(page))

		pageResponse, err := dispatcher.workflowRunsPage(ctx, requestURL+"?"+query.Encode())
		if err != nil {
			return workflowRunsResponse{}, err
		}
		if pageResponse.TotalCount < len(pageResponse.Runs) {
			return workflowRunsResponse{}, fmt.Errorf("github workflow runs query returned invalid total count %d", pageResponse.TotalCount)
		}
		if pageResponse.TotalCount > maxWorkflowRunPages*workflowRunPageSize {
			return workflowRunsResponse{}, fmt.Errorf("github workflow runs query exceeds %d %s runs", maxWorkflowRunPages*workflowRunPageSize, status)
		}
		var repeatedID int64
		for _, run := range pageResponse.Runs {
			if run.ID <= 0 {
				return workflowRunsResponse{}, fmt.Errorf("github workflow runs query returned invalid run ID %d", run.ID)
			}
			if seen[run.ID] {
				repeatedID = run.ID
			}
			seen[run.ID] = true
		}
		allRuns.Runs = append(allRuns.Runs, pageResponse.Runs...)
		prevTotal := allRuns.TotalCount
		allRuns.TotalCount = pageResponse.TotalCount
		if allRuns.TotalCount < prevTotal {
			return allRuns, fmt.Errorf("%w: count decreased from %d to %d on page %d", errUnstableRunListing, prevTotal, allRuns.TotalCount, page)
		}
		if repeatedID != 0 {
			return allRuns, fmt.Errorf("%w: run %d repeated on page %d", errUnstableRunListing, repeatedID, page)
		}
		if len(allRuns.Runs) >= allRuns.TotalCount {
			return allRuns, nil
		}
		if len(pageResponse.Runs) == 0 {
			return allRuns, fmt.Errorf("%w: ended before all runs were listed (page %d, listed %d, total_count %d)", errUnstableRunListing, page, len(allRuns.Runs), allRuns.TotalCount)
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

	var httpResponse *http.Response
	for attempt := 1; ; attempt++ {
		httpResponse, err = httpClient.Do(httpRequest)
		if err != nil {
			return workflowRunsResponse{}, fmt.Errorf("send workflow runs request: %w", err)
		}
		retryable := httpResponse.StatusCode == http.StatusBadGateway ||
			httpResponse.StatusCode == http.StatusServiceUnavailable ||
			httpResponse.StatusCode == http.StatusGatewayTimeout
		if !retryable || attempt == maxWorkflowRunAttempts {
			break
		}

		_, _ = io.Copy(io.Discard, io.LimitReader(httpResponse.Body, 4096))
		httpResponse.Body.Close()

		if err := waitForWorkflowRunRetry(ctx, attempt); err != nil {
			return workflowRunsResponse{}, err
		}
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

func waitForWorkflowRunRetry(ctx context.Context, attempt int) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait to retry workflow runs request: %w", ctx.Err())
	case <-time.After(time.Second << (attempt - 1)):
		return nil
	}
}

func parseWorkflowRunBotIndex(displayTitle string) (botIndex int, ok bool) {
	const separator = " bot "
	displayTitle = strings.TrimSpace(displayTitle)
	separatorIndex := strings.LastIndex(displayTitle, separator)
	if separatorIndex <= 0 {
		return 0, false
	}

	rawBotIndex := strings.TrimSpace(displayTitle[separatorIndex+len(separator):])
	if len(rawBotIndex) != 2 {
		return 0, false
	}
	for _, digit := range rawBotIndex {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}

	botIndex, err := strconv.Atoi(rawBotIndex)
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

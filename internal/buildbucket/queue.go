package buildbucket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultURL      = "https://cr-buildbucket.appspot.com"
	defaultPageSize = 1000
)

type QueueConfig struct {
	BaseURL     string
	Project     string
	Bucket      string
	Builder     string
	BuilderName string
	HTTPClient  *http.Client
}

type Queue struct {
	baseURL     string
	project     string
	bucket      string
	builder     string
	builderName string
	httpClient  *http.Client
}

func NewQueue(config QueueConfig) (Queue, error) {
	baseURL, err := normalizeBaseURL(config.BaseURL)
	if err != nil {
		return Queue{}, err
	}

	project := strings.TrimSpace(config.Project)
	if project == "" {
		return Queue{}, fmt.Errorf("buildbucket project is required")
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return Queue{
		baseURL:     baseURL,
		project:     project,
		bucket:      strings.TrimSpace(config.Bucket),
		builder:     strings.TrimSpace(config.Builder),
		builderName: strings.TrimSpace(config.BuilderName),
		httpClient:  httpClient,
	}, nil
}

func normalizeBaseURL(rawBaseURL string) (string, error) {
	rawBaseURL = strings.TrimSpace(rawBaseURL)
	if rawBaseURL == "" {
		rawBaseURL = DefaultURL
	}
	parsedURL, err := url.Parse(rawBaseURL)
	if err != nil {
		return "", fmt.Errorf("parse buildbucket url: %w", err)
	}
	if parsedURL.Scheme != "https" && parsedURL.Scheme != "http" {
		return "", fmt.Errorf("buildbucket url must use http or https")
	}
	if parsedURL.Host == "" {
		return "", fmt.Errorf("buildbucket url must include a host")
	}
	if parsedURL.User != nil {
		return "", fmt.Errorf("buildbucket url must not include user info")
	}
	if parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return "", fmt.Errorf("buildbucket url must not include query or fragment")
	}
	return strings.TrimRight(parsedURL.String(), "/"), nil
}

func (queue Queue) JobsInQueue(ctx context.Context) (int, error) {
	queueDepth := 0
	err := queue.visitBuildPages(ctx, "SCHEDULED", queue.builder, "id,builder", func(builds []searchBuild) {
		queueDepth += queue.countMatchingBuilds(builds)
	})
	if err != nil {
		return 0, err
	}
	return queueDepth, nil
}

// BusyBots reports the newest currently started build on each actual Swarming
// bot. A one-shot bot cannot serve new work even if its task is for a different
// builder from the one selected by the queue filter.
func (queue Queue) BusyBots(ctx context.Context) (map[string]time.Time, error) {
	bots := make(map[string]time.Time)
	err := queue.visitBuildPages(ctx, "STARTED", "", "startTime,infra.swarming.botDimensions", func(builds []searchBuild) {
		for _, build := range builds {
			for _, dim := range build.Infra.Swarming.BotDimensions {
				if dim.Key == "id" && dim.Value != "" && build.StartTime.After(bots[dim.Value]) {
					bots[dim.Value] = build.StartTime
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return bots, nil
}

func (queue Queue) visitBuildPages(ctx context.Context, status, builder, fields string, visit func([]searchBuild)) error {
	pageToken := ""
	seenPageTokens := map[string]bool{}

	for {
		response, err := queue.searchBuilds(ctx, status, builder, fields, pageToken)
		if err != nil {
			return err
		}
		visit(response.Builds)
		if response.NextPageToken == "" {
			return nil
		}
		if seenPageTokens[response.NextPageToken] {
			return fmt.Errorf("buildbucket returned repeated page token %q", response.NextPageToken)
		}

		seenPageTokens[response.NextPageToken] = true
		pageToken = response.NextPageToken
	}
}

func (queue Queue) countMatchingBuilds(builds []searchBuild) int {
	if queue.builderName == "" {
		return len(builds)
	}

	count := 0
	for _, build := range builds {
		if strings.Contains(build.Builder.Builder, queue.builderName) {
			count++
		}
	}
	return count
}

func (queue Queue) searchBuilds(ctx context.Context, status, builder, fields, pageToken string) (searchBuildsResponse, error) {
	requestBody, err := json.Marshal(searchBuildsRequest{
		Predicate: buildPredicate{
			Builder: builderID{
				Project: queue.project,
				Bucket:  queue.bucket,
				Builder: builder,
			},
			Status:              status,
			IncludeExperimental: true,
		},
		PageSize:  defaultPageSize,
		PageToken: pageToken,
		Mask: buildMask{
			Fields: fields,
		},
	})
	if err != nil {
		return searchBuildsResponse{}, fmt.Errorf("encode buildbucket search request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, queue.baseURL+"/prpc/buildbucket.v2.Builds/SearchBuilds", bytes.NewReader(requestBody))
	if err != nil {
		return searchBuildsResponse{}, fmt.Errorf("create buildbucket search request: %w", err)
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")

	httpResponse, err := queue.httpClient.Do(httpRequest)
	if err != nil {
		return searchBuildsResponse{}, fmt.Errorf("send buildbucket search request: %w", err)
	}
	defer httpResponse.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, 4<<20))
	if err != nil {
		return searchBuildsResponse{}, fmt.Errorf("read buildbucket search response: %w", err)
	}
	responseBody = trimPRPCXSSIPrefix(responseBody)

	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		trimmedBody := strings.TrimSpace(string(responseBody))
		if trimmedBody != "" {
			return searchBuildsResponse{}, fmt.Errorf("buildbucket search failed with status %d: %s", httpResponse.StatusCode, trimmedBody)
		}
		return searchBuildsResponse{}, fmt.Errorf("buildbucket search failed with status %d", httpResponse.StatusCode)
	}

	var response searchBuildsResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return searchBuildsResponse{}, fmt.Errorf("decode buildbucket search response: %w", err)
	}

	return response, nil
}

func trimPRPCXSSIPrefix(responseBody []byte) []byte {
	const prefix = ")]}'"
	if !bytes.HasPrefix(responseBody, []byte(prefix)) {
		return responseBody
	}

	responseBody = responseBody[len(prefix):]
	responseBody = bytes.TrimPrefix(responseBody, []byte("\r\n"))
	responseBody = bytes.TrimPrefix(responseBody, []byte("\n"))
	return responseBody
}

type searchBuildsRequest struct {
	Predicate buildPredicate `json:"predicate"`
	PageSize  int            `json:"pageSize,omitempty"`
	PageToken string         `json:"pageToken,omitempty"`
	Mask      buildMask      `json:"mask"`
}

type buildPredicate struct {
	Builder             builderID `json:"builder"`
	Status              string    `json:"status"`
	IncludeExperimental bool      `json:"includeExperimental,omitempty"`
}

type builderID struct {
	Project string `json:"project"`
	Bucket  string `json:"bucket,omitempty"`
	Builder string `json:"builder,omitempty"`
}

type buildMask struct {
	Fields string `json:"fields,omitempty"`
}

type searchBuildsResponse struct {
	Builds        []searchBuild `json:"builds"`
	NextPageToken string        `json:"nextPageToken"`
}

type searchBuild struct {
	ID        string    `json:"id"`
	Builder   builderID `json:"builder"`
	StartTime time.Time `json:"startTime"`
	Infra     struct {
		Swarming struct {
			BotDimensions []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"botDimensions"`
		} `json:"swarming"`
	} `json:"infra"`
}

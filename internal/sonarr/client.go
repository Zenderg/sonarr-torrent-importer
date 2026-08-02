package sonarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const maxErrorBody = 8 << 10

type Client struct {
	baseURL *url.URL
	apiKey  string
	http    *http.Client
}

type APIError struct {
	Method     string
	Endpoint   string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Sonarr %s %s returned HTTP %d: %s", e.Method, e.Endpoint, e.StatusCode, e.Message)
}

func NewClient(baseURL *url.URL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL: cloneURL(baseURL),
		apiKey:  apiKey,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) > 0 && !sameOrigin(request.URL, via[0].URL) {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
	}
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func (c *Client) SystemStatus(ctx context.Context) (SystemStatus, error) {
	var result SystemStatus
	err := c.doJSON(ctx, http.MethodGet, "/api/v3/system/status", nil, nil, &result)
	return result, err
}

func (c *Client) Queue(ctx context.Context) ([]QueueRecord, error) {
	query := url.Values{
		"includeSeries":  {"true"},
		"includeEpisode": {"true"},
	}
	var records []QueueRecord
	err := c.doJSON(ctx, http.MethodGet, "/api/v3/queue/details", query, nil, &records)
	return records, err
}

func (c *Client) Episodes(ctx context.Context, seriesID, seasonNumber int) ([]Episode, error) {
	query := url.Values{
		"seriesId":           {strconv.Itoa(seriesID)},
		"seasonNumber":       {strconv.Itoa(seasonNumber)},
		"includeEpisodeFile": {"true"},
	}
	var result []Episode
	err := c.doJSON(ctx, http.MethodGet, "/api/v3/episode", query, nil, &result)
	return result, err
}

func (c *Client) ManualImportCandidates(ctx context.Context, folder, downloadID string) ([]ManualImportCandidate, error) {
	query := url.Values{
		"filterExistingFiles": {"true"},
	}
	if downloadID != "" {
		query.Set("downloadId", downloadID)
	}
	if folder != "" {
		query.Set("folder", folder)
	}
	var result []ManualImportCandidate
	err := c.doJSON(ctx, http.MethodGet, "/api/v3/manualimport", query, nil, &result)
	return result, err
}

func (c *Client) Reprocess(ctx context.Context, items []ManualImportReprocess) ([]ManualImportReprocess, error) {
	var result []ManualImportReprocess
	err := c.doJSON(ctx, http.MethodPost, "/api/v3/manualimport", nil, items, &result)
	return result, err
}

func (c *Client) StartManualImport(ctx context.Context, files []ManualImportFile) (Command, error) {
	return c.StartManualImportWithMode(ctx, files, "auto")
}

func (c *Client) StartManualImportWithMode(ctx context.Context, files []ManualImportFile, importMode string) (Command, error) {
	if importMode != "auto" && importMode != "copy" {
		return Command{}, fmt.Errorf("unsupported Sonarr ManualImport mode %q", importMode)
	}
	request := ManualImportCommand{
		Name:       "ManualImport",
		Files:      files,
		ImportMode: importMode,
	}
	var result Command
	err := c.doJSON(ctx, http.MethodPost, "/api/v3/command", nil, request, &result)
	return result, err
}

func (c *Client) Command(ctx context.Context, id int) (Command, error) {
	var result Command
	err := c.doJSON(ctx, http.MethodGet, "/api/v3/command/"+strconv.Itoa(id), nil, nil, &result)
	return result, err
}

func (c *Client) History(ctx context.Context, downloadID string) ([]HistoryRecord, error) {
	const pageSize = 1000
	query := url.Values{
		"page":           {"1"},
		"pageSize":       {strconv.Itoa(pageSize)},
		"sortKey":        {"date"},
		"sortDirection":  {"descending"},
		"includeSeries":  {"false"},
		"includeEpisode": {"false"},
		"downloadId":     {downloadID},
		"eventType":      {"3"},
	}
	var result HistoryPage
	if err := c.doJSON(ctx, http.MethodGet, "/api/v3/history", query, nil, &result); err != nil {
		return nil, err
	}
	if result.TotalRecords > pageSize {
		return nil, fmt.Errorf("Sonarr history for download %q exceeds the verification limit of %d records", downloadID, pageSize)
	}
	return result.Records, nil
}

// RecentImportHistory returns the bounded newest import receipts without a
// downloadId filter. Sonarr omits downloadId from ManualImport history when
// the source torrent was added directly rather than tracked in its queue.
func (c *Client) RecentImportHistory(ctx context.Context) ([]HistoryRecord, error) {
	const pageSize = 1000
	query := url.Values{
		"page":           {"1"},
		"pageSize":       {strconv.Itoa(pageSize)},
		"sortKey":        {"date"},
		"sortDirection":  {"descending"},
		"includeSeries":  {"false"},
		"includeEpisode": {"false"},
		"eventType":      {"3"},
	}
	var result HistoryPage
	if err := c.doJSON(ctx, http.MethodGet, "/api/v3/history", query, nil, &result); err != nil {
		return nil, err
	}
	return result.Records, nil
}

// ImportHistorySince walks newest-first import history until it reaches the
// durable operation boundary. It prevents a busy Sonarr instance from pushing
// the relevant receipt beyond the first page while keeping the query bounded.
func (c *Client) ImportHistorySince(ctx context.Context, since time.Time) ([]HistoryRecord, error) {
	const pageSize = 1000
	const maxPages = 100
	result := make([]HistoryRecord, 0, pageSize)
	for pageNumber := 1; pageNumber <= maxPages; pageNumber++ {
		query := url.Values{
			"page":           {strconv.Itoa(pageNumber)},
			"pageSize":       {strconv.Itoa(pageSize)},
			"sortKey":        {"date"},
			"sortDirection":  {"descending"},
			"includeSeries":  {"false"},
			"includeEpisode": {"false"},
			"eventType":      {"3"},
		}
		var response HistoryPage
		if err := c.doJSON(ctx, http.MethodGet, "/api/v3/history", query, nil, &response); err != nil {
			return nil, err
		}
		for _, record := range response.Records {
			if record.Date.IsZero() {
				return nil, fmt.Errorf("Sonarr import history record %d has no date", record.ID)
			}
			if record.Date.Before(since) {
				return result, nil
			}
			result = append(result, record)
		}
		if len(response.Records) < pageSize || pageNumber*pageSize >= response.TotalRecords {
			return result, nil
		}
	}
	return nil, fmt.Errorf("Sonarr import history since %s exceeds the safety limit of %d records", since.UTC().Format(time.RFC3339), pageSize*maxPages)
}

func (c *Client) EpisodeFiles(ctx context.Context, ids []int) ([]EpisodeFile, error) {
	query := url.Values{}
	for _, id := range ids {
		query.Add("episodeFileIds", strconv.Itoa(id))
	}
	var result []EpisodeFile
	err := c.doJSON(ctx, http.MethodGet, "/api/v3/episodefile", query, nil, &result)
	return result, err
}

func (c *Client) FinalizeQueue(ctx context.Context, queueID int, changeCategory bool) error {
	query := url.Values{
		"removeFromClient": {"false"},
		"blocklist":        {"false"},
		"skipRedownload":   {"false"},
		"changeCategory":   {strconv.FormatBool(changeCategory)},
	}
	return c.doJSON(ctx, http.MethodDelete, "/api/v3/queue/"+strconv.Itoa(queueID), query, nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, query url.Values, body, result any) error {
	requestURL := c.resolve(endpoint)
	if len(query) > 0 {
		requestURL.RawQuery = query.Encode()
	}

	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Sonarr request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), requestBody)
	if err != nil {
		return fmt.Errorf("create Sonarr request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Api-Key", c.apiKey)
	request.Header.Set("User-Agent", "sonarr-torrent-importer")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("Sonarr %s %s failed: %w", method, endpoint, err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return &APIError{
			Method:     method,
			Endpoint:   endpoint,
			StatusCode: response.StatusCode,
			Message:    sanitizeErrorMessage(message),
		}
	}
	if result == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("decode Sonarr %s response: %w", endpoint, err)
	}
	return nil
}

func (c *Client) resolve(endpoint string) *url.URL {
	resolved := cloneURL(c.baseURL)
	resolved.Path = path.Join(c.baseURL.Path, endpoint)
	return resolved
}

func cloneURL(input *url.URL) *url.URL {
	copy := *input
	return &copy
}

func sanitizeErrorMessage(raw []byte) string {
	message := strings.TrimSpace(string(raw))
	if message == "" {
		return "empty response"
	}
	message = strings.ReplaceAll(message, "\n", " ")
	message = strings.ReplaceAll(message, "\r", " ")
	if len(message) > 512 {
		message = message[:512] + "..."
	}
	return message
}

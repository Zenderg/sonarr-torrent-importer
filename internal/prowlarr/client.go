package prowlarr

import (
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

const (
	maxErrorBody    = 8 << 10
	maxTorrentBytes = 16 << 20
)

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
	return fmt.Sprintf("Prowlarr %s %s returned HTTP %d: %s", e.Method, e.Endpoint, e.StatusCode, e.Message)
}

func NewClient(baseURL *url.URL, apiKey string, timeout time.Duration) *Client {
	client := &Client{
		baseURL: cloneURL(baseURL),
		apiKey:  apiKey,
	}
	client.http = &http.Client{
		Timeout: timeout,
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			if !sameOrigin(request.URL, client.baseURL) {
				return http.ErrUseLastResponse
			}
			stripAPIKeyQuery(request.URL)
			request.Header.Set("X-Api-Key", client.apiKey)
			return nil
		},
	}
	return client
}

func (c *Client) Search(ctx context.Context, query string, indexerIDs []int, limit int) ([]Release, error) {
	if len(indexerIDs) == 0 {
		return nil, errors.New("Prowlarr search requires at least one indexer ID")
	}
	if limit <= 0 {
		return nil, errors.New("Prowlarr search limit must be positive")
	}

	values := url.Values{
		"query": {query},
		"type":  {"search"},
		"limit": {strconv.Itoa(limit)},
	}
	for _, indexerID := range indexerIDs {
		if indexerID <= 0 {
			return nil, errors.New("Prowlarr indexer ID must be positive")
		}
		values.Add("indexerIds", strconv.Itoa(indexerID))
	}

	requestURL := c.resolve("/api/v1/search")
	requestURL.RawQuery = values.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, errors.New("create Prowlarr search request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Api-Key", c.apiKey)
	request.Header.Set("User-Agent", "sonarr-torrent-importer")

	response, err := c.http.Do(request)
	if err != nil {
		return nil, errors.New("Prowlarr search request failed")
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return nil, &APIError{
			Method:     http.MethodGet,
			Endpoint:   "/api/v1/search",
			StatusCode: response.StatusCode,
			Message:    sanitizeErrorMessage(message, c.apiKey),
		}
	}

	var releases []Release
	if err := json.NewDecoder(response.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode Prowlarr search response: %w", err)
	}
	return releases, nil
}

func (c *Client) Download(ctx context.Context, downloadURL string) ([]byte, error) {
	requestURL, err := url.Parse(downloadURL)
	if err != nil || !requestURL.IsAbs() || requestURL.User != nil {
		return nil, errors.New("invalid Prowlarr download URL")
	}
	if !sameOrigin(requestURL, c.baseURL) {
		return nil, errors.New("Prowlarr download URL origin does not match configured base URL")
	}

	secrets := []string{c.apiKey, downloadURL}
	for _, values := range requestURL.Query() {
		secrets = append(secrets, values...)
	}
	stripAPIKeyQuery(requestURL)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, errors.New("create Prowlarr download request")
	}
	request.Header.Set("Accept", "application/x-bittorrent")
	request.Header.Set("X-Api-Key", c.apiKey)
	request.Header.Set("User-Agent", "sonarr-torrent-importer")

	response, err := c.http.Do(request)
	if err != nil {
		return nil, errors.New("Prowlarr download request failed")
	}
	defer response.Body.Close()
	if response.Request != nil && response.Request.URL != nil {
		secrets = append(secrets, response.Request.URL.String())
		for _, values := range response.Request.URL.Query() {
			secrets = append(secrets, values...)
		}
	}

	limit := int64(maxTorrentBytes)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		limit = maxErrorBody
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if readErr != nil {
		return nil, errors.New("read Prowlarr download response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &APIError{
			Method:     http.MethodGet,
			Endpoint:   "/download",
			StatusCode: response.StatusCode,
			Message:    sanitizeErrorMessage(responseBody, secrets...),
		}
	}
	if len(responseBody) > maxTorrentBytes {
		return nil, fmt.Errorf("Prowlarr torrent response exceeds %d bytes", maxTorrentBytes)
	}
	return responseBody, nil
}

func (c *Client) resolve(endpoint string) *url.URL {
	resolved := cloneURL(c.baseURL)
	resolved.Path = path.Join(c.baseURL.Path, endpoint)
	resolved.RawQuery = ""
	resolved.Fragment = ""
	return resolved
}

func cloneURL(input *url.URL) *url.URL {
	copy := *input
	return &copy
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func stripAPIKeyQuery(target *url.URL) {
	query := target.Query()
	for key := range query {
		if strings.EqualFold(key, "apikey") {
			query.Del(key)
		}
	}
	target.RawQuery = query.Encode()
}

func sanitizeErrorMessage(raw []byte, secrets ...string) string {
	message := strings.TrimSpace(string(raw))
	if message == "" {
		return "empty response"
	}
	message = strings.ReplaceAll(message, "\n", " ")
	message = strings.ReplaceAll(message, "\r", " ")
	message = sanitizeErrorText(message, secrets...)
	if len(message) > 512 {
		message = message[:512] + "..."
	}
	return message
}

func sanitizeErrorText(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

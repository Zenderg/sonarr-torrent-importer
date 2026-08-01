package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	maxErrorBody   = 8 << 10
	maxSuccessBody = 32 << 20
)

type Client struct {
	baseURL  *url.URL
	username string
	password string
	http     *http.Client
}

type APIError struct {
	Method     string
	Endpoint   string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("qBittorrent %s %s returned HTTP %d: %s", e.Method, e.Endpoint, e.StatusCode, e.Message)
}

func NewClient(baseURL *url.URL, username, password string, timeout time.Duration) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create qBittorrent cookie jar: %w", err)
	}
	return &Client{
		baseURL:  cloneURL(baseURL),
		username: username,
		password: password,
		http: &http.Client{
			Timeout: timeout,
			Jar:     jar,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) > 0 && !sameOrigin(request.URL, via[0].URL) {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
	}, nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func (c *Client) Login(ctx context.Context) error {
	form := url.Values{
		"username": {c.username},
		"password": {c.password},
	}
	body, err := c.do(ctx, http.MethodPost, "/api/v2/auth/login", nil, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	loginResult := strings.TrimSpace(string(body))
	if loginResult != "" && loginResult != "Ok." {
		return errors.New("qBittorrent authentication failed")
	}
	return nil
}

func (c *Client) Versions(ctx context.Context) (Versions, error) {
	application, err := c.do(ctx, http.MethodGet, "/api/v2/app/version", nil, nil, "")
	if err != nil {
		return Versions{}, err
	}
	webAPI, err := c.do(ctx, http.MethodGet, "/api/v2/app/webapiVersion", nil, nil, "")
	if err != nil {
		return Versions{}, err
	}
	return Versions{
		Application: strings.TrimSpace(string(application)),
		WebAPI:      strings.TrimSpace(string(webAPI)),
	}, nil
}

func (c *Client) Torrent(ctx context.Context, hash string) (Torrent, error) {
	query := url.Values{"hashes": {hash}}
	body, err := c.do(ctx, http.MethodGet, "/api/v2/torrents/info", query, nil, "")
	if err != nil {
		return Torrent{}, err
	}
	var torrents []Torrent
	if err := json.Unmarshal(body, &torrents); err != nil {
		return Torrent{}, fmt.Errorf("decode qBittorrent torrent response: %w", err)
	}
	if len(torrents) != 1 {
		if len(torrents) == 0 {
			return Torrent{}, fmt.Errorf("qBittorrent torrent %q was not found", hash)
		}
		return Torrent{}, fmt.Errorf("qBittorrent returned %d torrents for the single hash %q", len(torrents), hash)
	}
	torrent := torrents[0]
	if strings.EqualFold(torrent.Hash, hash) || strings.EqualFold(torrent.InfoHashV1, hash) || strings.EqualFold(torrent.InfoHashV2, hash) {
		return torrent, nil
	}
	return Torrent{}, fmt.Errorf("qBittorrent returned a different torrent identity for hash %q", hash)
}

func (c *Client) Files(ctx context.Context, hash string) ([]File, error) {
	query := url.Values{"hash": {hash}}
	body, err := c.do(ctx, http.MethodGet, "/api/v2/torrents/files", query, nil, "")
	if err != nil {
		return nil, err
	}
	var files []File
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, fmt.Errorf("decode qBittorrent file manifest: %w", err)
	}
	return files, nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, query url.Values, body io.Reader, contentType string) ([]byte, error) {
	requestURL := c.resolve(endpoint)
	if len(query) > 0 {
		requestURL.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create qBittorrent request: %w", err)
	}
	request.Header.Set("Accept", "application/json, text/plain")
	request.Header.Set("User-Agent", "sonarr-torrent-importer")
	request.Header.Set("Referer", c.baseURL.String()+"/")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("qBittorrent %s %s failed: %w", method, endpoint, err)
	}
	defer response.Body.Close()
	limit := int64(maxSuccessBody)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		limit = maxErrorBody
	}
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if readErr != nil {
		return nil, fmt.Errorf("read qBittorrent %s response: %w", endpoint, readErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &APIError{
			Method: method, Endpoint: endpoint, StatusCode: response.StatusCode,
			Message: sanitizeErrorMessage(responseBody),
		}
	}
	if len(responseBody) > maxSuccessBody {
		return nil, fmt.Errorf("qBittorrent %s response exceeds %d bytes", endpoint, maxSuccessBody)
	}
	return responseBody, nil
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

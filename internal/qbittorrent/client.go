package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
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

// AddTorrentPendingError means qBittorrent accepted the add request for
// asynchronous processing but has not exposed a torrent identity yet. Callers
// must observe the expected torrent before deciding whether the add may be
// submitted again.
type AddTorrentPendingError struct {
	SuccessCount    int
	FailureCount    int
	PendingCount    int
	AddedTorrentIDs []string
}

func (e *AddTorrentPendingError) Error() string {
	return fmt.Sprintf("qBittorrent accepted %d torrent add request(s) for asynchronous processing", e.PendingCount)
}

// AddTorrentRejectedError means qBittorrent definitely rejected the one
// submitted torrent. Transport, malformed, and pending responses do not use
// this type because retrying those responses could submit the torrent twice.
type AddTorrentRejectedError struct {
	FailureCount    int
	AddedTorrentIDs []string
}

func (e *AddTorrentRejectedError) Error() string {
	return fmt.Sprintf("qBittorrent rejected %d torrent add request(s)", e.FailureCount)
}

type addTorrentResponse struct {
	SuccessCount    *int      `json:"success_count"`
	FailureCount    *int      `json:"failure_count"`
	PendingCount    *int      `json:"pending_count"`
	AddedTorrentIDs *[]string `json:"added_torrent_ids"`
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
	buildInfoBody, err := c.do(ctx, http.MethodGet, "/api/v2/app/buildInfo", nil, nil, "")
	if err != nil {
		return Versions{}, err
	}
	var buildInfo struct {
		Libtorrent string `json:"libtorrent"`
	}
	if err := json.Unmarshal(buildInfoBody, &buildInfo); err != nil {
		return Versions{}, fmt.Errorf("decode qBittorrent build info: %w", err)
	}
	buildInfo.Libtorrent = strings.TrimSpace(buildInfo.Libtorrent)
	if buildInfo.Libtorrent == "" {
		return Versions{}, errors.New("qBittorrent build info omitted libtorrent version")
	}
	return Versions{
		Application: strings.TrimSpace(string(application)),
		WebAPI:      strings.TrimSpace(string(webAPI)),
		Libtorrent:  buildInfo.Libtorrent,
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

func (c *Client) Properties(ctx context.Context, hash string) (TorrentProperties, error) {
	query := url.Values{"hash": {hash}}
	body, err := c.do(ctx, http.MethodGet, "/api/v2/torrents/properties", query, nil, "")
	if err != nil {
		return TorrentProperties{}, err
	}
	var properties TorrentProperties
	if err := json.Unmarshal(body, &properties); err != nil {
		return TorrentProperties{}, fmt.Errorf("decode qBittorrent torrent properties: %w", err)
	}
	return properties, nil
}

func (c *Client) PieceHashes(ctx context.Context, hash string) ([]string, error) {
	query := url.Values{"hash": {hash}}
	body, err := c.do(ctx, http.MethodGet, "/api/v2/torrents/pieceHashes", query, nil, "")
	if err != nil {
		return nil, err
	}
	var hashes []string
	if err := json.Unmarshal(body, &hashes); err != nil {
		return nil, fmt.Errorf("decode qBittorrent piece hashes: %w", err)
	}
	return hashes, nil
}

func (c *Client) AddTorrent(ctx context.Context, torrent []byte, options AddTorrentOptions) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	torrentPart, err := writer.CreateFormFile("torrents", "revision.torrent")
	if err != nil {
		return fmt.Errorf("create qBittorrent torrent multipart field: %w", err)
	}
	if _, err := torrentPart.Write(torrent); err != nil {
		return fmt.Errorf("write qBittorrent torrent multipart field: %w", err)
	}
	fields := []struct {
		name  string
		value string
	}{
		{name: "savepath", value: options.SavePath},
		{name: "category", value: options.Category},
		{name: "tags", value: options.Tags},
		{name: "stopped", value: "true"},
		{name: "contentLayout", value: "Original"},
		{name: "autoTMM", value: "false"},
		{name: "skip_checking", value: "false"},
	}
	for _, field := range fields {
		if err := writer.WriteField(field.name, field.value); err != nil {
			return fmt.Errorf("write qBittorrent %s multipart field: %w", field.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close qBittorrent torrent multipart body: %w", err)
	}

	response, err := c.do(ctx, http.MethodPost, "/api/v2/torrents/add", nil, bytes.NewReader(body.Bytes()), writer.FormDataContentType())
	if err != nil {
		return err
	}
	return parseAddTorrentResponse(response)
}

func parseAddTorrentResponse(response []byte) error {
	trimmed := strings.TrimSpace(string(response))
	if strings.EqualFold(trimmed, "Ok.") {
		return nil
	}
	if strings.EqualFold(trimmed, "Fails.") {
		return &AddTorrentRejectedError{FailureCount: 1}
	}

	var result addTorrentResponse
	if err := json.Unmarshal(response, &result); err != nil {
		return fmt.Errorf("decode qBittorrent add response: %w", err)
	}
	if result.SuccessCount == nil || result.FailureCount == nil || result.PendingCount == nil || result.AddedTorrentIDs == nil {
		return errors.New("qBittorrent add response omitted required result fields")
	}
	success := *result.SuccessCount
	failure := *result.FailureCount
	pending := *result.PendingCount
	ids := *result.AddedTorrentIDs
	if success < 0 || failure < 0 || pending < 0 {
		return errors.New("qBittorrent add response contained a negative result count")
	}
	if success != len(ids) {
		return fmt.Errorf("qBittorrent add response reported %d successful torrent(s) but returned %d torrent id(s)", success, len(ids))
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return errors.New("qBittorrent add response returned an empty torrent id")
		}
	}
	if success+failure+pending != 1 {
		return fmt.Errorf("qBittorrent add response described %d results for one submitted torrent", success+failure+pending)
	}
	if failure > 0 {
		return &AddTorrentRejectedError{FailureCount: failure, AddedTorrentIDs: append([]string(nil), ids...)}
	}
	if pending > 0 {
		return &AddTorrentPendingError{
			SuccessCount: success, FailureCount: failure, PendingCount: pending,
			AddedTorrentIDs: append([]string(nil), ids...),
		}
	}
	if success != 1 {
		return errors.New("qBittorrent add response did not confirm the submitted torrent")
	}
	return nil
}

func (c *Client) Stop(ctx context.Context, hash string) error {
	return c.postHashes(ctx, "/api/v2/torrents/stop", hash)
}

func (c *Client) Start(ctx context.Context, hash string) error {
	return c.postHashes(ctx, "/api/v2/torrents/start", hash)
}

func (c *Client) SetForceStart(ctx context.Context, hash string, enabled bool) error {
	value := "false"
	if enabled {
		value = "true"
	}
	form := url.Values{"hashes": {hash}, "value": {value}}
	_, err := c.do(ctx, http.MethodPost, "/api/v2/torrents/setForceStart", nil, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	return err
}

func (c *Client) Recheck(ctx context.Context, hash string) error {
	return c.postHashes(ctx, "/api/v2/torrents/recheck", hash)
}

func (c *Client) DeleteTorrentRecord(ctx context.Context, hash string) error {
	form := url.Values{
		"hashes":      {hash},
		"deleteFiles": {"false"},
	}
	_, err := c.do(ctx, http.MethodPost, "/api/v2/torrents/delete", nil, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	return err
}

func (c *Client) postHashes(ctx context.Context, endpoint, hash string) error {
	form := url.Values{"hashes": {hash}}
	_, err := c.do(ctx, http.MethodPost, endpoint, nil, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	return err
}

func (c *Client) RenameFile(ctx context.Context, hash, oldPath, newPath string) error {
	form := url.Values{
		"hash":    {hash},
		"oldPath": {oldPath},
		"newPath": {newPath},
	}
	_, err := c.do(ctx, http.MethodPost, "/api/v2/torrents/renameFile", nil, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	return err
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

package qbittorrent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuthenticatedManifestContract(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v2/auth/login":
			if err := request.ParseForm(); err != nil {
				return nil, err
			}
			if request.Form.Get("username") != "user" || request.Form.Get("password") != "password" {
				t.Error("unexpected login credentials")
			}
			return textResponse("Ok.", "custom-session=authenticated; Path=/"), nil
		case "/api/v2/app/version":
			requireSession(t, request)
			return textResponse("v5.0.0", ""), nil
		case "/api/v2/app/webapiVersion":
			requireSession(t, request)
			return textResponse("2.11.3", ""), nil
		case "/api/v2/app/buildInfo":
			requireSession(t, request)
			return textResponse(`{"libtorrent":"2.0.11.0"}`, ""), nil
		case "/api/v2/torrents/files":
			requireSession(t, request)
			if request.URL.Query().Get("hash") != "abc" {
				t.Errorf("got hash %q", request.URL.Query().Get("hash"))
			}
			return textResponse(`[{"index":0,"name":"[03].mkv","size":100,"progress":1,"priority":1,"availability":0.5}]`, ""), nil
		case "/api/v2/torrents/renameFile":
			requireSession(t, request)
			if err := request.ParseForm(); err != nil {
				return nil, err
			}
			if request.Form.Get("hash") != "abc" || request.Form.Get("oldPath") != "[03].mkv" || request.Form.Get("newPath") != "Clockwork.Garden.S02E03.WEBDL-1080p.mkv" {
				t.Errorf("unexpected rename form: %v", request.Form)
			}
			return textResponse("", ""), nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("not found"))}, nil
		}
	})

	baseURL, _ := url.Parse("http://qbittorrent.test")
	client, err := NewClient(baseURL, "user", "password", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = transport
	if err := client.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	versions, err := client.Versions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if versions.Application != "v5.0.0" || versions.WebAPI != "2.11.3" || versions.Libtorrent != "2.0.11.0" {
		t.Fatalf("unexpected versions: %+v", versions)
	}
	files, err := client.Files(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Index != 0 || files[0].Priority != 1 || files[0].Progress != 1 {
		t.Fatalf("unexpected manifest: %+v", files)
	}
	if err := client.RenameFile(context.Background(), "abc", "[03].mkv", "Clockwork.Garden.S02E03.WEBDL-1080p.mkv"); err != nil {
		t.Fatal(err)
	}
}

func TestRenameFilePreservesConflictResponse(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusConflict,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("newPath already in use")),
		}, nil
	})
	baseURL, _ := url.Parse("http://qbittorrent.test")
	client, err := NewClient(baseURL, "user", "password", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = transport
	err = client.RenameFile(context.Background(), "abc", "Release/[01].mkv", "Release/S01E01.mkv")
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusConflict {
		t.Fatalf("rename conflict = %v, want typed HTTP 409", err)
	}
}

func TestRollingObservationContract(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Query().Get("hash") != "abc" {
			t.Errorf("unexpected observation request: %s %s", request.Method, request.URL.String())
		}
		switch request.URL.Path {
		case "/api/v2/torrents/properties":
			return textResponse(`{"piece_size":1048576,"pieces_have":12,"pieces_num":13,"completion_date":1720000000}`, ""), nil
		case "/api/v2/torrents/pieceHashes":
			return textResponse(`["0011","2233"]`, ""), nil
		default:
			return nil, errors.New("unexpected endpoint")
		}
	})
	client := newTestClient(t, transport)

	properties, err := client.Properties(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if properties.PieceSize != 1048576 || properties.PiecesHave != 12 || properties.PiecesNum != 13 || properties.CompletionDate != 1720000000 {
		t.Fatalf("unexpected properties: %+v", properties)
	}
	hashes, err := client.PieceHashes(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 2 || hashes[0] != "0011" || hashes[1] != "2233" {
		t.Fatalf("unexpected piece hashes: %v", hashes)
	}
}

func TestAddTorrentUsesExactBytesAndStoppedOptions(t *testing.T) {
	metainfo := []byte{'d', '4', ':', 'i', 'n', 'f', 'o', 'd', 0, 0xff, 'e', 'e'}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v2/torrents/add" {
			t.Fatalf("unexpected add request: %s %s", request.Method, request.URL.Path)
		}
		reader, err := request.MultipartReader()
		if err != nil {
			return nil, err
		}
		parts := make(map[string][][]byte)
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			contents, err := io.ReadAll(part)
			if err != nil {
				return nil, err
			}
			if part.FormName() == "torrents" && part.FileName() != "revision.torrent" {
				t.Errorf("unexpected torrent filename %q", part.FileName())
			}
			parts[part.FormName()] = append(parts[part.FormName()], contents)
		}

		expected := map[string][]byte{
			"torrents":      metainfo,
			"savepath":      []byte("/media/downloads"),
			"category":      []byte("tv-sonarr"),
			"tags":          []byte("rolling, managed"),
			"stopped":       []byte("true"),
			"contentLayout": []byte("Original"),
			"autoTMM":       []byte("false"),
			"skip_checking": []byte("false"),
		}
		if len(parts) != len(expected) {
			t.Errorf("multipart fields = %v, want exactly %v", parts, expected)
		}
		for name, want := range expected {
			values := parts[name]
			if len(values) != 1 || !bytes.Equal(values[0], want) {
				t.Errorf("multipart field %q = %q, want %q", name, values, want)
			}
		}
		if _, exists := parts["paused"]; exists {
			t.Error("v5 add request must use stopped, not paused")
		}
		if _, exists := parts["root_folder"]; exists {
			t.Error("v5 add request must use contentLayout, not root_folder")
		}
		return textResponse(`{"success_count":1,"failure_count":0,"pending_count":0,"added_torrent_ids":["abc"]}`, ""), nil
	})
	client := newTestClient(t, transport)

	err := client.AddTorrent(context.Background(), metainfo, AddTorrentOptions{
		SavePath: "/media/downloads",
		Category: "tv-sonarr",
		Tags:     "rolling, managed",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRollingMutationForms(t *testing.T) {
	seen := make(map[string]bool)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Errorf("mutation method = %s, want POST", request.Method)
		}
		if err := request.ParseForm(); err != nil {
			return nil, err
		}
		if request.Form.Get("hashes") != "abc" {
			t.Errorf("hashes = %q, want abc", request.Form.Get("hashes"))
		}
		switch request.URL.Path {
		case "/api/v2/torrents/stop", "/api/v2/torrents/start", "/api/v2/torrents/recheck":
			if request.Form.Has("deleteFiles") {
				t.Errorf("unexpected deleteFiles on %s", request.URL.Path)
			}
		case "/api/v2/torrents/delete":
			if request.Form.Get("deleteFiles") != "false" {
				t.Errorf("deleteFiles = %q, want hardcoded false", request.Form.Get("deleteFiles"))
			}
		case "/api/v2/torrents/setForceStart":
			if request.Form.Get("value") != "true" {
				t.Errorf("force-start value = %q, want true", request.Form.Get("value"))
			}
		default:
			return nil, errors.New("unexpected mutation endpoint")
		}
		seen[request.URL.Path] = true
		return textResponse("", ""), nil
	})
	client := newTestClient(t, transport)
	mutations := []func(context.Context, string) error{
		client.Stop,
		client.Start,
		client.Recheck,
		client.DeleteTorrentRecord,
	}
	for _, mutation := range mutations {
		if err := mutation(context.Background(), "abc"); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.SetForceStart(context.Background(), "abc", true); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/api/v2/torrents/stop", "/api/v2/torrents/start", "/api/v2/torrents/recheck", "/api/v2/torrents/delete", "/api/v2/torrents/setForceStart"} {
		if !seen[endpoint] {
			t.Errorf("endpoint %s was not called", endpoint)
		}
	}
}

func TestAddTorrentLegacyResponseCompatibility(t *testing.T) {
	for _, test := range []struct {
		name       string
		response   string
		wantError  bool
		wantReject bool
	}{
		{name: "success", response: "Ok."},
		{name: "failure", response: "Fails.", wantError: true, wantReject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return textResponse(test.response, ""), nil
			}))
			err := client.AddTorrent(context.Background(), []byte("de"), AddTorrentOptions{})
			if (err != nil) != test.wantError {
				t.Fatalf("AddTorrent error = %v, wantError %t", err, test.wantError)
			}
			var rejected *AddTorrentRejectedError
			if errors.As(err, &rejected) != test.wantReject {
				t.Fatalf("AddTorrent rejection error = %v, wantReject %t", err, test.wantReject)
			}
		})
	}
}

func TestAddTorrentStructuredResponseContract(t *testing.T) {
	tests := []struct {
		name        string
		response    string
		status      int
		wantError   bool
		wantPending bool
		wantReject  bool
	}{
		{
			name:     "success",
			response: `{"success_count":1,"failure_count":0,"pending_count":0,"added_torrent_ids":["abc"]}`,
			status:   http.StatusOK,
		},
		{
			name:       "failure",
			response:   `{"success_count":0,"failure_count":1,"pending_count":0,"added_torrent_ids":[]}`,
			status:     http.StatusOK,
			wantError:  true,
			wantReject: true,
		},
		{
			name:        "pending",
			response:    `{"success_count":0,"failure_count":0,"pending_count":1,"added_torrent_ids":[]}`,
			status:      http.StatusAccepted,
			wantError:   true,
			wantPending: true,
		},
		{
			name:      "missing required field",
			response:  `{"success_count":1,"failure_count":0,"pending_count":0}`,
			status:    http.StatusOK,
			wantError: true,
		},
		{
			name:      "success count and ids disagree",
			response:  `{"success_count":1,"failure_count":0,"pending_count":0,"added_torrent_ids":[]}`,
			status:    http.StatusOK,
			wantError: true,
		},
		{
			name:      "unexpected number of results",
			response:  `{"success_count":2,"failure_count":0,"pending_count":0,"added_torrent_ids":["abc","def"]}`,
			status:    http.StatusOK,
			wantError: true,
		},
		{
			name:      "unknown response",
			response:  "accepted",
			status:    http.StatusOK,
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				response := textResponse(test.response, "")
				response.StatusCode = test.status
				return response, nil
			}))
			err := client.AddTorrent(context.Background(), []byte("de"), AddTorrentOptions{})
			if (err != nil) != test.wantError {
				t.Fatalf("AddTorrent error = %v, wantError %t", err, test.wantError)
			}
			var pending *AddTorrentPendingError
			if errors.As(err, &pending) != test.wantPending {
				t.Fatalf("AddTorrent pending error = %v, wantPending %t", err, test.wantPending)
			}
			if pending != nil && (pending.SuccessCount != 0 || pending.FailureCount != 0 || pending.PendingCount != 1 || len(pending.AddedTorrentIDs) != 0) {
				t.Fatalf("unexpected pending result: %+v", pending)
			}
			var rejected *AddTorrentRejectedError
			if errors.As(err, &rejected) != test.wantReject {
				t.Fatalf("AddTorrent rejection error = %v, wantReject %t", err, test.wantReject)
			}
		})
	}
}

func newTestClient(t *testing.T, transport http.RoundTripper) *Client {
	t.Helper()
	baseURL, err := url.Parse("http://qbittorrent.test")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(baseURL, "user", "password", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = transport
	return client
}

func requireSession(t *testing.T, request *http.Request) {
	t.Helper()
	cookie, err := request.Cookie("custom-session")
	if err != nil || cookie.Value != "authenticated" {
		t.Fatalf("missing authenticated cookie: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func textResponse(body, cookie string) *http.Response {
	header := http.Header{"Content-Type": {"text/plain"}}
	if cookie != "" {
		header.Set("Set-Cookie", cookie)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

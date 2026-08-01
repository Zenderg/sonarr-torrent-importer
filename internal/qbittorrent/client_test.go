package qbittorrent

import (
	"context"
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
		case "/api/v2/torrents/files":
			requireSession(t, request)
			if request.URL.Query().Get("hash") != "abc" {
				t.Errorf("got hash %q", request.URL.Query().Get("hash"))
			}
			return textResponse(`[{"index":0,"name":"[03].mkv","size":100,"progress":1,"priority":1,"availability":0.5}]`, ""), nil
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
	if versions.Application != "v5.0.0" || versions.WebAPI != "2.11.3" {
		t.Fatalf("unexpected versions: %+v", versions)
	}
	files, err := client.Files(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Index != 0 || files[0].Priority != 1 || files[0].Progress != 1 {
		t.Fatalf("unexpected manifest: %+v", files)
	}
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

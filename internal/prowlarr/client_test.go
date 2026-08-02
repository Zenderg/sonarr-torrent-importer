package prowlarr

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSearchContract(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/prowlarr/api/v1/search" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("query") != "Show Name S01" || query.Get("type") != "search" || query.Get("limit") != "100" {
			t.Fatalf("unexpected search query: %s", request.URL.RawQuery)
		}
		if got := query["indexerIds"]; !reflect.DeepEqual(got, []string{"7", "9"}) {
			t.Fatalf("unexpected indexer IDs: %v", got)
		}
		if request.Header.Get("X-Api-Key") != "prowlarr-secret" {
			t.Fatal("missing Prowlarr API key header")
		}
		return response(http.StatusOK, `[{"guid":"topic-1","size":1234,"files":2,"indexerId":7,"indexer":"Tracker","title":"Show.Name.S01","publishDate":"2026-08-02T10:00:00Z","downloadUrl":"http://prowlarr.test/prowlarr/7/download?apikey=secret&link=opaque","infoUrl":"https://tracker.test/topic/1","categories":[{"id":5000,"name":"TV"}],"magnetUrl":"","infoHash":"ABCDEF","seeders":10,"leechers":2,"protocol":"torrent"}]`), nil
	})

	baseURL, _ := url.Parse("http://prowlarr.test/prowlarr")
	client := NewClient(baseURL, "prowlarr-secret", time.Second)
	client.http.Transport = transport
	releases, err := client.Search(context.Background(), "Show Name S01", []int{7, 9}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 {
		t.Fatalf("unexpected releases: %+v", releases)
	}
	release := releases[0]
	if release.GUID != "topic-1" || release.IndexerID != 7 || release.Protocol != "torrent" || release.InfoHash != "ABCDEF" {
		t.Fatalf("unexpected release: %+v", release)
	}
	if release.Files == nil || *release.Files != 2 || release.Seeders == nil || *release.Seeders != 10 {
		t.Fatalf("missing optional release fields: %+v", release)
	}
}

func TestDownloadStripsAPIKeyAndReturnsTorrentBytes(t *testing.T) {
	want := []byte("d4:infod4:name4:testee")
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/prowlarr/7/download" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		if request.URL.Query().Get("apikey") != "" || strings.Contains(strings.ToLower(request.URL.RawQuery), "apikey") {
			t.Fatalf("API key remained in download URL: %s", request.URL.RawQuery)
		}
		if request.URL.Query().Get("link") != "opaque-link" || request.URL.Query().Get("file") != "Show.Name.S01" {
			t.Fatalf("download locator was changed: %s", request.URL.RawQuery)
		}
		if request.Header.Get("X-Api-Key") != "configured-secret" {
			t.Fatal("missing Prowlarr API key header")
		}
		if request.Header.Get("Accept") != "application/x-bittorrent" {
			t.Fatalf("unexpected Accept header: %q", request.Header.Get("Accept"))
		}
		return binaryResponse(http.StatusOK, want), nil
	})

	baseURL, _ := url.Parse("http://prowlarr.test/prowlarr")
	client := NewClient(baseURL, "configured-secret", time.Second)
	client.http.Transport = transport
	got, err := client.Download(context.Background(), "http://prowlarr.test/prowlarr/7/download?apikey=url-secret&link=opaque-link&file=Show.Name.S01")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected torrent bytes: %q", got)
	}
}

func TestDownloadRejectsCrossOrigin(t *testing.T) {
	baseURL, _ := url.Parse("http://prowlarr.test")
	client := NewClient(baseURL, "configured-secret", time.Second)
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("cross-origin request was sent to %s", request.URL)
		return nil, nil
	})

	_, err := client.Download(context.Background(), "http://tracker.test/download?apikey=url-secret")
	if err == nil {
		t.Fatal("expected cross-origin URL to be rejected")
	}
	if strings.Contains(err.Error(), "url-secret") || strings.Contains(err.Error(), "configured-secret") {
		t.Fatalf("secret leaked in error: %v", err)
	}
}

func TestDownloadEnforcesTorrentSizeLimit(t *testing.T) {
	baseURL, _ := url.Parse("http://prowlarr.test")
	client := NewClient(baseURL, "secret", time.Second)
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := io.LimitReader(zeroReader{}, maxTorrentBytes+1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(body)}, nil
	})

	_, err := client.Download(context.Background(), "http://prowlarr.test/7/download?link=opaque")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

func TestErrorsRedactSecrets(t *testing.T) {
	baseURL, _ := url.Parse("http://prowlarr.test")
	client := NewClient(baseURL, "configured-secret", time.Second)
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(http.StatusBadGateway, `failed apikey=configured-secret link=opaque-link url-secret`), nil
	})

	_, err := client.Download(context.Background(), "http://prowlarr.test/7/download?apikey=url-secret&link=opaque-link")
	if err == nil {
		t.Fatal("expected download error")
	}
	for _, secret := range []string{"configured-secret", "url-secret", "opaque-link"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("secret %q leaked in error: %v", secret, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = 0
	}
	return len(buffer), nil
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func binaryResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/x-bittorrent"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

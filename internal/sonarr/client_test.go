package sonarr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestManualImportReprocessContract(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v3/manualimport" || request.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("X-Api-Key") != "test-api-key" {
			t.Error("missing Sonarr API key header")
		}
		var items []ManualImportReprocess
		if err := json.NewDecoder(request.Body).Decode(&items); err != nil {
			return nil, err
		}
		if len(items) != 1 || len(items[0].EpisodeIDs) != 1 || items[0].EpisodeIDs[0] != 456 || items[0].SeriesID != 123 {
			t.Fatalf("unexpected reprocess request: %+v", items)
		}
		items[0].Episodes = []Episode{{ID: 456, SeriesID: 123, SeasonNumber: 2, EpisodeNumber: 3}}
		items[0].EpisodeIDs = nil
		encoded, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		return jsonResponse(encoded), nil
	})

	baseURL, _ := url.Parse("http://sonarr.test")
	client := NewClient(baseURL, "test-api-key", time.Second)
	client.http.Transport = transport
	season := 2
	response, err := client.Reprocess(context.Background(), []ManualImportReprocess{{
		Path: "/downloads/Clockwork Garden/[03].mkv", SeriesID: 123,
		SeasonNumber: &season, EpisodeIDs: []int{456},
		Quality:     json.RawMessage(`{"quality":{"id":1}}`),
		Languages:   json.RawMessage(`[{"id":1}]`),
		ReleaseType: json.RawMessage(`"seasonPack"`),
		DownloadID:  "0123456789012345678901234567890123456789",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response) != 1 || len(response[0].Episodes) != 1 || response[0].Episodes[0].ID != 456 {
		t.Fatalf("unexpected reprocess response: %+v", response)
	}
}

func TestQueueUsesDetailsEndpoint(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v3/queue/details" {
			t.Errorf("got path %q", request.URL.Path)
		}
		if request.URL.Query().Get("includeSeries") != "true" || request.URL.Query().Get("includeEpisode") != "true" {
			t.Errorf("missing queue include flags: %s", request.URL.RawQuery)
		}
		return jsonResponse([]byte(`[{"id":11,"downloadId":"abc","protocol":"torrent"}]`)), nil
	})

	baseURL, _ := url.Parse("http://sonarr.test")
	client := NewClient(baseURL, "key", time.Second)
	client.http.Transport = transport
	records, err := client.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != 11 {
		t.Fatalf("unexpected queue records: %+v", records)
	}
}

func TestManualImportCandidateDiscoveryOmitsUnknownDownloadID(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v3/manualimport" || request.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		if _, exists := request.URL.Query()["downloadId"]; exists {
			t.Errorf("empty downloadId must be omitted, got query %s", request.URL.RawQuery)
		}
		if request.URL.Query().Get("folder") != "/downloads/rolling/revision" || request.URL.Query().Get("filterExistingFiles") != "true" {
			t.Errorf("unexpected candidate query %s", request.URL.RawQuery)
		}
		return jsonResponse([]byte(`[]`)), nil
	})

	baseURL, _ := url.Parse("http://sonarr.test")
	client := NewClient(baseURL, "key", time.Second)
	client.http.Transport = transport
	if _, err := client.ManualImportCandidates(context.Background(), "/downloads/rolling/revision", ""); err != nil {
		t.Fatal(err)
	}
}

func TestStartManualImportWithExplicitCopyMode(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v3/command" || request.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		var command ManualImportCommand
		if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
			return nil, err
		}
		if command.Name != "ManualImport" || command.ImportMode != "copy" || len(command.Files) != 1 {
			t.Fatalf("unexpected ManualImport command: %+v", command)
		}
		return jsonResponse([]byte(`{"id":42,"status":"queued"}`)), nil
	})

	baseURL, _ := url.Parse("http://sonarr.test")
	client := NewClient(baseURL, "key", time.Second)
	client.http.Transport = transport
	command, err := client.StartManualImportWithMode(context.Background(), []ManualImportFile{{Path: "/downloads/episode.mkv"}}, "copy")
	if err != nil {
		t.Fatal(err)
	}
	if command.ID != 42 {
		t.Fatalf("unexpected command: %+v", command)
	}
	if _, err := client.StartManualImportWithMode(context.Background(), nil, "move"); err == nil {
		t.Fatal("unsupported ManualImport mode was accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jsonResponse(body []byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

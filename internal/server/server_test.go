package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthRouteSupportsGo121MuxSemantics(t *testing.T) {
	handler := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", time.Minute).Handler()

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /health returned %d, want 200", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/health", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST /health returned status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}
}

func TestExecuteRequiresDryRunPlanToken(t *testing.T) {
	handler := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "test", time.Minute).Handler()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/imports/execute",
		strings.NewReader(`{"downloadId":"0123456789012345678901234567890123456789","confirmDownloadId":"0123456789012345678901234567890123456789"}`),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "planToken") {
		t.Fatalf("execute without planToken returned status=%d body=%s", response.Code, response.Body.String())
	}
}

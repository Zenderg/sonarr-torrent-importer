package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/workflow"
)

const maxRequestBody = 1 << 20

type workflowRunner interface {
	Run(context.Context, workflow.Selection, bool, string) (workflow.Result, error)
}

type workflowStatusReader interface {
	LatestResult() (workflow.Result, bool, error)
}

type Server struct {
	engine          workflowRunner
	logger          *slog.Logger
	version         string
	workflowTimeout time.Duration
	running         atomic.Bool
	lastMu          sync.RWMutex
	last            *workflow.Result
	lastErr         string
}

type importRequest struct {
	DownloadID        string `json:"downloadId"`
	QueueID           int    `json:"queueId"`
	ConfirmDownloadID string `json:"confirmDownloadId,omitempty"`
	PlanToken         string `json:"planToken,omitempty"`
}

type statusResponse struct {
	Running bool             `json:"running"`
	Latest  *workflow.Result `json:"latest,omitempty"`
	Error   string           `json:"error,omitempty"`
}

func New(engine workflowRunner, logger *slog.Logger, version string, workflowTimeout time.Duration) *Server {
	server := &Server{engine: engine, logger: logger, version: version, workflowTimeout: workflowTimeout}
	if reader, ok := engine.(workflowStatusReader); ok {
		latest, exists, err := reader.LatestResult()
		if err != nil {
			server.lastErr = fmt.Sprintf("load durable operation status: %v", err)
			logger.Error("durable operation status is unreadable")
		} else if exists {
			latestCopy := latest
			server.last = &latestCopy
		}
	}
	return server
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", requireMethod(http.MethodGet, s.health))
	mux.HandleFunc("/api/v1/status", requireMethod(http.MethodGet, s.status))
	mux.HandleFunc("/api/v1/imports/dry-run", requireMethod(http.MethodPost, s.importDryRun))
	mux.HandleFunc("/api/v1/imports/execute", requireMethod(http.MethodPost, s.importExecute))
	return s.recoverPanics(mux)
}

func (s *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) status(writer http.ResponseWriter, _ *http.Request) {
	s.lastMu.RLock()
	defer s.lastMu.RUnlock()
	writeJSON(writer, http.StatusOK, statusResponse{Running: s.running.Load(), Latest: s.last, Error: s.lastErr})
}

func (s *Server) importDryRun(writer http.ResponseWriter, request *http.Request) {
	s.runImport(writer, request, false)
}

func (s *Server) importExecute(writer http.ResponseWriter, request *http.Request) {
	s.runImport(writer, request, true)
}

func (s *Server) runImport(writer http.ResponseWriter, request *http.Request, execute bool) {
	var payload importRequest
	if err := decodeRequest(writer, request, &payload); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if payload.DownloadID == "" && payload.QueueID <= 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "downloadId or queueId is required"})
		return
	}
	if execute && (payload.DownloadID == "" || payload.ConfirmDownloadID != payload.DownloadID) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "execute requires confirmDownloadId to exactly match downloadId"})
		return
	}
	if execute && payload.PlanToken == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "execute requires planToken from the matching dry-run"})
		return
	}
	if !s.running.CompareAndSwap(false, true) {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "another import workflow is already running"})
		return
	}
	defer s.running.Store(false)

	selection := workflow.Selection{DownloadID: payload.DownloadID, QueueID: payload.QueueID}
	workflowContext, cancel := context.WithTimeout(context.Background(), s.workflowTimeout)
	defer cancel()
	result, err := s.engine.Run(workflowContext, selection, execute, payload.PlanToken)
	s.lastMu.Lock()
	resultCopy := result
	s.last = &resultCopy
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
	}
	s.lastMu.Unlock()

	mode := "dry-run"
	if execute {
		mode = "execute"
	}
	s.logger.Info("workflow finished", "mode", mode, "outcome", result.Outcome, "error", err != nil)
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{"error": err.Error(), "result": result})
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func requireMethod(method string, handler http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != method {
			writer.Header().Set("Allow", method)
			writeJSON(writer, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		handler(writer, request)
	}
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("HTTP handler panic", "method", request.Method, "path", request.URL.Path)
				writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
			}
		}()
		next.ServeHTTP(writer, request)
	})
}

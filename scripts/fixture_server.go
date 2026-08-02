package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	upstream, err := url.Parse("http://qbittorrent:8080")
	if err != nil {
		log.Fatal(err)
	}
	delay, err := time.ParseDuration(os.Getenv("QBIT_RENAME_RESPONSE_DELAY"))
	if err != nil || delay <= 0 {
		log.Fatal("QBIT_RENAME_RESPONSE_DELAY must be a positive duration")
	}
	addDelay, err := time.ParseDuration(os.Getenv("QBIT_ADD_RESPONSE_DELAY"))
	if err != nil || addDelay < 0 {
		log.Fatal("QBIT_ADD_RESPONSE_DELAY must be a non-negative duration")
	}
	deleteDelay, err := time.ParseDuration(os.Getenv("QBIT_DELETE_RESPONSE_DELAY"))
	if err != nil || deleteDelay < 0 {
		log.Fatal("QBIT_DELETE_RESPONSE_DELAY must be a non-negative duration")
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var renameRequests atomic.Int64
	var addRequests atomic.Int64
	var deleteRequests atomic.Int64
	var rollingRevision atomic.Int64
	rollingRevision.Store(1)
	mux := http.NewServeMux()
	mux.HandleFunc("/qbittorrent/", func(writer http.ResponseWriter, request *http.Request) {
		proxied := request.Clone(request.Context())
		proxied.URL.Path = "/" + strings.TrimPrefix(request.URL.Path, "/qbittorrent/")
		if proxied.URL.Path != "/api/v2/torrents/renameFile" && proxied.URL.Path != "/api/v2/torrents/add" && proxied.URL.Path != "/api/v2/torrents/delete" {
			proxy.ServeHTTP(writer, proxied)
			return
		}
		responseDelay := delay
		switch proxied.URL.Path {
		case "/api/v2/torrents/add":
			addRequests.Add(1)
			responseDelay = addDelay
		case "/api/v2/torrents/delete":
			deleteRequests.Add(1)
			responseDelay = deleteDelay
		default:
			renameRequests.Add(1)
		}
		buffer := httptest.NewRecorder()
		proxy.ServeHTTP(buffer, proxied)
		select {
		case <-time.After(responseDelay):
		case <-request.Context().Done():
			return
		}
		for name, values := range buffer.Header() {
			for _, value := range values {
				writer.Header().Add(name, value)
			}
		}
		writer.WriteHeader(buffer.Code)
		_, _ = writer.Write(buffer.Body.Bytes())
	})
	mux.HandleFunc("/metrics", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]int64{
			"renameRequests": renameRequests.Load(),
			"addRequests":    addRequests.Load(),
			"deleteRequests": deleteRequests.Load(),
		})
	})
	mux.HandleFunc("/api/v1/search", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Query().Get("type") != "search" || request.URL.Query().Get("indexerIds") != "1" {
			http.Error(writer, "invalid fixture search", http.StatusBadRequest)
			return
		}
		revision := rollingRevision.Load()
		torrentPath := filepath.Join("/fixtures", "rolling-rev"+strconv.FormatInt(revision, 10)+".torrent")
		if _, err := os.Stat(torrentPath); err != nil {
			http.Error(writer, "rolling fixture is unavailable", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode([]map[string]any{{
			"guid": "fixture:rolling-futurama-s01", "indexerId": 1, "indexer": "Local rolling fixture",
			"title": "Futurama.S01.Rolling.720p.WEB-DL-GROUP", "publishDate": time.Now().UTC(),
			"downloadUrl": "http://fixture-server:8080/1/download?apikey=fixture-secret&link=rolling",
			"size":        0, "protocol": "torrent",
		}})
	})
	mux.HandleFunc("/1/download", func(writer http.ResponseWriter, request *http.Request) {
		revision := rollingRevision.Load()
		writer.Header().Set("Content-Type", "application/x-bittorrent")
		http.ServeFile(writer, request, filepath.Join("/fixtures", "rolling-rev"+strconv.FormatInt(revision, 10)+".torrent"))
	})
	mux.HandleFunc("/advance", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rollingRevision.Store(2)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]int64{"revision": 2})
	})
	mux.Handle("/", http.FileServer(http.Dir("/fixtures")))
	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServe())
}

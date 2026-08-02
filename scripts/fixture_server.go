package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
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
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var renameRequests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/qbittorrent/", func(writer http.ResponseWriter, request *http.Request) {
		proxied := request.Clone(request.Context())
		proxied.URL.Path = "/" + strings.TrimPrefix(request.URL.Path, "/qbittorrent/")
		if proxied.URL.Path != "/api/v2/torrents/renameFile" {
			proxy.ServeHTTP(writer, proxied)
			return
		}
		renameRequests.Add(1)
		buffer := httptest.NewRecorder()
		proxy.ServeHTTP(buffer, proxied)
		select {
		case <-time.After(delay):
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
		_ = json.NewEncoder(writer).Encode(map[string]int64{"renameRequests": renameRequests.Load()})
	})
	mux.Handle("/", http.FileServer(http.Dir("/fixtures")))
	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServe())
}

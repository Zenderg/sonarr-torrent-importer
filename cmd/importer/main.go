package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zenderg/sonarr-torrent-importer/internal/config"
	"github.com/zenderg/sonarr-torrent-importer/internal/qbittorrent"
	"github.com/zenderg/sonarr-torrent-importer/internal/server"
	"github.com/zenderg/sonarr-torrent-importer/internal/sonarr"
	"github.com/zenderg/sonarr-torrent-importer/internal/workflow"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "version" {
		fmt.Printf("sonarr-torrent-importer %s (commit %s, built %s)\n", version, commit, buildTime)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	engine, err := newEngine(cfg)
	if err != nil {
		return err
	}

	switch command {
	case "serve":
		return serve(cfg, engine)
	case "run":
		return runOnce(engine, cfg.WorkflowTimeout, os.Args[2:])
	default:
		return fmt.Errorf("unknown command %q; expected serve, run, or version", command)
	}
}

func newEngine(cfg config.Config) (*workflow.Engine, error) {
	sonarrClient := sonarr.NewClient(cfg.SonarrURL, cfg.SonarrAPIKey, cfg.RequestTimeout)
	qbitClient, err := qbittorrent.NewClient(cfg.QBittorrentURL, cfg.QBittorrentUsername, cfg.QBittorrentPassword, cfg.RequestTimeout)
	if err != nil {
		return nil, err
	}
	return workflow.NewEngine(sonarrClient, qbitClient, cfg.CommandTimeout, cfg.WorkflowTimeout, cfg.PollInterval, cfg.DataRoot)
}

func runOnce(engine *workflow.Engine, workflowTimeout time.Duration, arguments []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	downloadID := flags.String("download-id", "", "Sonarr/qBittorrent download ID")
	queueID := flags.Int("queue-id", 0, "Sonarr queue item ID")
	execute := flags.Bool("execute", false, "execute the verified import plan")
	confirmDownloadID := flags.String("confirm-download-id", "", "exact download ID confirmation required with --execute")
	planToken := flags.String("plan-token", "", "exact dry-run plan token required with --execute")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("run does not accept positional arguments")
	}
	if *downloadID == "" && *queueID <= 0 {
		return errors.New("--download-id or --queue-id is required")
	}
	if *execute && (*downloadID == "" || *confirmDownloadID != *downloadID) {
		return errors.New("--execute requires --confirm-download-id to exactly match --download-id")
	}
	if *execute && *planToken == "" {
		return errors.New("--execute requires --plan-token from the matching dry-run")
	}

	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go func() {
		<-signalContext.Done()
		stopSignals()
	}()
	ctx, cancel := context.WithTimeout(signalContext, workflowTimeout)
	defer cancel()
	result, err := engine.Run(ctx, workflow.Selection{DownloadID: *downloadID, QueueID: *queueID}, *execute, *planToken)
	if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
		return encodeErr
	}
	return err
}

func serve(cfg config.Config, engine *workflow.Engine) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	application := server.New(engine, logger, version, cfg.WorkflowTimeout)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddress(),
		Handler:           application.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("HTTP server starting", "address", cfg.ListenAddress(), "version", version)
		serverErrors <- httpServer.ListenAndServe()
	}()

	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signals.Done():
		logger.Info("shutdown requested")
		stop()
	}

	shutdownTimeout := cfg.WorkflowTimeout + cfg.RequestTimeout
	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}

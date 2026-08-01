package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host                string
	Port                int
	DataRoot            string
	SonarrURL           *url.URL
	SonarrAPIKey        string
	QBittorrentURL      *url.URL
	QBittorrentUsername string
	QBittorrentPassword string
	RequestTimeout      time.Duration
	CommandTimeout      time.Duration
	WorkflowTimeout     time.Duration
	PollInterval        time.Duration
}

func Load() (Config, error) {
	var cfg Config
	var err error

	cfg.Host = envOrDefault("HOST", "0.0.0.0")
	cfg.DataRoot = envOrDefault("DATA_ROOT", "/data")
	cfg.SonarrAPIKey = strings.TrimSpace(os.Getenv("SONARR_API_KEY"))
	cfg.QBittorrentUsername = strings.TrimSpace(os.Getenv("QBITTORRENT_USERNAME"))
	cfg.QBittorrentPassword = os.Getenv("QBITTORRENT_PASSWORD")

	cfg.Port, err = parseInt("PORT", envOrDefault("PORT", "8080"), 1, 65535)
	if err != nil {
		return Config{}, err
	}
	cfg.SonarrURL, err = parseBaseURL("SONARR_URL", os.Getenv("SONARR_URL"))
	if err != nil {
		return Config{}, err
	}
	cfg.QBittorrentURL, err = parseBaseURL("QBITTORRENT_URL", os.Getenv("QBITTORRENT_URL"))
	if err != nil {
		return Config{}, err
	}
	cfg.RequestTimeout, err = parseDuration("REQUEST_TIMEOUT", envOrDefault("REQUEST_TIMEOUT", "30s"))
	if err != nil {
		return Config{}, err
	}
	cfg.CommandTimeout, err = parseDuration("COMMAND_TIMEOUT", envOrDefault("COMMAND_TIMEOUT", "10m"))
	if err != nil {
		return Config{}, err
	}
	cfg.WorkflowTimeout, err = parseDuration("WORKFLOW_TIMEOUT", envOrDefault("WORKFLOW_TIMEOUT", "30m"))
	if err != nil {
		return Config{}, err
	}
	cfg.PollInterval, err = parseDuration("POLL_INTERVAL", envOrDefault("POLL_INTERVAL", "2s"))
	if err != nil {
		return Config{}, err
	}

	if cfg.SonarrAPIKey == "" {
		return Config{}, errors.New("SONARR_API_KEY is required")
	}
	if cfg.QBittorrentUsername == "" {
		return Config{}, errors.New("QBITTORRENT_USERNAME is required")
	}
	if cfg.QBittorrentPassword == "" {
		return Config{}, errors.New("QBITTORRENT_PASSWORD is required")
	}
	if cfg.Host == "" || net.ParseIP(cfg.Host) == nil {
		return Config{}, errors.New("HOST must be an IP address")
	}
	if !filepath.IsAbs(cfg.DataRoot) {
		return Config{}, errors.New("DATA_ROOT must be an absolute path")
	}
	if cfg.PollInterval > cfg.CommandTimeout {
		return Config{}, errors.New("POLL_INTERVAL must not exceed COMMAND_TIMEOUT")
	}
	if cfg.CommandTimeout > cfg.WorkflowTimeout {
		return Config{}, errors.New("COMMAND_TIMEOUT must not exceed WORKFLOW_TIMEOUT")
	}

	return cfg, nil
}

func (c Config) ListenAddress() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

func envOrDefault(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}

func parseInt(key, raw string, min, max int) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", key, min, max)
	}
	return value, nil
}

func parseDuration(key, raw string) (time.Duration, error) {
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return value, nil
}

func parseBaseURL(key, raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%s is required", key)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("%s must be an absolute HTTP(S) URL", key)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%s must use http or https", key)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%s must not contain credentials, query, or fragment", key)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

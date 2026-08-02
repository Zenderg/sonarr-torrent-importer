package config

import "testing"

func setRequiredEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("SONARR_URL", "http://sonarr:8989")
	t.Setenv("SONARR_API_KEY", "sonarr-key")
	t.Setenv("QBITTORRENT_URL", "http://qbittorrent:8080")
	t.Setenv("QBITTORRENT_USERNAME", "importer")
	t.Setenv("QBITTORRENT_PASSWORD", "password")
	t.Setenv("PROWLARR_URL", "")
	t.Setenv("PROWLARR_API_KEY", "")
	t.Setenv("QBITTORRENT_MEDIA_ROOT", "")
	t.Setenv("SONARR_MEDIA_ROOT", "")
	t.Setenv("IMPORTER_MEDIA_ROOT", "")
}

func TestRollingConfigurationIsOptionalAndAtomic(t *testing.T) {
	setRequiredEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RollingEnabled() {
		t.Fatal("rolling support unexpectedly enabled")
	}

	t.Setenv("PROWLARR_URL", "http://prowlarr:9696")
	if _, err := Load(); err == nil {
		t.Fatal("partial rolling configuration was accepted")
	}

	t.Setenv("PROWLARR_API_KEY", "prowlarr-key")
	t.Setenv("QBITTORRENT_MEDIA_ROOT", "/downloads")
	t.Setenv("SONARR_MEDIA_ROOT", "/remote/downloads")
	t.Setenv("IMPORTER_MEDIA_ROOT", "/media/qbittorrent")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RollingEnabled() {
		t.Fatal("complete rolling configuration was not enabled")
	}
	if cfg.SonarrMediaRoot != "/remote/downloads" {
		t.Fatalf("unexpected Sonarr media root %q", cfg.SonarrMediaRoot)
	}

	t.Setenv("REVISION_POLL_INTERVAL", "59s")
	if _, err := Load(); err == nil {
		t.Fatal("unsafe rolling revision poll interval was accepted")
	}
}

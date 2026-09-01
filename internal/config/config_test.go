package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ipedrazas/tagger/internal/config"
)

// setValid populates the minimum environment for a successful Load.
func setValid(t *testing.T) {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", "test-key")
}

func TestLoadDefaults(t *testing.T) {
	setValid(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OpenRouterBaseURL != config.DefaultOpenRouterBaseURL {
		t.Errorf("BaseURL = %q, want %q", cfg.OpenRouterBaseURL, config.DefaultOpenRouterBaseURL)
	}
	if cfg.OpenRouterModel != config.DefaultOpenRouterModel {
		t.Errorf("Model = %q, want %q", cfg.OpenRouterModel, config.DefaultOpenRouterModel)
	}
	if cfg.HTTPAddr != config.DefaultHTTPAddr || cfg.GRPCAddr != config.DefaultGRPCAddr {
		t.Errorf("addrs = %q/%q, want %q/%q",
			cfg.HTTPAddr, cfg.GRPCAddr, config.DefaultHTTPAddr, config.DefaultGRPCAddr)
	}
	if cfg.MaxTags != config.DefaultMaxTags {
		t.Errorf("MaxTags = %d, want %d", cfg.MaxTags, config.DefaultMaxTags)
	}
	if cfg.RequestTimeout != config.DefaultRequestTimeout {
		t.Errorf("RequestTimeout = %s, want %s", cfg.RequestTimeout, config.DefaultRequestTimeout)
	}
}

func TestLoadOverrides(t *testing.T) {
	setValid(t)
	t.Setenv("OPENROUTER_MODEL", "anthropic/claude-3.5-haiku")
	// The trailing slash must be normalised away, or request URLs get a //.
	t.Setenv("OPENROUTER_BASE_URL", "http://localhost:1234/v1/")
	t.Setenv("HTTP_ADDR", ":18080")
	t.Setenv("GRPC_ADDR", ":19090")
	t.Setenv("MAX_TAGS", "3")
	t.Setenv("MAX_TEXT_BYTES", "100")
	t.Setenv("REQUEST_TIMEOUT", "5s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OpenRouterModel != "anthropic/claude-3.5-haiku" {
		t.Errorf("Model = %q", cfg.OpenRouterModel)
	}
	if cfg.OpenRouterBaseURL != "http://localhost:1234/v1" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", cfg.OpenRouterBaseURL)
	}
	if cfg.HTTPAddr != ":18080" || cfg.GRPCAddr != ":19090" {
		t.Errorf("addrs = %q/%q", cfg.HTTPAddr, cfg.GRPCAddr)
	}
	if cfg.MaxTags != 3 || cfg.MaxTextBytes != 100 {
		t.Errorf("MaxTags/MaxTextBytes = %d/%d, want 3/100", cfg.MaxTags, cfg.MaxTextBytes)
	}
	if cfg.RequestTimeout != 5*time.Second {
		t.Errorf("RequestTimeout = %s, want 5s", cfg.RequestTimeout)
	}
}

func TestLoadRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want a missing-key error")
	}
	if !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("error = %v, want it to name OPENROUTER_API_KEY", err)
	}
}

func TestLoadRejectsMalformedValues(t *testing.T) {
	tests := map[string]struct{ key, value string }{
		"non-numeric max tags": {"MAX_TAGS", "many"},
		"zero max tags":        {"MAX_TAGS", "0"},
		"negative max tags":    {"MAX_TAGS", "-1"},
		"bad duration":         {"REQUEST_TIMEOUT", "soon"},
		"zero duration":        {"REQUEST_TIMEOUT", "0s"},
		"bad max bytes":        {"MAX_TEXT_BYTES", "lots"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			setValid(t)
			t.Setenv(tc.key, tc.value)
			if _, err := config.Load(); err == nil {
				t.Errorf("Load() with %s=%s error = nil, want an error", tc.key, tc.value)
			}
		})
	}
}

func TestLoadReportsAllProblemsAtOnce(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("MAX_TAGS", "0")

	err := func() error { _, err := config.Load(); return err }()
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "OPENROUTER_API_KEY") || !strings.Contains(msg, "MAX_TAGS") {
		t.Errorf("error = %v, want both problems reported", err)
	}
}

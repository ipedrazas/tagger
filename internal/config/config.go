// Package config loads the service configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
	DefaultOpenRouterModel   = "openai/gpt-4o-mini"
	DefaultHTTPAddr          = ":8080"
	DefaultGRPCAddr          = ":9090"
	DefaultRequestTimeout    = 30 * time.Second
	DefaultMaxTags           = 8
	DefaultMaxTextBytes      = 32 * 1024
)

// Config is the fully resolved service configuration.
type Config struct {
	HTTPAddr string
	GRPCAddr string

	OpenRouterAPIKey  string
	OpenRouterModel   string
	OpenRouterBaseURL string

	// RequestTimeout bounds a single upstream LLM call.
	RequestTimeout time.Duration
	// MaxTags caps how many tags the service returns.
	MaxTags int
	// MaxTextBytes rejects oversized inputs before they reach the model.
	MaxTextBytes int

	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// AppURL and AppName populate OpenRouter's optional attribution headers.
	AppURL  string
	AppName string
}

// Load reads the configuration from the process environment.
//
// It returns an error when a required value is missing or malformed so that
// misconfiguration fails at startup rather than on the first request.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:          env("HTTP_ADDR", DefaultHTTPAddr),
		GRPCAddr:          env("GRPC_ADDR", DefaultGRPCAddr),
		OpenRouterAPIKey:  strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")),
		OpenRouterModel:   env("OPENROUTER_MODEL", DefaultOpenRouterModel),
		OpenRouterBaseURL: strings.TrimRight(env("OPENROUTER_BASE_URL", DefaultOpenRouterBaseURL), "/"),
		LogLevel:          env("LOG_LEVEL", "info"),
		AppURL:            os.Getenv("APP_URL"),
		AppName:           env("APP_NAME", "tagger"),
	}

	var errs []error

	timeout, err := envDuration("REQUEST_TIMEOUT", DefaultRequestTimeout)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.RequestTimeout = timeout

	maxTags, err := envInt("MAX_TAGS", DefaultMaxTags)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.MaxTags = maxTags

	maxBytes, err := envInt("MAX_TEXT_BYTES", DefaultMaxTextBytes)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.MaxTextBytes = maxBytes

	if cfg.OpenRouterAPIKey == "" {
		errs = append(errs, errors.New("OPENROUTER_API_KEY is required"))
	}
	if cfg.MaxTags <= 0 {
		errs = append(errs, errors.New("MAX_TAGS must be greater than zero"))
	}
	if cfg.MaxTextBytes <= 0 {
		errs = append(errs, errors.New("MAX_TEXT_BYTES must be greater than zero"))
	}
	if cfg.RequestTimeout <= 0 {
		errs = append(errs, errors.New("REQUEST_TIMEOUT must be greater than zero"))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return v, nil
}

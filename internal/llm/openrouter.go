// Package llm contains the OpenRouter-backed implementation of
// tagging.Completer.
//
// OpenRouter speaks the OpenAI chat-completions wire format, and the service
// only ever needs one endpoint, so this uses net/http directly rather than
// pulling in a full vendor SDK. See the README for the trade-off.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"
)

// Client calls OpenRouter's chat-completions API. It is safe for concurrent
// use and is intended to be built once at start-up and shared: the underlying
// http.Client pools and reuses TLS connections, which a per-request client
// would throw away.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	model      string
	appURL     string
	appName    string
	maxRetries int
	logger     *slog.Logger
}

// Options configures a Client.
type Options struct {
	// BaseURL is the API root, without a trailing slash.
	BaseURL string
	// APIKey is the OpenRouter key. Required.
	APIKey string
	// Model is the OpenRouter model slug, e.g. "openai/gpt-4o-mini".
	Model string
	// Timeout bounds a single HTTP attempt. Defaults to 30s.
	Timeout time.Duration
	// MaxRetries is the number of retries after the first attempt. Zero
	// selects the default of 2; a negative value disables retries.
	MaxRetries int
	// AppURL and AppName populate OpenRouter's optional attribution headers.
	AppURL  string
	AppName string
	// HTTPClient overrides the transport. Used by tests.
	HTTPClient *http.Client
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// ErrConfig indicates the client was constructed with unusable options.
var ErrConfig = errors.New("invalid openrouter client configuration")

// New builds an OpenRouter client.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, fmt.Errorf("%w: api key is required", ErrConfig)
	}
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("%w: base url is required", ErrConfig)
	}
	if opts.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrConfig)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	switch {
	case opts.MaxRetries < 0:
		opts.MaxRetries = 0
	case opts.MaxRetries == 0:
		opts.MaxRetries = 2
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Client{
		httpClient: httpClient,
		baseURL:    opts.BaseURL,
		apiKey:     opts.APIKey,
		model:      opts.Model,
		appURL:     opts.AppURL,
		appName:    opts.AppName,
		maxRetries: opts.MaxRetries,
		logger:     logger,
	}, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Complete implements tagging.Completer.
func (c *Client) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	payload, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		// Tagging wants repeatable output, not creative output.
		Temperature: 0,
	})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return "", err
			}
		}

		content, retryable, err := c.do(ctx, payload)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !retryable || ctx.Err() != nil {
			break
		}
		c.logger.WarnContext(ctx, "openrouter call failed, retrying",
			slog.Int("attempt", attempt+1), slog.String("error", err.Error()))
	}
	return "", lastErr
}

// do performs one attempt. The bool reports whether a retry could help.
func (c *Client) do(ctx context.Context, payload []byte) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	if c.appURL != "" {
		req.Header.Set("HTTP-Referer", c.appURL)
	}
	if c.appName != "" {
		req.Header.Set("X-Title", c.appName)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport errors (dial, reset, timeout) are worth another attempt.
		return "", true, fmt.Errorf("call openrouter: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	// Cap the body: a misconfigured base URL should not let a stray endpoint
	// stream unbounded data into memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", true, fmt.Errorf("read openrouter response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", retryable, fmt.Errorf("openrouter returned %d: %s",
			resp.StatusCode, snippet(body))
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", false, fmt.Errorf("decode openrouter response: %w", err)
	}
	// OpenRouter can return a 200 carrying an error object from the upstream
	// provider.
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", false, fmt.Errorf("openrouter error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", false, errors.New("openrouter returned no choices")
	}
	return parsed.Choices[0].Message.Content, false, nil
}

// backoff returns an exponential delay: 250ms, 500ms, 1s, ...
func backoff(attempt int) time.Duration {
	return time.Duration(math.Pow(2, float64(attempt-1))*250) * time.Millisecond
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func snippet(b []byte) string {
	const limit = 200
	if len(b) > limit {
		return strconv.Quote(string(b[:limit]) + "...")
	}
	return strconv.Quote(string(b))
}

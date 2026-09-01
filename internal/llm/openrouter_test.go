package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipedrazas/tagger/internal/llm"
)

// chatCompletion renders a minimal OpenRouter success body.
func chatCompletion(content string) string {
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": content}},
		},
	})
	return string(body)
}

func newClient(t *testing.T, baseURL string, opts ...func(*llm.Options)) *llm.Client {
	t.Helper()
	o := llm.Options{
		BaseURL:    baseURL,
		APIKey:     "test-key",
		Model:      "openai/gpt-4o-mini",
		MaxRetries: -1, // deterministic by default; tests opt into retries
	}
	for _, fn := range opts {
		fn(&o)
	}
	c, err := llm.New(o)
	if err != nil {
		t.Fatalf("llm.New() error = %v", err)
	}
	return c
}

func TestCompleteSendsWellFormedRequest(t *testing.T) {
	var gotPath, gotAuth, gotTitle string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotTitle = r.Header.Get("X-Title")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, chatCompletion(`["a"]`))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, func(o *llm.Options) { o.AppName = "tagger" })
	got, err := c.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got != `["a"]` {
		t.Errorf("Complete() = %q, want %q", got, `["a"]`)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotTitle != "tagger" {
		t.Errorf("X-Title = %q, want tagger", gotTitle)
	}
	if gotBody["model"] != "openai/gpt-4o-mini" {
		t.Errorf("model = %v, want openai/gpt-4o-mini", gotBody["model"])
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2 entries", gotBody["messages"])
	}
	if role := msgs[0].(map[string]any)["role"]; role != "system" {
		t.Errorf("first message role = %v, want system", role)
	}
	if content := msgs[1].(map[string]any)["content"]; content != "usr" {
		t.Errorf("second message content = %v, want usr", content)
	}
}

func TestCompleteRetriesServerErrorsThenSucceeds(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"upstream busy"}}`)
			return
		}
		_, _ = io.WriteString(w, chatCompletion(`["ok"]`))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, func(o *llm.Options) { o.MaxRetries = 2 })
	got, err := c.Complete(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got != `["ok"]` {
		t.Errorf("Complete() = %q, want [\"ok\"]", got)
	}
	if n := attempts.Load(); n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}
}

func TestCompleteDoesNotRetryClientErrors(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, func(o *llm.Options) { o.MaxRetries = 3 })
	if _, err := c.Complete(context.Background(), "sys", "usr"); err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want 1 (401 is not retryable)", n)
	}
}

func TestCompleteRetriesRateLimits(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, func(o *llm.Options) { o.MaxRetries = 1 })
	if _, err := c.Complete(context.Background(), "sys", "usr"); err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}
	if n := attempts.Load(); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

func TestCompleteSurfacesInlineErrorObject(t *testing.T) {
	// OpenRouter can answer 200 while carrying a provider error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"error":{"message":"model is down","code":503}}`)
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).Complete(context.Background(), "sys", "usr")
	if err == nil || !strings.Contains(err.Error(), "model is down") {
		t.Fatalf("Complete() error = %v, want it to mention the provider message", err)
	}
}

func TestCompleteRejectsEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	if _, err := newClient(t, srv.URL).Complete(context.Background(), "sys", "usr"); err == nil {
		t.Fatal("Complete() error = nil, want an error")
	}
}

func TestCompleteHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = io.WriteString(w, chatCompletion(`["late"]`))
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := newClient(t, srv.URL).Complete(ctx, "sys", "usr"); err == nil {
		t.Fatal("Complete() error = nil, want a timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Complete() took %v; context deadline was not honoured", elapsed)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	tests := map[string]llm.Options{
		"missing key":   {BaseURL: "http://x", Model: "m"},
		"missing url":   {APIKey: "k", Model: "m"},
		"missing model": {APIKey: "k", BaseURL: "http://x"},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := llm.New(opts); err == nil {
				t.Error("llm.New() error = nil, want an error")
			}
		})
	}
}

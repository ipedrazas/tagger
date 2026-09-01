package rest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipedrazas/tagger/internal/api/rest"
	"github.com/ipedrazas/tagger/internal/tagging"
)

// fakeTagger stands in for the tagging service.
type fakeTagger struct {
	tags     []string
	err      error
	lastText string
}

func (f *fakeTagger) Tag(_ context.Context, text string) ([]string, error) {
	f.lastText = text
	return f.tags, f.err
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTagReturnsTags(t *testing.T) {
	svc := &fakeTagger{tags: []string{"go", "grpc"}}
	h := rest.NewHandler(svc, rest.Options{})

	rec := do(t, h, http.MethodPost, "/tag", `{"text":"Go and gRPC"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got rest.TagResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "go" || got.Tags[1] != "grpc" {
		t.Errorf("tags = %v, want [go grpc]", got.Tags)
	}
	if svc.lastText != "Go and gRPC" {
		t.Errorf("service received %q, want %q", svc.lastText, "Go and gRPC")
	}
}

func TestTagRendersEmptyTagsAsArray(t *testing.T) {
	h := rest.NewHandler(&fakeTagger{tags: []string{}}, rest.Options{})

	rec := do(t, h, http.MethodPost, "/tag", `{"text":"..."}`)
	if body := strings.TrimSpace(rec.Body.String()); body != `{"tags":[]}` {
		t.Errorf("body = %s, want {\"tags\":[]}", body)
	}
}

func TestTagBadRequests(t *testing.T) {
	tests := map[string]struct {
		body     string
		svcErr   error
		wantCode int
	}{
		"missing text field": {`{}`, tagging.ErrEmptyText, http.StatusBadRequest},
		"empty text":         {`{"text":""}`, tagging.ErrEmptyText, http.StatusBadRequest},
		"whitespace text":    {`{"text":"   "}`, tagging.ErrEmptyText, http.StatusBadRequest},
		"malformed json":     {`{"text":`, nil, http.StatusBadRequest},
		"empty body":         {``, nil, http.StatusBadRequest},
		"wrong type":         {`{"text":123}`, nil, http.StatusBadRequest},
		"unknown field":      {`{"txt":"hi"}`, nil, http.StatusBadRequest},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := rest.NewHandler(&fakeTagger{err: tc.svcErr}, rest.Options{})
			rec := do(t, h, http.MethodPost, "/tag", tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantCode, rec.Body)
			}
			var errBody rest.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if errBody.Error == "" {
				t.Error("error body has no message")
			}
		})
	}
}

func TestTagUpstreamFailureIsBadGateway(t *testing.T) {
	for name, err := range map[string]error{
		"upstream":    tagging.ErrUpstream,
		"unparseable": tagging.ErrUnparseable,
	} {
		t.Run(name, func(t *testing.T) {
			h := rest.NewHandler(&fakeTagger{err: err}, rest.Options{})
			rec := do(t, h, http.MethodPost, "/tag", `{"text":"hello"}`)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", rec.Code)
			}
		})
	}
}

func TestTagDoesNotLeakUpstreamDetail(t *testing.T) {
	err := errors.New("openrouter returned 401: {\"error\":\"invalid api key sk-or-secret\"}")
	h := rest.NewHandler(&fakeTagger{err: err}, rest.Options{})

	rec := do(t, h, http.MethodPost, "/tag", `{"text":"hello"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sk-or-secret") {
		t.Errorf("response leaked upstream detail: %s", rec.Body)
	}
}

func TestTagOversizedTextIsPayloadTooLarge(t *testing.T) {
	h := rest.NewHandler(&fakeTagger{err: tagging.ErrTextTooLarge}, rest.Options{})
	rec := do(t, h, http.MethodPost, "/tag", `{"text":"hello"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestTagOversizedBodyIsPayloadTooLarge(t *testing.T) {
	h := rest.NewHandler(&fakeTagger{tags: []string{}}, rest.Options{MaxBodyBytes: 32})
	rec := do(t, h, http.MethodPost, "/tag", `{"text":"`+strings.Repeat("x", 200)+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", rec.Code, rec.Body)
	}
}

func TestTagRejectsWrongMethod(t *testing.T) {
	h := rest.NewHandler(&fakeTagger{}, rest.Options{})
	rec := do(t, h, http.MethodGet, "/tag", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	h := rest.NewHandler(&fakeTagger{}, rest.Options{Version: "v1.2.3"})
	rec := do(t, h, http.MethodGet, "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got rest.HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Status != "ok" || got.Version != "v1.2.3" {
		t.Errorf("health = %+v, want {ok v1.2.3}", got)
	}
}

func TestOpenAPISpecIsServed(t *testing.T) {
	h := rest.NewHandler(&fakeTagger{}, rest.Options{})
	rec := do(t, h, http.MethodGet, "/openapi.yaml", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "openapi: 3.1.0") {
		t.Errorf("body does not look like an OpenAPI document: %s", rec.Body)
	}
}

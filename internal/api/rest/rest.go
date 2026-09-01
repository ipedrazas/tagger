// Package rest exposes the tagging service over HTTP/JSON.
package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ipedrazas/tagger/api"
	"github.com/ipedrazas/tagger/internal/tagging"
)

// Tagger is the behaviour the HTTP layer needs from the tagging service.
type Tagger interface {
	Tag(ctx context.Context, text string) ([]string, error)
}

// TagRequest is the POST /tag request body.
type TagRequest struct {
	Text string `json:"text"`
}

// TagResponse is the POST /tag success body.
type TagResponse struct {
	Tags []string `json:"tags"`
}

// ErrorResponse is the body returned for every non-2xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// HealthResponse is the GET /health body.
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// Options configures the HTTP handler.
type Options struct {
	// Version is reported by GET /health.
	Version string
	// MaxBodyBytes bounds the request body. Defaults to 64 KiB.
	MaxBodyBytes int64
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// NewHandler builds the fully routed HTTP handler for the service.
func NewHandler(svc Tagger, opts Options) http.Handler {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 64 * 1024
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &server{svc: svc, opts: opts, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /tag", s.handleTag)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /openapi.yaml", s.handleOpenAPI)

	return logRequests(logger)(mux)
}

type server struct {
	svc    Tagger
	opts   Options
	logger *slog.Logger
}

func (s *server) handleTag(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)

	var req TagRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "request body must be a JSON object with a \"text\" field")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	tags, err := s.svc.Tag(r.Context(), req.Text)
	if err != nil {
		s.writeTagError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, TagResponse{Tags: tags})
}

// writeTagError maps domain errors onto HTTP status codes. Upstream detail is
// logged but never echoed to the client, so provider messages and keys cannot
// leak through the API.
func (s *server) writeTagError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tagging.ErrEmptyText):
		writeError(w, http.StatusBadRequest, "text must not be empty")
	case errors.Is(err, tagging.ErrTextTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "text exceeds maximum size")
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// The client hung up; nothing useful to write.
		s.logger.DebugContext(r.Context(), "client cancelled tag request")
	case errors.Is(err, context.DeadlineExceeded):
		s.logger.ErrorContext(r.Context(), "tag request timed out", slog.String("error", err.Error()))
		writeError(w, http.StatusGatewayTimeout, "tagging timed out")
	case errors.Is(err, tagging.ErrUpstream), errors.Is(err, tagging.ErrUnparseable):
		s.logger.ErrorContext(r.Context(), "upstream tagging failure", slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, "tagging provider unavailable")
	default:
		s.logger.ErrorContext(r.Context(), "unexpected tagging failure", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok", Version: s.opts.Version})
}

func (s *server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(api.OpenAPISpec)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func logRequests(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.InfoContext(r.Context(), "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

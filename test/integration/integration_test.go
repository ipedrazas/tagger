//go:build integration

// Package integration exercises the REST and gRPC surfaces of a running
// tagger over real sockets.
//
// By default it starts the service in-process against a stub OpenRouter, so it
// needs no API key and no network. Point TAGGER_HTTP_ADDR and TAGGER_GRPC_ADDR
// at an already-running instance (docker compose, a deployed pod) to run the
// same assertions against it instead.
//
//	task test:integration
//	TAGGER_HTTP_ADDR=localhost:8080 TAGGER_GRPC_ADDR=localhost:9090 task test:integration
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/ipedrazas/tagger/internal/app"
	"github.com/ipedrazas/tagger/internal/config"
	taggerv1 "github.com/ipedrazas/tagger/proto/gen/tagger/v1"
)

// target is the pair of addresses under test.
type target struct {
	httpAddr string
	grpcAddr string
	// external is true when we are driving somebody else's process, in which
	// case the stub-upstream assertions do not apply.
	external bool
}

var (
	// upstreamCalls counts requests the service made to the stub OpenRouter.
	upstreamCalls atomic.Int32
	// upstreamReply is the content the stub returns for the next call.
	upstreamReply atomic.Value // string
	// upstreamStatus is the HTTP status the stub returns. 0 means 200.
	upstreamStatus atomic.Int32
)

var testTarget target

func TestMain(m *testing.M) {
	// run() owns every defer; TestMain only translates its result into an
	// exit code, so no cleanup is skipped by os.Exit.
	os.Exit(run(m))
}

func run(m *testing.M) int {
	httpAddr, grpcAddr := os.Getenv("TAGGER_HTTP_ADDR"), os.Getenv("TAGGER_GRPC_ADDR")
	if httpAddr != "" && grpcAddr != "" {
		testTarget = target{httpAddr: httpAddr, grpcAddr: grpcAddr, external: true}
		if err := waitForHealth(context.Background(), httpAddr, 30*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "service at %s never became healthy: %v\n", httpAddr, err)
			return 1
		}
		return m.Run()
	}

	stub := httptest.NewServer(http.HandlerFunc(stubOpenRouter))
	defer stub.Close()

	// The service is configured exactly as in production, except that the
	// upstream is the stub and both ports are ephemeral.
	env := map[string]string{
		"OPENROUTER_API_KEY":  "integration-test-key",
		"OPENROUTER_BASE_URL": stub.URL,
		"HTTP_ADDR":           "127.0.0.1:0",
		"GRPC_ADDR":           "127.0.0.1:0",
		"MAX_TAGS":            "4",
		"MAX_TEXT_BYTES":      "512",
		"REQUEST_TIMEOUT":     "5s",
	}
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "set %s: %v\n", k, err)
			return 1
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.New(ctx, cfg, "integration", logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build app: %v\n", err)
		return 1
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	testTarget = target{httpAddr: a.HTTPAddr(), grpcAddr: a.GRPCAddr()}
	if err := waitForHealth(ctx, testTarget.httpAddr, 10*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "service never became healthy: %v\n", err)
		return 1
	}

	code := m.Run()

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "server exited with error: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	case <-time.After(20 * time.Second):
		fmt.Fprintln(os.Stderr, "server did not shut down in time")
		if code == 0 {
			code = 1
		}
	}
	return code
}

// stubOpenRouter answers the chat-completions call the service makes.
func stubOpenRouter(w http.ResponseWriter, _ *http.Request) {
	upstreamCalls.Add(1)

	if code := upstreamStatus.Load(); code != 0 {
		w.WriteHeader(int(code))
		_, _ = io.WriteString(w, `{"error":{"message":"stub failure"}}`)
		return
	}

	reply, _ := upstreamReply.Load().(string)
	if reply == "" {
		reply = `["kubernetes", "operators", "custom resources"]`
	}
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": reply}},
		},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// setUpstream configures the stub for one test and restores it afterwards.
// It skips when running against an external service, where we cannot steer the
// model.
func setUpstream(t *testing.T, reply string, statusCode int) {
	t.Helper()
	if testTarget.external {
		t.Skip("cannot steer the model of an externally running service")
	}
	upstreamReply.Store(reply)
	upstreamStatus.Store(int32(statusCode))
	t.Cleanup(func() {
		upstreamReply.Store("")
		upstreamStatus.Store(int32(0))
	})
}

func waitForHealth(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := get(ctx, "http://"+addr+"/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health returned %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return lastErr
}

// get and post are thin context-carrying wrappers; the bare http.Get/http.Post
// helpers cannot be cancelled.
func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func post(ctx context.Context, url, contentType string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return http.DefaultClient.Do(req)
}

// --- REST -------------------------------------------------------------------

func postTag(t *testing.T, body string) (int, []byte) {
	t.Helper()
	resp, err := post(t.Context(), "http://"+testTarget.httpAddr+"/tag",
		"application/json", []byte(body))
	if err != nil {
		t.Fatalf("POST /tag: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

func TestRESTHealth(t *testing.T) {
	resp, err := get(t.Context(), "http://"+testTarget.httpAddr+"/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
}

func TestRESTTag(t *testing.T) {
	setUpstream(t, `["Kubernetes", "Operators", "kubernetes"]`, 0)

	code, raw := postTag(t, `{"text":"Kubernetes operators reconcile desired state."}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", code, raw)
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	// Normalisation lower-cases and de-duplicates end to end.
	if len(body.Tags) != 2 || body.Tags[0] != "kubernetes" || body.Tags[1] != "operators" {
		t.Errorf("tags = %v, want [kubernetes operators]", body.Tags)
	}
}

func TestRESTEmptyTextIsBadRequest(t *testing.T) {
	for _, body := range []string{`{"text":""}`, `{"text":"   "}`, `{}`} {
		code, raw := postTag(t, body)
		if code != http.StatusBadRequest {
			t.Errorf("POST %s: status = %d, want 400 (body %s)", body, code, raw)
		}
	}
}

func TestRESTMalformedJSONIsBadRequest(t *testing.T) {
	code, raw := postTag(t, `{"text":`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %s)", code, raw)
	}
}

func TestRESTUpstreamFailureIsBadGateway(t *testing.T) {
	setUpstream(t, "", http.StatusInternalServerError)

	code, raw := postTag(t, `{"text":"anything"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", code, raw)
	}
	if strings.Contains(string(raw), "stub failure") {
		t.Errorf("response leaked upstream detail: %s", raw)
	}
}

func TestRESTUnparseableModelOutputIsBadGateway(t *testing.T) {
	setUpstream(t, "I'm afraid I can't do that.", 0)

	code, raw := postTag(t, `{"text":"anything"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", code, raw)
	}
}

func TestRESTOversizedTextIsRejected(t *testing.T) {
	if testTarget.external {
		t.Skip("size limit depends on the target's configuration")
	}
	body, _ := json.Marshal(map[string]string{"text": strings.Repeat("x", 600)})

	code, raw := postTag(t, string(body))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", code, raw)
	}
}

func TestRESTServesOpenAPISpec(t *testing.T) {
	resp, err := get(t.Context(), "http://"+testTarget.httpAddr+"/openapi.yaml")
	if err != nil {
		t.Fatalf("GET /openapi.yaml: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte("openapi:")) {
		t.Fatalf("status = %d, body = %.80s", resp.StatusCode, raw)
	}
}

// --- gRPC -------------------------------------------------------------------

func dialGRPC(t *testing.T) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(testTarget.grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", testTarget.grpcAddr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestGRPCTag(t *testing.T) {
	setUpstream(t, `["Kubernetes", "Operators", "kubernetes"]`, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := taggerv1.NewTaggerClient(dialGRPC(t)).
		Tag(ctx, &taggerv1.TagRequest{Text: "Kubernetes operators reconcile desired state."})
	if err != nil {
		t.Fatalf("Tag() error = %v", err)
	}
	if got := resp.GetTags(); len(got) != 2 || got[0] != "kubernetes" || got[1] != "operators" {
		t.Errorf("tags = %v, want [kubernetes operators]", got)
	}
}

func TestGRPCEmptyTextIsInvalidArgument(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := taggerv1.NewTaggerClient(dialGRPC(t)).Tag(ctx, &taggerv1.TagRequest{Text: "  "})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument (err %v)", got, err)
	}
}

func TestGRPCUpstreamFailureIsUnavailable(t *testing.T) {
	setUpstream(t, "", http.StatusInternalServerError)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := taggerv1.NewTaggerClient(dialGRPC(t)).Tag(ctx, &taggerv1.TagRequest{Text: "anything"})
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %s, want Unavailable (err %v)", got, err)
	}
	if err != nil && strings.Contains(err.Error(), "stub failure") {
		t.Errorf("status leaked upstream detail: %v", err)
	}
}

func TestGRPCHealthCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := healthpb.NewHealthClient(dialGRPC(t)).
		Check(ctx, &healthpb.HealthCheckRequest{Service: "tagger.v1.Tagger"})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %s, want SERVING", resp.GetStatus())
	}
}

// TestBothTransportsAgree is the point of having two surfaces: the same input
// must produce the same tags whichever door it comes through.
func TestBothTransportsAgree(t *testing.T) {
	setUpstream(t, `["alpha", "beta", "gamma"]`, 0)

	const text = "A sentence worth tagging."

	code, raw := postTag(t, `{"text":"`+text+`"}`)
	if code != http.StatusOK {
		t.Fatalf("REST status = %d, want 200 (body %s)", code, raw)
	}
	var restBody struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(raw, &restBody); err != nil {
		t.Fatalf("decode REST body: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	grpcResp, err := taggerv1.NewTaggerClient(dialGRPC(t)).
		Tag(ctx, &taggerv1.TagRequest{Text: text})
	if err != nil {
		t.Fatalf("gRPC Tag() error = %v", err)
	}

	if strings.Join(restBody.Tags, ",") != strings.Join(grpcResp.GetTags(), ",") {
		t.Errorf("REST tags %v != gRPC tags %v", restBody.Tags, grpcResp.GetTags())
	}
}

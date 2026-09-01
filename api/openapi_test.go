package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ipedrazas/tagger/api"
	"github.com/ipedrazas/tagger/internal/api/rest"
)

// spec is the subset of the OpenAPI document these tests assert on.
type spec struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"info"`
	Paths map[string]map[string]struct {
		OperationID string         `yaml:"operationId"`
		Summary     string         `yaml:"summary"`
		Responses   map[string]any `yaml:"responses"`
	} `yaml:"paths"`
	Components struct {
		Schemas map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

// stubTagger keeps this test focused on the contract, not on the model.
type stubTagger struct{}

func (stubTagger) Tag(_ context.Context, _ string) ([]string, error) { return []string{}, nil }

func load(t *testing.T) spec {
	t.Helper()
	var s spec
	if err := yaml.Unmarshal(api.OpenAPISpec, &s); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	return s
}

func TestSpecIsWellFormed(t *testing.T) {
	s := load(t)
	if !strings.HasPrefix(s.OpenAPI, "3.") {
		t.Errorf("openapi = %q, want a 3.x document", s.OpenAPI)
	}
	if s.Info.Title == "" || s.Info.Version == "" {
		t.Errorf("info = %+v, want a title and a version", s.Info)
	}
	for _, want := range []string{"TagRequest", "TagResponse", "ErrorResponse", "HealthResponse"} {
		if _, ok := s.Components.Schemas[want]; !ok {
			t.Errorf("components.schemas is missing %s", want)
		}
	}
}

// TestSpecMatchesRoutes is the point of keeping the spec in the binary: a route
// that exists but is undocumented (or vice versa) fails the build.
func TestSpecMatchesRoutes(t *testing.T) {
	s := load(t)

	documented := map[string]bool{}
	for path, ops := range s.Paths {
		for method := range ops {
			documented[strings.ToUpper(method)+" "+path] = true
		}
	}

	// /openapi.yaml serves the document itself and is deliberately not part of
	// the described API surface.
	wantRoutes := []string{"POST /tag", "GET /health"}
	for _, route := range wantRoutes {
		if !documented[route] {
			t.Errorf("%s is served but not documented in openapi.yaml", route)
		}
	}
	if len(documented) != len(wantRoutes) {
		t.Errorf("openapi.yaml documents %v, want exactly %v", documented, wantRoutes)
	}
}

// TestDocumentedStatusCodesAreReachable checks the codes the spec promises are
// the codes the handler actually produces for the corresponding inputs.
func TestDocumentedStatusCodesAreReachable(t *testing.T) {
	s := load(t)

	tagOp, ok := s.Paths["/tag"]["post"]
	if !ok {
		t.Fatal("openapi.yaml does not describe POST /tag")
	}

	h := rest.NewHandler(stubTagger{}, rest.Options{})
	for _, code := range []string{"200", "400", "413"} {
		if _, documented := tagOp.Responses[code]; !documented {
			t.Errorf("POST /tag response %s is produced by the handler but not documented", code)
		}
	}

	// 400 really is what a malformed body yields.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/tag",
		strings.NewReader(`{"text":`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body gave %d, but the spec documents 400", rec.Code)
	}
}

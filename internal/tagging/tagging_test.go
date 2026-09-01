package tagging_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ipedrazas/tagger/internal/tagging"
)

// fakeCompleter is a stub LLM: it records what it was asked and replays a
// canned response.
type fakeCompleter struct {
	response   string
	err        error
	calls      int
	lastSystem string
	lastUser   string
}

func (f *fakeCompleter) Complete(_ context.Context, system, user string) (string, error) {
	f.calls++
	f.lastSystem = system
	f.lastUser = user
	return f.response, f.err
}

func TestTagHappyPath(t *testing.T) {
	llm := &fakeCompleter{response: `["Kubernetes", "operators", "Custom Resources"]`}
	svc := tagging.NewService(llm, tagging.Options{})

	tags, err := svc.Tag(context.Background(), "Kubernetes operators reconcile state.")
	if err != nil {
		t.Fatalf("Tag() error = %v", err)
	}

	want := []string{"kubernetes", "operators", "custom resources"}
	assertTags(t, tags, want)

	if llm.calls != 1 {
		t.Errorf("llm calls = %d, want 1", llm.calls)
	}
	if !strings.Contains(llm.lastUser, "Kubernetes operators reconcile state.") {
		t.Errorf("user prompt does not carry the input text: %q", llm.lastUser)
	}
	if llm.lastSystem == "" {
		t.Error("system prompt was empty")
	}
}

func TestTagNormalisesAndDeduplicates(t *testing.T) {
	llm := &fakeCompleter{response: `["Go", "  go  ", "GO", "web   services", "", "   ", "#rust!"]`}
	svc := tagging.NewService(llm, tagging.Options{})

	tags, err := svc.Tag(context.Background(), "some text")
	if err != nil {
		t.Fatalf("Tag() error = %v", err)
	}
	assertTags(t, tags, []string{"go", "web services", "rust"})
}

func TestTagRespectsMaxTags(t *testing.T) {
	llm := &fakeCompleter{response: `["a", "b", "c", "d", "e"]`}
	svc := tagging.NewService(llm, tagging.Options{MaxTags: 2})

	tags, err := svc.Tag(context.Background(), "some text")
	if err != nil {
		t.Fatalf("Tag() error = %v", err)
	}
	assertTags(t, tags, []string{"a", "b"})

	if !strings.Contains(llm.lastUser, "at most 2 tags") {
		t.Errorf("prompt should ask for at most 2 tags, got %q", llm.lastUser)
	}
}

func TestTagEmptyArrayIsNotAnError(t *testing.T) {
	llm := &fakeCompleter{response: `[]`}
	svc := tagging.NewService(llm, tagging.Options{})

	tags, err := svc.Tag(context.Background(), "...")
	if err != nil {
		t.Fatalf("Tag() error = %v", err)
	}
	if tags == nil {
		t.Fatal("tags is nil; want a non-nil empty slice so JSON renders []")
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v, want empty", tags)
	}
}

func TestTagRejectsEmptyText(t *testing.T) {
	llm := &fakeCompleter{response: `["unused"]`}
	svc := tagging.NewService(llm, tagging.Options{})

	for _, input := range []string{"", "   ", "\n\t "} {
		if _, err := svc.Tag(context.Background(), input); !errors.Is(err, tagging.ErrEmptyText) {
			t.Errorf("Tag(%q) error = %v, want ErrEmptyText", input, err)
		}
	}
	if llm.calls != 0 {
		t.Errorf("llm was called %d times for empty input, want 0", llm.calls)
	}
}

func TestTagRejectsOversizedText(t *testing.T) {
	llm := &fakeCompleter{response: `["unused"]`}
	svc := tagging.NewService(llm, tagging.Options{MaxTextBytes: 10})

	_, err := svc.Tag(context.Background(), strings.Repeat("x", 11))
	if !errors.Is(err, tagging.ErrTextTooLarge) {
		t.Fatalf("Tag() error = %v, want ErrTextTooLarge", err)
	}
	if llm.calls != 0 {
		t.Errorf("llm was called %d times for oversized input, want 0", llm.calls)
	}
}

func TestTagWrapsUpstreamFailure(t *testing.T) {
	sentinel := errors.New("boom")
	svc := tagging.NewService(&fakeCompleter{err: sentinel}, tagging.Options{})

	_, err := svc.Tag(context.Background(), "some text")
	if !errors.Is(err, tagging.ErrUpstream) {
		t.Fatalf("Tag() error = %v, want ErrUpstream", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Tag() error should wrap the underlying cause, got %v", err)
	}
}

func TestTagPropagatesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc := tagging.NewService(&fakeCompleter{err: context.Canceled}, tagging.Options{})
	_, err := svc.Tag(ctx, "some text")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Tag() error = %v, want context.Canceled", err)
	}
}

func TestTagRejectsUnparseableResponse(t *testing.T) {
	for name, response := range map[string]string{
		"prose":         "Sure! Here are some tags for you.",
		"empty":         "",
		"mixed types":   `["a", 42]`,
		"truncated":     `["a", "b"`,
		"two arrays":    `["a"] ["b"]`,
		"nested arrays": `[["a"]]`,
	} {
		t.Run(name, func(t *testing.T) {
			svc := tagging.NewService(&fakeCompleter{response: response}, tagging.Options{})
			if _, err := svc.Tag(context.Background(), "text"); !errors.Is(err, tagging.ErrUnparseable) {
				t.Errorf("Tag() error = %v, want ErrUnparseable", err)
			}
		})
	}
}

func TestParseTagsToleratesModelPackaging(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want []string
	}{
		"bare array":     {`["a","b"]`, []string{"a", "b"}},
		"fenced json":    {"```json\n[\"a\", \"b\"]\n```", []string{"a", "b"}},
		"bare fence":     {"```\n[\"a\"]\n```", []string{"a"}},
		"leading prose":  {"Here you go:\n[\"a\", \"b\"]", []string{"a", "b"}},
		"trailing prose": {"[\"a\"]\nHope that helps!", []string{"a"}},
		"whitespace":     {"  \n [\"a\"] \n ", []string{"a"}},
		"empty array":    {`[]`, []string{}},
		"tags object":    {`{"tags": ["a", "b"]}`, []string{"a", "b"}},
		"fenced object":  {"```json\n{\"tags\": [\"a\"]}\n```", []string{"a"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := tagging.ParseTags(tc.raw)
			if err != nil {
				t.Fatalf("ParseTags(%q) error = %v", tc.raw, err)
			}
			assertTags(t, got, tc.want)
		})
	}
}

func TestTagDropsAbsurdlyLongTags(t *testing.T) {
	long := strings.Repeat("a", 100)
	svc := tagging.NewService(&fakeCompleter{response: `["ok", "` + long + `"]`}, tagging.Options{})

	tags, err := svc.Tag(context.Background(), "text")
	if err != nil {
		t.Fatalf("Tag() error = %v", err)
	}
	assertTags(t, tags, []string{"ok"})
}

func assertTags(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tags = %v, want %v", got, want)
		}
	}
}

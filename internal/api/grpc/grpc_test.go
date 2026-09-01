package grpcapi_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	grpcapi "github.com/ipedrazas/tagger/internal/api/grpc"
	"github.com/ipedrazas/tagger/internal/tagging"
	taggerv1 "github.com/ipedrazas/tagger/proto/gen/tagger/v1"
)

type fakeTagger struct {
	tags     []string
	err      error
	lastText string
}

func (f *fakeTagger) Tag(_ context.Context, text string) ([]string, error) {
	f.lastText = text
	return f.tags, f.err
}

// newClient starts the gRPC service over an in-memory listener and returns a
// connected client.
func newClient(t *testing.T, svc *fakeTagger) taggerv1.TaggerClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	grpcapi.NewServer(svc, nil).Register(srv)

	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("grpc serve: %v", err)
		}
	}()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	})
	return taggerv1.NewTaggerClient(conn)
}

func TestTagReturnsTags(t *testing.T) {
	svc := &fakeTagger{tags: []string{"go", "grpc"}}
	client := newClient(t, svc)

	resp, err := client.Tag(context.Background(), &taggerv1.TagRequest{Text: "Go and gRPC"})
	if err != nil {
		t.Fatalf("Tag() error = %v", err)
	}
	if got := resp.GetTags(); len(got) != 2 || got[0] != "go" || got[1] != "grpc" {
		t.Errorf("tags = %v, want [go grpc]", got)
	}
	if svc.lastText != "Go and gRPC" {
		t.Errorf("service received %q, want %q", svc.lastText, "Go and gRPC")
	}
}

func TestTagEmptyResultIsNotAnError(t *testing.T) {
	client := newClient(t, &fakeTagger{tags: []string{}})

	resp, err := client.Tag(context.Background(), &taggerv1.TagRequest{Text: "..."})
	if err != nil {
		t.Fatalf("Tag() error = %v", err)
	}
	if len(resp.GetTags()) != 0 {
		t.Errorf("tags = %v, want empty", resp.GetTags())
	}
}

func TestTagErrorCodes(t *testing.T) {
	tests := map[string]struct {
		err  error
		want codes.Code
	}{
		"empty text":  {tagging.ErrEmptyText, codes.InvalidArgument},
		"too large":   {tagging.ErrTextTooLarge, codes.InvalidArgument},
		"upstream":    {tagging.ErrUpstream, codes.Unavailable},
		"unparseable": {tagging.ErrUnparseable, codes.Unavailable},
		"unknown":     {errors.New("boom"), codes.Internal},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			client := newClient(t, &fakeTagger{err: tc.err})
			_, err := client.Tag(context.Background(), &taggerv1.TagRequest{Text: "hi"})
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %s, want %s (err %v)", got, tc.want, err)
			}
		})
	}
}

func TestTagDoesNotLeakUpstreamDetail(t *testing.T) {
	svcErr := errors.New("openrouter returned 401: invalid api key sk-or-secret")
	client := newClient(t, &fakeTagger{err: svcErr})

	_, err := client.Tag(context.Background(), &taggerv1.TagRequest{Text: "hi"})
	if err == nil {
		t.Fatal("Tag() error = nil, want an error")
	}
	if strings.Contains(err.Error(), "sk-or-secret") {
		t.Errorf("status leaked upstream detail: %v", err)
	}
}

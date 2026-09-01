// Package grpcapi exposes the tagging service over gRPC.
//
// The directory is named grpc to match the REST sibling, but the package is
// grpcapi so it does not shadow google.golang.org/grpc at call sites.
package grpcapi

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ipedrazas/tagger/internal/tagging"
	taggerv1 "github.com/ipedrazas/tagger/proto/gen/tagger/v1"
)

// Tagger is the behaviour the gRPC layer needs from the tagging service.
type Tagger interface {
	Tag(ctx context.Context, text string) ([]string, error)
}

// Server implements the tagger.v1.Tagger service.
type Server struct {
	taggerv1.UnimplementedTaggerServer

	svc    Tagger
	logger *slog.Logger
}

// NewServer builds a gRPC service implementation around svc.
func NewServer(svc Tagger, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{svc: svc, logger: logger}
}

// Register wires the service into a grpc.Server.
func (s *Server) Register(gs grpc.ServiceRegistrar) {
	taggerv1.RegisterTaggerServer(gs, s)
}

// Tag implements taggerv1.TaggerServer.
func (s *Server) Tag(ctx context.Context, req *taggerv1.TagRequest) (*taggerv1.TagResponse, error) {
	tags, err := s.svc.Tag(ctx, req.GetText())
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &taggerv1.TagResponse{Tags: tags}, nil
}

// toStatus maps domain errors onto gRPC codes, mirroring the REST mapping.
// Upstream detail is logged rather than returned so provider messages stay
// server-side.
func (s *Server) toStatus(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, tagging.ErrEmptyText):
		return status.Error(codes.InvalidArgument, "text must not be empty")
	case errors.Is(err, tagging.ErrTextTooLarge):
		return status.Error(codes.InvalidArgument, "text exceeds maximum size")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		s.logger.ErrorContext(ctx, "tag request timed out", slog.String("error", err.Error()))
		return status.Error(codes.DeadlineExceeded, "tagging timed out")
	case errors.Is(err, tagging.ErrUpstream), errors.Is(err, tagging.ErrUnparseable):
		s.logger.ErrorContext(ctx, "upstream tagging failure", slog.String("error", err.Error()))
		return status.Error(codes.Unavailable, "tagging provider unavailable")
	default:
		s.logger.ErrorContext(ctx, "unexpected tagging failure", slog.String("error", err.Error()))
		return status.Error(codes.Internal, "internal error")
	}
}

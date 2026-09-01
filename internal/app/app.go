// Package app wires the configuration, the LLM client, the tagging service and
// both transports into a single runnable unit.
//
// Keeping this out of main lets the integration tests start the real stack on
// ephemeral ports instead of re-implementing the wiring.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	grpcapi "github.com/ipedrazas/tagger/internal/api/grpc"
	"github.com/ipedrazas/tagger/internal/api/rest"
	"github.com/ipedrazas/tagger/internal/config"
	"github.com/ipedrazas/tagger/internal/llm"
	"github.com/ipedrazas/tagger/internal/tagging"
	taggerv1 "github.com/ipedrazas/tagger/proto/gen/tagger/v1"
)

// shutdownGrace bounds how long in-flight requests get to finish.
const shutdownGrace = 15 * time.Second

// App is a configured, listening but not yet serving instance of the service.
type App struct {
	cfg       config.Config
	logger    *slog.Logger
	httpSrv   *http.Server
	httpLis   net.Listener
	grpcSrv   *grpc.Server
	grpcLis   net.Listener
	healthSrv *health.Server
}

// New builds the service and binds both listeners. Binding here rather than in
// Run means callers can read the effective addresses before serving, which is
// what makes port 0 usable in tests. ctx governs only the bind, not the
// subsequent serving; pass the serving context to Run.
func New(ctx context.Context, cfg config.Config, version string, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}

	client, err := llm.New(llm.Options{
		BaseURL: cfg.OpenRouterBaseURL,
		APIKey:  cfg.OpenRouterAPIKey,
		Model:   cfg.OpenRouterModel,
		Timeout: cfg.RequestTimeout,
		AppURL:  cfg.AppURL,
		AppName: cfg.AppName,
		Logger:  logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build openrouter client: %w", err)
	}

	svc := tagging.NewService(client, tagging.Options{
		MaxTags:      cfg.MaxTags,
		MaxTextBytes: cfg.MaxTextBytes,
	})

	handler := withTimeout(cfg.RequestTimeout, rest.NewHandler(svc, rest.Options{
		Version: version,
		// Leave headroom over MaxTextBytes for JSON escaping overhead.
		MaxBodyBytes: int64(cfg.MaxTextBytes) * 2,
		Logger:       logger,
	}))

	var lc net.ListenConfig
	httpLis, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		return nil, fmt.Errorf("listen http on %s: %w", cfg.HTTPAddr, err)
	}
	grpcLis, err := lc.Listen(ctx, "tcp", cfg.GRPCAddr)
	if err != nil {
		_ = httpLis.Close()
		return nil, fmt.Errorf("listen grpc on %s: %w", cfg.GRPCAddr, err)
	}

	grpcSrv := grpc.NewServer(grpc.ChainUnaryInterceptor(
		timeoutInterceptor(cfg.RequestTimeout),
		loggingInterceptor(logger),
	))
	grpcapi.NewServer(svc, logger).Register(grpcSrv)

	healthSrv := health.NewServer()
	healthSrv.SetServingStatus(
		taggerv1.Tagger_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)

	// Reflection lets grpcurl drive the API without a local copy of the
	// descriptors.
	reflection.Register(grpcSrv)

	return &App{
		cfg:    cfg,
		logger: logger,
		httpSrv: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      cfg.RequestTimeout + 10*time.Second,
			IdleTimeout:       120 * time.Second,
		},
		httpLis:   httpLis,
		grpcSrv:   grpcSrv,
		grpcLis:   grpcLis,
		healthSrv: healthSrv,
	}, nil
}

// HTTPAddr returns the address the REST server is bound to.
func (a *App) HTTPAddr() string { return a.httpLis.Addr().String() }

// GRPCAddr returns the address the gRPC server is bound to.
func (a *App) GRPCAddr() string { return a.grpcLis.Addr().String() }

// Run serves both transports until ctx is cancelled or either server fails,
// then shuts both down gracefully.
func (a *App) Run(ctx context.Context) error {
	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		a.logger.Info("http server listening", slog.String("addr", a.HTTPAddr()))
		if err := a.httpSrv.Serve(a.httpLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})

	group.Go(func() error {
		a.logger.Info("grpc server listening",
			slog.String("addr", a.GRPCAddr()), slog.String("model", a.cfg.OpenRouterModel))
		if err := a.grpcSrv.Serve(a.grpcLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpc server: %w", err)
		}
		return nil
	})

	// Shut both servers down as soon as either a signal arrives or one of them
	// fails, so the process never lingers half-serving.
	group.Go(func() error {
		<-groupCtx.Done()
		return a.shutdown()
	})

	return group.Wait()
}

func (a *App) shutdown() error {
	a.logger.Info("shutting down")
	a.healthSrv.SetServingStatus(
		taggerv1.Tagger_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_NOT_SERVING)

	// Detached from the (already cancelled) run context so the grace period is
	// actually honoured.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		a.grpcSrv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		a.logger.Warn("grpc graceful stop timed out, forcing")
		a.grpcSrv.Stop()
	}

	if err := a.httpSrv.Shutdown(ctx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}

// withTimeout bounds every HTTP request's context, so a slow model cannot pin
// a connection open indefinitely.
func withTimeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func timeoutInterceptor(d time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return handler(ctx, req)
	}
}

func loggingInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		logger.InfoContext(ctx, "grpc request",
			slog.String("method", info.FullMethod),
			slog.Bool("ok", err == nil),
			slog.Duration("duration", time.Since(start)),
		)
		return resp, err
	}
}

// Command video-service is the gRPC video probing microservice (CON-148). It
// serves video.v1.VideoService (Probe) backed by ffmpeg/ffprobe, plus a
// standard grpc.health.v1 endpoint. It is internal-only — the Ogen API reaches
// it over the private network.
package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	videov1 "github.com/ogen-app/video-service/gen/video/v1"
	"github.com/ogen-app/video-service/internal/config"
	"github.com/ogen-app/video-service/internal/logging"
	"github.com/ogen-app/video-service/internal/runtimetune"
	"github.com/ogen-app/video-service/internal/server"
	"github.com/ogen-app/video-service/internal/videoengine"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// Pre-logger: config drives the logger's level/format, so a load
		// failure can only report through the stdlib default (CON-107 keeps
		// boot log.Fatal*).
		log.Fatalf("video-service: config: %v", err)
	}

	logger := logging.New(cfg)

	// Bound the heap to the container and return burst-freed memory to the OS so
	// RSS tracks real usage instead of holding a high-water mark.
	runtimetune.Apply(logger, cfg)

	engine, err := videoengine.New(videoengine.Config{
		FFprobePath:    cfg.FFprobePath,
		FFmpegPath:     cfg.FFmpegPath,
		Workers:        cfg.Workers,
		ProbeTimeout:   cfg.ProbeTimeout,
		ScavengeOnIdle: cfg.ScavengeOnIdle,
	})
	if err != nil {
		logger.Error("init engine", "component", "boot", "err", err)
		os.Exit(1)
	}
	defer engine.Close()

	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("listen", "component", "boot", "addr", cfg.Listen, "err", err)
		os.Exit(1)
	}

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(logging.UnaryServerInterceptor(logger)),
		grpc.ChainStreamInterceptor(logging.StreamServerInterceptor(logger)),
	)
	videov1.RegisterVideoServiceServer(srv, server.New(engine))

	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("video.v1.VideoService", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logger.Info("shutting down", "component", "boot")
		srv.GracefulStop()
	}()

	logger.Info("listening", "component", "boot", "addr", cfg.Listen, "workers", cfg.Workers)
	if err := srv.Serve(lis); err != nil {
		logger.Error("serve", "component", "boot", "err", err)
		os.Exit(1)
	}
}

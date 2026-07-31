// Package server implements the video.v1.VideoService gRPC service on top of
// the ffmpeg/ffprobe engine.
package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	videov1 "github.com/ogen-app/video-service/gen/video/v1"
	"github.com/ogen-app/video-service/internal/videoengine"
)

// Server implements videov1.VideoServiceServer.
type Server struct {
	videov1.UnimplementedVideoServiceServer
	engine *videoengine.Engine
}

func New(engine *videoengine.Engine) *Server { return &Server{engine: engine} }

// Probe reads the video at the request's source_url and returns its metadata
// plus (when requested) a poster frame. Unreadable input is a terminal
// InvalidArgument; transient faults are Internal so the caller can degrade.
func (s *Server) Probe(ctx context.Context, req *videov1.ProbeRequest) (*videov1.ProbeResponse, error) {
	url := strings.TrimSpace(req.GetSourceUrl())
	if url == "" {
		return nil, status.Error(codes.InvalidArgument, "source_url is required")
	}

	res, err := s.engine.Probe(ctx, url, req.GetRenderPoster())
	if err != nil {
		return nil, mapEngineErr(err)
	}

	slog.InfoContext(ctx, "probe complete",
		"component", "server.probe",
		"filename", req.GetFilename(),
		"duration_ms", res.DurationMs,
		"codec", res.Codec,
		"container", res.Container,
		"width", res.Width,
		"height", res.Height,
		"poster_bytes", len(res.PosterPNG),
	)
	return &videov1.ProbeResponse{
		DurationMs: res.DurationMs,
		Codec:      res.Codec,
		Container:  res.Container,
		Width:      int32(res.Width),
		Height:     int32(res.Height),
		Bitrate:    res.Bitrate,
		PosterPng:  res.PosterPNG,
	}, nil
}

// mapEngineErr turns an unreadable video into a terminal InvalidArgument (so the
// client doesn't retry); everything else is Internal (transient). A context
// deadline/cancel surfaces as its own code so callers see the timeout.
func mapEngineErr(err error) error {
	if errors.Is(err, videoengine.ErrInvalidVideo) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

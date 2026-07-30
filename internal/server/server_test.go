package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	videov1 "github.com/ogen-app/video-service/gen/video/v1"
	"github.com/ogen-app/video-service/internal/videoengine"
)

func TestProbe_EmptyURLIsInvalidArgument(t *testing.T) {
	// The url check short-circuits before the engine is touched, so a nil
	// engine is safe here.
	s := New(nil)
	_, err := s.Probe(context.Background(), &videov1.ProbeRequest{SourceUrl: "   "})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty source_url must be InvalidArgument, got %v", err)
	}
}

func TestMapEngineErr(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want codes.Code
	}{
		{"invalid video", fmt.Errorf("%w: no video stream", videoengine.ErrInvalidVideo), codes.InvalidArgument},
		{"deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), codes.DeadlineExceeded},
		{"canceled", fmt.Errorf("wrap: %w", context.Canceled), codes.Canceled},
		{"transient", errors.New("ffprobe failed: connection refused"), codes.Internal},
	}
	for _, tc := range cases {
		if got := status.Code(mapEngineErr(tc.in)); got != tc.want {
			t.Errorf("%s: mapEngineErr code = %v, want %v", tc.name, got, tc.want)
		}
	}
}

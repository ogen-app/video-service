// Package videoengine probes a video and captures a poster frame by shelling
// out to ffprobe/ffmpeg (CON-148). It is the video counterpart of
// pdf-service's pdfengine.
//
// Backend: the ffmpeg/ffprobe binaries (found on PATH by default). ffprobe
// reads the metadata (duration/codec/container/resolution/bitrate); ffmpeg
// captures a single representative frame as a downscaled PNG. Both read the
// source URL directly over HTTP, range-requesting only what they need, so the
// service never buffers the whole (possibly multi-GB) file. A worker semaphore
// bounds how many external processes run at once.
package videoengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidVideo marks input ffprobe could not decode as a video (corrupt,
// truncated, audio-only, or not a media file). The gRPC layer maps it to
// InvalidArgument so the client treats it as terminal (no retry). Everything
// else (network, timeout, missing binary) is transient → Internal.
var ErrInvalidVideo = errors.New("videoengine: invalid or unreadable video")

// maxPosterBytes caps the poster PNG embedded in ProbeResponse. It is kept
// under gRPC's default 4 MiB receive-message limit so a client that hasn't
// raised MaxCallRecvMsgSize can still receive the response — we don't rely on
// a client-side override. An oversize frame is dropped (renderPoster returns
// nil) and the metadata response is still sent without a poster. Downscaling to
// <=1280px keeps a real single frame far under this.
const maxPosterBytes = 3 << 20 // 3 MiB, safely below the 4 MiB gRPC default

// Engine runs ffprobe/ffmpeg, bounded by a worker semaphore.
type Engine struct {
	ffprobe      string
	ffmpeg       string
	probeTimeout time.Duration
	sem          chan struct{}
}

// Config tunes the engine.
type Config struct {
	FFprobePath  string
	FFmpegPath   string
	Workers      int
	ProbeTimeout time.Duration
}

// New resolves the ffprobe/ffmpeg binaries and initialises the worker pool. It
// fails fast if either binary is missing so a misconfigured deploy surfaces at
// boot, not on the first request.
func New(cfg Config) (*Engine, error) {
	ffprobe := orDefault(cfg.FFprobePath, "ffprobe")
	ffmpeg := orDefault(cfg.FFmpegPath, "ffmpeg")
	if _, err := exec.LookPath(ffprobe); err != nil {
		return nil, fmt.Errorf("videoengine: ffprobe not found (%q): %w", ffprobe, err)
	}
	if _, err := exec.LookPath(ffmpeg); err != nil {
		return nil, fmt.Errorf("videoengine: ffmpeg not found (%q): %w", ffmpeg, err)
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = 4
	}
	timeout := cfg.ProbeTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &Engine{
		ffprobe:      ffprobe,
		ffmpeg:       ffmpeg,
		probeTimeout: timeout,
		sem:          make(chan struct{}, workers),
	}, nil
}

// Close is a no-op; the process-based backend holds no long-lived resources.
// Present for symmetry with pdfengine so cmd/ wiring is identical.
func (e *Engine) Close() error { return nil }

// Result is the probed metadata plus an optional poster frame.
type Result struct {
	DurationMs int64
	Codec      string
	Container  string
	Width      int
	Height     int
	Bitrate    int64
	PosterPNG  []byte // empty if not requested / capture failed
}

// Probe reads the video at sourceURL and returns its metadata, plus a poster
// frame when renderPoster is set. The whole call holds one worker slot and is
// bounded by the engine's probe timeout.
func (e *Engine) Probe(ctx context.Context, sourceURL string, renderPoster bool) (*Result, error) {
	if strings.TrimSpace(sourceURL) == "" {
		return nil, fmt.Errorf("%w: empty source url", ErrInvalidVideo)
	}

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, e.probeTimeout)
	defer cancel()

	meta, err := e.ffprobeMeta(ctx, sourceURL)
	if err != nil {
		return nil, err
	}
	res, err := meta.toResult()
	if err != nil {
		return nil, err
	}
	if renderPoster {
		// Best-effort: a poster failure never fails the probe.
		res.PosterPNG = e.renderPoster(ctx, sourceURL, res.DurationMs)
	}
	return res, nil
}

// ffprobeMeta runs ffprobe -show_format -show_streams and decodes the JSON.
func (e *Engine) ffprobeMeta(ctx context.Context, url string) (*ffprobeOutput, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.ffprobe,
		"-v", "error",
		"-hide_banner",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		"-i", url,
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("videoengine: ffprobe: %w", ctx.Err())
		}
		return nil, classifyProbeErr(stderr.String(), err)
	}
	var out ffprobeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		// ffprobe exited 0 but its output isn't decodable — an anomaly in the
		// tool's own output, not evidence of content corruption. Treat it as
		// transient (plain error -> Internal) so a valid upload isn't wrongly
		// rejected; terminal ErrInvalidVideo is reserved for recognized content
		// markers from classifyProbeErr.
		return nil, fmt.Errorf("videoengine: ffprobe output not decodable: %w", err)
	}
	return &out, nil
}

// renderPoster captures a single frame as a downscaled PNG. Returns nil on any
// failure — the caller keeps the metadata result regardless.
func (e *Engine) renderPoster(ctx context.Context, url string, durationMs int64) []byte {
	// Seek a little into the file for a non-black frame when the clip is long
	// enough; otherwise start at 0.
	seek := "0"
	if durationMs > 2000 {
		seek = "1"
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, e.ffmpeg,
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
		"-ss", seek,
		"-i", url,
		"-frames:v", "1",
		// Downscale to a sensible thumbnail width, preserving aspect and never
		// upscaling; -2 keeps height even for the encoder.
		"-vf", "scale='min(1280,iw)':-2",
		"-f", "image2",
		"-c:v", "png",
		"pipe:1",
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Poster capture is best-effort; log at debug so the failure is
		// diagnosable without noising up a normal probe.
		slog.DebugContext(ctx, "poster render failed",
			"component", "videoengine.poster",
			"err", err,
			"stderr", strings.TrimSpace(stderr.String()),
		)
		return nil
	}
	if b := stdout.Bytes(); len(b) > 0 && len(b) <= maxPosterBytes {
		return b
	}
	return nil
}

// ffprobeOutput is the subset of ffprobe -show_format/-show_streams JSON we use.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeStream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Duration  string `json:"duration"`
}

type ffprobeFormat struct {
	FormatName string `json:"format_name"`
	Duration   string `json:"duration"`
	BitRate    string `json:"bit_rate"`
}

// toResult projects ffprobe output into a Result, or ErrInvalidVideo when the
// input carries no decodable video stream.
func (o *ffprobeOutput) toResult() (*Result, error) {
	var vs *ffprobeStream
	for i := range o.Streams {
		if o.Streams[i].CodecType == "video" && o.Streams[i].Width > 0 && o.Streams[i].Height > 0 {
			vs = &o.Streams[i]
			break
		}
	}
	if vs == nil {
		return nil, fmt.Errorf("%w: no video stream", ErrInvalidVideo)
	}

	res := &Result{
		Codec:     vs.CodecName,
		Container: o.Format.FormatName,
		Width:     vs.Width,
		Height:    vs.Height,
		Bitrate:   parseInt64(o.Format.BitRate),
	}
	// Prefer the container duration; fall back to the stream's.
	if ms := parseSecondsToMs(o.Format.Duration); ms > 0 {
		res.DurationMs = ms
	} else {
		res.DurationMs = parseSecondsToMs(vs.Duration)
	}
	return res, nil
}

// classifyProbeErr decides whether an ffprobe failure is a terminal
// "not a video" verdict or a transient (network/timeout/binary) fault. It is
// deliberately conservative: only well-known content-corruption markers are
// terminal, so a transient blip never wrongly rejects a valid upload — the API
// then degrades gracefully (keeps the upload, no metadata).
func classifyProbeErr(stderr string, runErr error) error {
	s := strings.ToLower(stderr)
	transient := []string{
		"connection refused", "could not resolve", "failed to resolve",
		"connection reset", "connection timed out", "timed out", "no route to host",
		"network is unreachable", "server returned 4", "server returned 5",
		"http error", "tls", "i/o error", "protocol not found",
	}
	for _, m := range transient {
		if strings.Contains(s, m) {
			return fmt.Errorf("videoengine: ffprobe transient: %s", firstLine(stderr, runErr))
		}
	}
	invalid := []string{
		"invalid data found", "moov atom not found", "could not find codec parameters",
		"end of file", "unknown format", "header missing", "truncat", "invalid argument",
	}
	for _, m := range invalid {
		if strings.Contains(s, m) {
			return fmt.Errorf("%w: %s", ErrInvalidVideo, firstLine(stderr, runErr))
		}
	}
	// Unknown ffprobe failure: treat as transient so a valid upload is never
	// wrongly rejected.
	return fmt.Errorf("videoengine: ffprobe failed: %s", firstLine(stderr, runErr))
}

func firstLine(stderr string, runErr error) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		if runErr != nil {
			return runErr.Error()
		}
		return "unknown error"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func parseSecondsToMs(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(f * 1000)
}

func parseInt64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

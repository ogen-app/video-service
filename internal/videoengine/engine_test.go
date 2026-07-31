package videoengine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToResult_NoVideoStreamIsInvalid(t *testing.T) {
	// Audio-only ffprobe output → terminal invalid.
	out := &ffprobeOutput{
		Streams: []ffprobeStream{{CodecType: "audio", CodecName: "aac"}},
		Format:  ffprobeFormat{FormatName: "mp3", Duration: "12.5"},
	}
	if _, err := out.toResult(); !errors.Is(err, ErrInvalidVideo) {
		t.Fatalf("audio-only must be ErrInvalidVideo, got %v", err)
	}
}

func TestToResult_PicksVideoStream(t *testing.T) {
	out := &ffprobeOutput{
		Streams: []ffprobeStream{
			{CodecType: "audio", CodecName: "aac"},
			{CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080, Duration: "10.0"},
		},
		Format: ffprobeFormat{FormatName: "mov,mp4,m4a,3gp,3g2,mj2", Duration: "10.5", BitRate: "2500000"},
	}
	res, err := out.toResult()
	if err != nil {
		t.Fatalf("toResult: %v", err)
	}
	if res.Codec != "h264" || res.Width != 1920 || res.Height != 1080 {
		t.Errorf("stream fields wrong: %+v", res)
	}
	if res.Container != "mov,mp4,m4a,3gp,3g2,mj2" {
		t.Errorf("container = %q", res.Container)
	}
	if res.DurationMs != 10500 { // format duration wins
		t.Errorf("duration = %d, want 10500", res.DurationMs)
	}
	if res.Bitrate != 2500000 {
		t.Errorf("bitrate = %d", res.Bitrate)
	}
}

func TestClassifyProbeErr(t *testing.T) {
	if err := classifyProbeErr("Invalid data found when processing input", nil); !errors.Is(err, ErrInvalidVideo) {
		t.Errorf("corrupt content must be terminal, got %v", err)
	}
	if err := classifyProbeErr("moov atom not found", nil); !errors.Is(err, ErrInvalidVideo) {
		t.Errorf("moov atom missing must be terminal, got %v", err)
	}
	// Network faults are transient (NOT ErrInvalidVideo) so the API degrades.
	if err := classifyProbeErr("Connection refused", nil); errors.Is(err, ErrInvalidVideo) {
		t.Errorf("network fault must be transient, got %v", err)
	}
	if err := classifyProbeErr("Server returned 503 Service Unavailable", nil); errors.Is(err, ErrInvalidVideo) {
		t.Errorf("5xx must be transient, got %v", err)
	}
	// Unknown failures default to transient.
	if err := classifyProbeErr("some unrecognised ffprobe complaint", nil); errors.Is(err, ErrInvalidVideo) {
		t.Errorf("unknown must default transient, got %v", err)
	}
	// Ambiguous markers must NOT be terminal: a truncated transfer, an ffmpeg
	// option/EINVAL error, or a recoverable "Truncating packet" warning must
	// degrade gracefully rather than reject a possibly-valid upload.
	for _, msg := range []string{"End of file", "Invalid argument", "Truncating packet of size 100 to 40"} {
		if err := classifyProbeErr(msg, nil); errors.Is(err, ErrInvalidVideo) {
			t.Errorf("%q must be transient, got %v", msg, err)
		}
	}
}

func TestParseHelpers(t *testing.T) {
	if got := parseSecondsToMs("10.5"); got != 10500 {
		t.Errorf("parseSecondsToMs(10.5) = %d", got)
	}
	if got := parseSecondsToMs("N/A"); got != 0 {
		t.Errorf("parseSecondsToMs(N/A) = %d, want 0", got)
	}
	if got := parseInt64("2500000"); got != 2500000 {
		t.Errorf("parseInt64 = %d", got)
	}
	if got := parseInt64("N/A"); got != 0 {
		t.Errorf("parseInt64(N/A) = %d, want 0", got)
	}
}

// TestProbeEndToEnd generates a real 2s test-pattern clip with ffmpeg and
// probes it. Skipped when ffmpeg/ffprobe aren't installed (e.g. minimal CI).
func TestProbeEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	// 2s, 320x240, 30fps H.264 test pattern.
	gen := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=30",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("could not generate test clip (codec unavailable?): %v: %s", err, out)
	}

	eng, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := eng.Probe(ctx, src, true)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Width != 320 || res.Height != 240 {
		t.Errorf("resolution = %dx%d, want 320x240", res.Width, res.Height)
	}
	if res.Codec != "h264" {
		t.Errorf("codec = %q, want h264", res.Codec)
	}
	if res.DurationMs < 1500 || res.DurationMs > 2500 {
		t.Errorf("duration = %dms, want ~2000", res.DurationMs)
	}
	if len(res.PosterPNG) == 0 {
		t.Error("expected a poster frame")
	}
	// A valid poster must begin with the full 8-byte PNG signature. Compare
	// unconditionally so a short or malformed payload fails rather than
	// slipping past the check.
	pngSignature := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if len(res.PosterPNG) < len(pngSignature) || !bytes.Equal(res.PosterPNG[:len(pngSignature)], pngSignature) {
		t.Errorf("poster is not a PNG (len=%d): % x", len(res.PosterPNG), res.PosterPNG[:min(len(res.PosterPNG), len(pngSignature))])
	}
}

func TestProbe_InvalidContentIsTerminal(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "notavideo.mp4")
	if err := os.WriteFile(bad, []byte("this is definitely not a video file"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	eng, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := eng.Probe(context.Background(), bad, false); !errors.Is(err, ErrInvalidVideo) {
		t.Fatalf("garbage input must be ErrInvalidVideo, got %v", err)
	}
}

func TestFirstLineRedactsPresignedURL(t *testing.T) {
	stderr := "https://bucket.s3.amazonaws.com/t/abc/key.mp4?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLE&X-Amz-Signature=deadbeefcafe: Invalid data found when processing input\nsecond line"
	got := firstLine(stderr, nil)

	// No credential material may survive.
	for _, secret := range []string{"X-Amz-Signature", "deadbeefcafe", "X-Amz-Credential", "AKIAEXAMPLE"} {
		if strings.Contains(got, secret) {
			t.Fatalf("credential %q leaked into %q", secret, got)
		}
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("expected a redaction marker, got %q", got)
	}
	// The diagnostic message is preserved, and only the first line is returned.
	if !strings.Contains(got, "Invalid data found") {
		t.Errorf("message dropped: %q", got)
	}
	if strings.Contains(got, "second line") {
		t.Errorf("first-line behavior not preserved: %q", got)
	}
	// A message with no URL is returned unchanged.
	if got := firstLine("moov atom not found", nil); got != "moov atom not found" {
		t.Errorf("non-URL message must pass through, got %q", got)
	}
}

func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{limit: 4}

	// Under-limit write accumulates fully, no overflow.
	if n, _ := c.Write([]byte("ab")); n != 2 || c.overflow || string(c.Bytes()) != "ab" {
		t.Fatalf("under-limit: n=%d overflow=%v bytes=%q", n, c.overflow, c.Bytes())
	}

	// A write crossing the limit reports its full length (so the process isn't
	// stopped) but only retains up to limit and flags overflow.
	if n, _ := c.Write([]byte("cdef")); n != 4 {
		t.Fatalf("Write must report full length, got %d", n)
	}
	if !c.overflow {
		t.Fatal("expected overflow after crossing the limit")
	}
	if string(c.Bytes()) != "abcd" {
		t.Fatalf("buffer must cap at limit, got %q", c.Bytes())
	}

	// Further data past the limit is discarded; length stays at limit.
	if n, _ := c.Write([]byte("ghi")); n != 3 || len(c.Bytes()) != 4 {
		t.Fatalf("past-limit write should discard: n=%d len=%d", n, len(c.Bytes()))
	}
}

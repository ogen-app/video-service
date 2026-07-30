// Package config loads video-service settings from the environment via envconfig.
package config

import (
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Config holds the service settings. Read from the environment with no global
// prefix (each field names its own var), matching the Ogen API's config style.
type Config struct {
	// Listen is the gRPC listen address. Accepts a full "host:port" or a bare
	// port (e.g. "50051", as Railway's PORT provides) — Load normalises a bare
	// port to ":port" so net.Listen gets the leading colon it requires.
	Listen string `envconfig:"VIDEO_SERVICE_LISTEN" default:":50051"`
	// Workers bounds how many ffprobe/ffmpeg processes run concurrently. Unlike
	// pdf-service's single-threaded pdfium, ffmpeg is process-based so real
	// parallelism is possible; this caps it so a burst can't exhaust the box.
	Workers int `envconfig:"VIDEO_SERVICE_WORKERS" default:"4"`
	// ProbeTimeout bounds a single ffprobe+poster run. A remote URL that
	// stalls (slow origin, huge moov atom) is aborted rather than pinning a
	// worker slot. The API also applies its own per-call deadline.
	ProbeTimeout time.Duration `envconfig:"VIDEO_SERVICE_PROBE_TIMEOUT" default:"90s"`
	// FFprobePath / FFmpegPath are the binary names or absolute paths. Defaults
	// resolve on PATH; override only for non-standard installs.
	FFprobePath string `envconfig:"FFPROBE_PATH" default:"ffprobe"`
	FFmpegPath  string `envconfig:"FFMPEG_PATH"  default:"ffmpeg"`
	// LogLevel is the minimum slog level: debug|info|warn|error. Unknown/empty
	// falls back to info. Bare LOG_LEVEL (not prefixed) matches the Ogen API's
	// knob so operators use identical settings across services (CON-107).
	LogLevel string `envconfig:"LOG_LEVEL" default:"info"`
	// LogFormat selects the slog handler: json (default, prod) or text (local).
	LogFormat string `envconfig:"LOG_FORMAT" default:"json"`
}

// Load reads and validates the configuration from the environment.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return nil, err
	}
	// A bare port like "50051" is a valid env value (Railway's PORT) but net.Listen
	// needs "host:port" — without a colon it errors "missing port in address".
	// Prefix it so the service binds all interfaces on that port.
	if c.Listen != "" && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	return &c, nil
}

# video-service

gRPC video probing microservice for Ogen (CON-148). It is the video counterpart
of [pdf-service](https://github.com/ogen-app/pdf-service): the Ogen API hands it
a short-lived presigned GET URL, and it returns the video's
duration/codec/container/resolution/bitrate plus an optional poster frame.

Stateless and **internal-only** — the API reaches it over the private network
(no public port). It is the *structural validator* for video uploads: a file
that ffprobe can't decode comes back as a terminal error, so the API rejects the
upload instead of letting it dead-end in Draft.

## Contract

`video.v1.VideoService` (see [`proto/video/v1/video.proto`](proto/video/v1/video.proto)):

```protobuf
rpc Probe(ProbeRequest) returns (ProbeResponse)
```

- **Request:** `source_url` (presigned GET URL), `render_poster` (bool), `filename`.
- **Response:** `duration_ms`, `codec`, `container`, `width`, `height`, `bitrate`, `poster_png`.

`Probe` is **unary**: ffprobe/ffmpeg read the URL directly over HTTP and
range-request only the bytes they need, so a multi-GB file never streams through
either the service or the API process. (pdf-service, by contrast, client-streams
the whole file.)

### Error semantics

| Condition | gRPC code | API behaviour |
|-----------|-----------|---------------|
| Not a decodable video (corrupt, truncated, audio-only) | `InvalidArgument` | reject upload (terminal 400/415) |
| Network / timeout / transient | `Internal` / `DeadlineExceeded` | keep upload, skip metadata (graceful degradation) |
| Empty `source_url` | `InvalidArgument` | — |

The classifier is deliberately conservative: only well-known content-corruption
markers are terminal, so a transient blip never wrongly rejects a valid upload.

Also serves standard `grpc.health.v1.Health` for orchestrator probes.

## Configuration

Environment variables (no prefix, matching the Ogen API's style):

| Var | Default | Purpose |
|-----|---------|---------|
| `VIDEO_SERVICE_LISTEN` | `:50051` | gRPC listen address (bare port accepted) |
| `VIDEO_SERVICE_WORKERS` | `4` | max concurrent ffprobe/ffmpeg processes |
| `VIDEO_SERVICE_PROBE_TIMEOUT` | `90s` | per-probe upper bound |
| `FFPROBE_PATH` | `ffprobe` | ffprobe binary (name or path) |
| `FFMPEG_PATH` | `ffmpeg` | ffmpeg binary (name or path) |
| `VIDEO_SERVICE_GC_PERCENT` | `50` | GC target (GOGC); lower = smaller heap, more CPU. `<=0` keeps the runtime default |
| `VIDEO_SERVICE_MEMORY_LIMIT_RATIO` | `0.9` | soft mem limit (GOMEMLIMIT) as a fraction of the cgroup limit; ignored if `GOMEMLIMIT` is set or no cgroup limit is found |
| `VIDEO_SERVICE_SCAVENGE_ON_IDLE` | `true` | return freed memory to the OS once the worker pool drains after a burst |
| `LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `LOG_FORMAT` | `json` | `json` (prod) or `text` (local) |

### Memory footprint

The service holds no state across probes — every buffer is request-local and
bounded (see `videoengine`). What container metrics show as a memory "leak" is
usually the Go runtime and cgroup holding a **reclaimable** high-water mark
after a burst: RSS steps up and stays flat rather than being freed. The three
knobs above address that — `GOMEMLIMIT` (derived from the cgroup limit) caps the
heap, `GOGC` collects more often, and the idle scavenge returns freed pages to
the OS once the worker pool drains — so RSS tracks real usage. A genuine leak
looks different: a rising staircase across successive bursts, not a plateau.

## Build & run

The service shells out to **ffmpeg/ffprobe** (built with HTTPS support), so no
CGO is needed. The Docker runtime image installs ffmpeg from Debian.

```sh
buf generate proto        # regenerate gen/ after editing the proto
go build ./cmd/video-service
go test ./...             # end-to-end videoengine tests run when ffmpeg is present, else skip

docker build -t video-service .
```

The API reaches it at `video-service:50051` in compose (behind the `video`
profile) / `video-service.railway.internal:50051` in prod, via
`VIDEO_SERVICE_ADDR` on the API side.

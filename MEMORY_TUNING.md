# Memory tuning (Railway)

How to set the runtime-memory knobs for a **lightly-loaded** `video-service` on
Railway where **memory consumption translates directly to budget**.

## Billing model that drives these choices

Railway bills **actual measured usage** (GB-hours of real RSS + vCPU-hours), not
the plan cap or any limit you configure. A lightly-loaded service is idle for the
vast majority of its hours, so the **idle baseline dominates the bill** — not the
rare burst peak. The goal is therefore to keep idle RSS low, not just to cap the
peak.

The service holds no state across probes (every buffer is request-local and
bounded), so the "leak" seen in metrics is reclaimable memory the Go
runtime/cgroup hasn't returned — a flat plateau after a burst, not a rising
staircase. The knobs below make RSS track real usage.

## Recommended values

```bash
VIDEO_SERVICE_SCAVENGE_ON_IDLE=true    # the actual money-saver
VIDEO_SERVICE_GC_PERCENT=50            # keep default; minor lever here
VIDEO_SERVICE_MEMORY_LIMIT_RATIO=0.7   # safety net, tune to your cap (see below)
```

### `SCAVENGE_ON_IDLE=true` — the one that saves money

Railway bills measured RSS over time, and the service is idle most hours. The
idle scavenge (`debug.FreeOSMemory` once the worker pool drains) is what drops
the ~200 MB burst plateau back toward the Go baseline (~30–50 MB), so most
billing samples catch the low idle number. For a bursty/idle service this knob
does almost all the work. Keep it on.

### `GC_PERCENT=50` — minor lever, don't overthink

GOGC only affects the heap high-water mark **during activity**, and this
service's Go heap is small (a few MB posters + gRPC buffers; the heavy memory is
in the ffmpeg **child processes**, which GOGC doesn't touch). It has **no effect
on idle RSS** once the scavenge force-returns memory. 50 gives slightly lower
burst peaks than the default 100 at trivial CPU cost. Don't go below ~40 — that
just burns CPU (also billed) chasing an already-tiny heap.

### `MEMORY_LIMIT_RATIO` — a safety net, not a cost lever

This sets `GOMEMLIMIT`, a soft **ceiling** that prevents OOM; it does not reduce
average usage, so it doesn't directly cut the bill. Key nuance: **`GOMEMLIMIT`
governs only the Go heap — not the ffmpeg/ffprobe children**, which also count
against the container cgroup. Leave room for them:

| Your Railway memory limit | Suggested ratio | Reasoning |
|---|---|---|
| ≤ 1 GB | **0.6–0.7** | reserve room for concurrent ffmpeg children so a burst doesn't OOM |
| ≥ 4 GB (or unset) | 0.85–0.9 | child headroom is ample; ratio barely binds |

If you haven't set an explicit memory limit on the Railway service, `memory.max`
is likely your plan's large default, so `0.9 × huge` is effectively unlimited and
the ratio does nothing. In that case **set an explicit Railway memory limit**
(e.g. 512 MB, given the observed ~200 MB peak) — it costs nothing extra on
usage-based billing, gives a hard backstop against a runaway, and makes the
derived `GOMEMLIMIT` meaningful.

## Two bigger levers

- **`VIDEO_SERVICE_WORKERS`** (default 4): for a lightly-loaded service, dropping
  to **2** halves the peak concurrent ffmpeg memory — the real driver of burst
  RSS and OOM risk, and the part `GOMEMLIMIT` can't govern. Cleaner peak reducer
  than fiddling with the ratio if requests are rarely concurrent.
- **Idle baseline itself:** after deploy, check what the service floors at between
  bursts. If it's still > ~60 MB idle, the remaining cost is the always-on
  Go/gRPC baseline — the only further lever there is architectural (fewer
  replicas, or not keeping it always-on), not these knobs.

## Verify after deploy

Run a couple of probes and confirm memory returns toward baseline (reuse) rather
than climbing 200 → 400 → 600 MB (which would be a real leak). At
`LOG_LEVEL=debug`, each scavenge logs `component=videoengine.scavenge`; boot logs
the derived `GOMEMLIMIT` under `component=runtimetune`.

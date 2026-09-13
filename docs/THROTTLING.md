# Terminal Throttling / Slow-Terminal Output

Design and implementation doc for the "Throttle/discard slow terminal output"
release item tracked in `docs/TODO.md` (section 4) and `README.md` (WIP item 5).

**Status: IMPLEMENTED (2026-09-13).** Option A + `tail` policy confirmed by review;
`--max-log-queue` default 256 (see §9 — the bound is a library default, not a CLI
flag), `--columns` (default 0), `--output auto|term` are live and verified
end-to-end with `trebuchet` under a pty + slow reader.

Goal: ensure a slow terminal consumer (or a high-rate producer like
`tools/trebuchet`) cannot make the formation appear stuck, while keeping
process cancellation/reaping semantics intact.

---

## 1. Problem

`procman` logs every line of every process's `stdout`/`stderr` to a single
`*slog.Logger` whose handler is a `termhandler.TermHandler` writing to
`os.Stdout`, which may be a slow sink (a pipe to a slower reader, an attached
debugger, an SSH client under load, or a SIGSTOP'd terminal peer).

The write path is fully synchronous end-to-end:

1. `os/exec` runs a per-stream copy goroutine reading the child pipe
   (`c.Stdout`/`c.Stderr`) into an `io.Writer`
   (`pkg/process/process.go:186-187`, `c.Stdout = stdout` / `c.Stderr = stderr`).
2. That writer is a `*writelog.stream` (`pkg/writelog/writelog.go:18`):
   `stream.Write` buffers into a `bytes.Buffer` and, **per complete line**
   (at `writelog.go:28-34`), calls `sink.LogAttrs(...)` **synchronously**
   and **blocks** until it returns.
3. `TermHandler.Handle` (`pkg/termhandler/termhandler.go:108`) writes
   `linePrefix + rec.Message` to `h.out` **synchronously under `h.mu`**
   (`pkg/termhandler/termhandler.go:131-135`).
4. That write is the terminal write, i.e. the actual `write(2)`/`WriteFile`
   to `os.Stdout`.

So a slow terminal blocks `stream.Write`, which blocks the `os/exec` copy
goroutine, which blocks the child pipe from draining, which can stall the
**child process itself** if it writes output faster than the terminal accepts
it. The formation looks "hung" even though no process crashed.

`tools/trebuchet` (`tools/trebuchet/trebuchet.go`) only *measures* this: its
`blocked` field is the time around its own `slog.Info` call
(`trebuchet.go:27-32`), which is exactly the synchronous trip
subprocess write → `stream.Write` → `LogAttrs` → `TermHandler.Handle` →
`out.Write`. When `blocked > max-block` (default 15 ms) it aborts with exit 1
(`trebuchet.go:34-35`). There is currently **no throttle logic** that would
keep `blocked` under the limit by shedding load.

### Dead CLI surface that touches this

- `--output auto|term` is parsed in `cmd/procman/main.go:30` but never
  consulted; `TermHandler` is always constructed unconditionally at
  `cmd/procman/main.go:45`.
- `TermHandler.Options.Columns` (`termhandler.go:43`) is never set from the
  CLI, so it is always `0` and the truncation branch at
  `termhandler.go:121-124` never fires. No `--columns` flag exists.

---

## 2. Constraints

1. **Cancel/reaping semantics are non-negotiable.** The `errgroup` in
   `Formation.Run` (`pkg/process/process.go:104`) cancels the whole group
   context when any process exits; `c.Cancel` SIGKILLs the process group
   (`pkg/process/process.go:192`). `c.WaitDelay` is `1 * time.Second`
   (`pkg/process/process.go:188`); the `Setpgid`/process-group cancel model
   is at `process.go:194-196`. A throttle must never prevent this.
2. **Backpressure must not propagate to the child.** Once throttling
   engages, the child's `os/exec` copy goroutine must be able to return from
   `stream.Write` promptly so the child is not stalled by its own output.
3. **Ordering within a stream should be preserved** for the lines that *are*
   emitted (so logs stay readable), even if some are discarded.
4. **No data races.** The child pipe copy and the log drain run in separate
   goroutines; any buffer between them needs a mutex or a channel.
5. **Minimal scope.** Prefer not to change `TermHandler`'s rendering contract;
   the throttle is fundamentally about shedding *volume* between the child
   and the logger, not about terminal width.

---

## 3. Options

### Option A — Bounded async queue in `writelog` (recommended)

Introduce a bounded queue in `writelog.Stream`: `Write` enqueues a line and
returns immediately; a single draining worker dequeues and calls
`sink.LogAttrs`. When the queue is full, apply a **drop policy** so the child
is never blocked.

- **Decouples** the child pipe copy from the terminal write — the child can
  always hand its line to the queue and continue.
- **Keeps `TermHandler` unchanged.** It only sees the (already-throttled)
  line rate, so its synchronous terminal write is no longer on the critical
  path of the child.
- **One natural place** to define discard policy: at the enqueue point,
  where volume is being shed.
- **Close() semantics:** `Stream.Close()` (`writelog.go:40`) must flush the
  tail. With an async worker, `Close` signals the worker to drain remaining
  queued items and then returns (or optionally waits). The existing partial-
  line flush (`writelog.go:42-47`) is preserved.

Drop policy sub-variants (see §5 decision):
- **drop oldest** (FIFO drop) — simplest, keeps the latest window.
- **coalesce tail** — keep the *last* line of a burst, drop the middle, so
  the reader sees representative activity rather than a stale tail.
- **drop newest** — true backpressure (child still stalls) — rejected under
  constraint 2.

### Option B — Drop-oldest inside `TermHandler.Handle`

Bound the work inside `Handle` itself, e.g. a ring buffer of recent lines or
a fast-path that skips `out.Write` under backpressure.

- **Inferior decoupling:** `writelog.Stream` still calls `LogAttrs`
  synchronously (`writelog.go:33`), so `stream.Write` is still on the
  blocking path until the queue inside `Handle` fills — and *filling* a
  queue inside `Handle` requires `Handle` to not block, which means
  non-blocking enqueue *from Handle*, i.e. the queue already has to live
  somewhere async. This collapses into "Option A, but the queue is in the
  handler" — which is the wrong side of the `slog` boundary and harder to
  make cancel-safe (`Handle` is called by the slog framework; we'd be
  spawning/draining workers from within a log call).
- **Mixes concerns:** `TermHandler` is supposed to be a renderer (color,
  prefix, truncation), not a volume controller.

### Option C — Do nothing in code; document `--output` modes

Leave the throttle to the user (`--output auto`|`term` already exists but is
dead). Reject: it doesn't fix the hang, only papers over it by disabling
color when piped.

---

## 4. Chosen approach

**Option A** with these properties:

- Queue lives in `pkg/writelog` (one per `Stream`), bounded by a configurable
  depth (default tuned against `trebuchet`; see §5).
- Single draining worker per `Stream`; enqueue/dequeue via a mutex + ring
  buffer (or an unbuffered handoff with a non-blocking select — decide in
  implementation; `sync.Mutex` matches the existing `stream.mu` lock style
  at `writelog.go:14`).
- Discard policy applied at enqueue when full (default: see §5).
- `Stream.Close()` drains the in-flight queue before returning so the
  "final partial line" flush is not lost.
- `TermHandler.Options.Columns` wired to a `--columns` CLI flag so line
  truncation (`termhandler.go:121-124`) becomes live.
- `--output` flag made real: `auto` = `TermHandler` when stdout is a tty,
  plain `slog.TextHandler` otherwise; `term` = force the `TermHandler`
  (i.e. force color/prefixes even when piped). This does *not* add the queue;
  the queue is always present. `--output` is purely about *format*.

Rationale: the child-pipe copy goroutine (`os/exec`) is the producer we must
keep unblocked; `writelog.Stream` is the unique choke point every process
output passes through (`process.go:181-182`); putting the bounded queue
there satisfies constraints 1, 2, and 3 with the smallest blast radius and
without changing `TermHandler`'s rendering contract.

Note: `writelog.Stream` is currently constructed without options
(`pkg/process/process.go:181-182`), so the queue depth/policy must be plumbed
through `process.Stream`/`runOptions` or a new `StreamConfig` arg before it
reaches `writelog`.

---

## 5. Open decision (needs a call) — RESOLVED

**Discard policy + queue depth.** The TODO left this as a "pick in review" item;
**the call was made: `tail` policy (oldest-line eviction on a full ring) and
queue depth `DefaultMaxQueue` = 256** (per §6 recommendation, confirmed
2026-09-13). The depth is **not exposed as a CLI flag** — see §9 — and
`--columns` defaults to 0 (off) and `--output` to `auto`. There is no
unbounded mode; a config value `<= 0` falls back to `DefaultMaxQueue`.

The evaluation table that led to the decision:

| Policy | Behavior | When to prefer |
|---|---|---|
| `tail` | On full queue, evict the **oldest** line and append the new one | **Chosen.** Default. Preserves a recent window; cheap; predictable memory. Matches "drop oldest." |
| `coalesce` | On full queue, **replace the last queued line** with the new one (drop middle of a burst, keep latest) | High-rate single-source producer (e.g. one `trebuchet` spamming one tag); the reader sees the latest state rather than stale middle lines. |

Queue depth is independent and tunable. Proposed (then revised in §9): a
single `--max-log-queue` int flag (default `256`) applied to *every* `Stream`.
Depth is a memory bound; policy is a quality signal.

**Recommendation (to confirm):** `tail` policy, queue depth `256`,
`--columns` default `0` (off, preserving current behavior), `--output` default
`auto`. Flag the choice here and close it in review rather than hardcoding.
(Follow-up: the queue depth stayed a library default, not a CLI flag — §9.)

---

## 6. Tasks — DONE (2026-09-13)

- [x] Add a bounded queue + drain worker to `pkg/writelog.Stream`. `Write`
      enqueues per complete line, returns promptly. `Close` drains then
      flushes any partial line.
- [x] Implement the chosen discard policy behind a field in `Stream`/opts
      (`tail`: evict oldest on full ring).
- [x] Surface the queue depth from `cmd/procman/main.go`, plumbed through
      `Formation.LogQueueSize` → `withLogQueue` → `writelog.Stream`. **REVISED
      2026-09-13 (see §9): the CLI flag and this plumbing were removed**;
      `Formation`/`runOptions` always use the default, and `StreamConfig.MaxQueue`
      remains only as a library knob for embedders.
- [x] Wire `--output` in `cmd/procman/main.go`: `auto` = termhandler on tty
      else `TextHandler`; `term` = force `TermHandler`/color.
- [x] Wire `--columns` → `TermHandler.Options.Columns`; also fixed the latent
      `Handle` bug (truncation length was computed but the full buffer was
      written).
- [x] `writelog` test: saturate the queue and assert (a) `Write` does not
      block the producer, (b) tail-dropped lines are the oldest, (c) `Close`
      drains the tail and flushes the partial line.
- [x] `termhandler` tests: `--columns` truncation, forced colors, `IsTerminal`.
- [x] Acceptance: trebuchet (1000 msgs) under a pty with a slow reader
      completes; delivered lines bounded by queue depth + OS buffers (436 for
      queue=8 vs 689 for queue=256); no "blocked for" abort; fast sinks
      deliver everything (no spurious drops).
- [x] README: documents `--output`, `--columns` and the throttle tradeoff. See
      also `docs/TODO.md` §4.

---

## 7. Acceptance criteria

1. A high-rate single-tag producer (`trebuchet`) keeps `blocked` under
   `--max-block` once the queue is engaged (verified via `trebuchet`'s own
   exit code 1 path).
2. The child process is never stalled by its own output: increasing the
   producer rate does not indefinitely delay process exit/reaping.
3. `Ctrl-C` / sibling-exit cancellation still kills the formation promptly
   (errgroup cancel → `c.Cancel` SIGKILL), regardless of queue backlog.
4. `Stream.Close()` still flushes the final partial line
   (`pkg/writelog/writelog_test.go` `TestStreams` keeps passing).
5. `--columns N` truncates a `TermHandler` line to `N` bytes.
6. `make test` (including new tests above) is green; `go vet ./pkg/writelog
   ./pkg/termhandler ./pkg/process ./cmd/...` is clean.

---

## 8. Notes / non-goals

- This is **not** log rotation or long-term buffering; there is no on-disk
  sink. The queue is in-memory and bounded.
- This does **not** change `c.WaitDelay` (`process.go:188`, 1s) or the
  `Setpgid`/process-group cancel model (`process.go:194-196`). Those are
  correct as-is.
- `go test .` from the repo root reports "no Go files"
  (`docs/TODO.md` v1 readiness note); vet per-package.

---

## 9. Hot-path performance (2026-09-13)

Post-implementation pass on the `writelog → termhandler` path (analysis +
optimization). Benchmark source: `pkg/writelog/hotpath_bench_test.go`
(isolated `Write`, pure `TermHandler.Handle`, full term path, and a 16-stream
contention bench). CPU: i7-11700F, Go 1.26.

### Findings (pre-change)

- **Allocation churn / GC was the dominant cost.** The line split used
  `bytes.Buffer.ReadBytes`, which copies each complete line out of the buffer
  into a fresh slice, then `string()` copies it *again* → **2 allocs/line**.
  The alloc profile showed `ReadBytes`+`string` at **~99% of all
  allocations** (49.5% flat in `Write`, 49.5% in `ReadBytes`); `memmove` and
  `indexbytebody` were visible in the CPU profile from the redundant
  copying.
- **Lock contention under concurrency.** Per stream, `Write` held `s.mu` for
  the whole split phase *and then* re-acquired it **per line** in `push`
  (double lock). All streams from all processes share **one** `TermHandler`
  mutex and **one** fd (via the shallow copy in `WithGroup`) — a global
  serialization point. At 16 concurrent streams, ~30% of CPU was mutex
  machinery (`Lock`/`Unlock`/`lockSlow`/`procyield`) and `push` alone was
  18% cumulative; per-line cost ballooned from ~120 ns (single stream) to
  ~700–835 ns.
- **No drain batching.** The worker popped exactly one line per lock hold and
  made one `LogAttrs` call, so bursty output was mutex/cond-wake bound.

### Changes applied (`pkg/writelog/writelog.go`, behavior-preserving)

1. **Single-copy line split.** Replaced `bytes.Buffer.ReadBytes` with a direct
   `bytes.IndexByte` scan that converts each line straight to an immutable
   string → **1 alloc/line** (was 2). Public API unchanged.
2. **Batched enqueue + drain.** `push` → `pushAll` (entire batch enqueued under
   one lock hold + one cond-wake, still tail-dropping FIFO); `drain` pops up
   to `drainBatch` (32) lines per lock hold and logs them outside the lock.

### Results (16-way machine)

| Benchmark | Before | After | |
|---|---|---|---|
| `writelog.Write` (cheap sink) | 118 ns · 192 B · 2 alloc | 88 ns · 96 B · 1 alloc | −25% time, −50% alloc |
| Full term path (producer) | ~121 ns · 193 B · 2 alloc | 83 ns · 103 B · 1 alloc | −31% time, −50% alloc |
| 16-stream contention | ~695–835 ns · 3085 B | 535 ns · 1584 B | −23–36%, −49% alloc |

All unit tests, `go vet`, full `go test ./...`, and `-race` pass.

### Revision: queue depth not exposed as a CLI flag

The `--max-log-queue` flag (and `Formation.LogQueueSize` → `withLogQueue`
plumbing) was **removed**. Rationale: the exact bound has no user-tunable sweet
spot — any reasonable value keeps the child unblocked, and a larger queue just
holds more stale lines before tail-dropping (lengthening output lag under
sustained overload). The default `256` is fine. `writelog.StreamConfig.MaxQueue`
(and the `DefaultMaxQueue` fallback) remains as a **library** knob for
embedders and for deterministic tail-drop tests; `Formation` always uses the
default. The CLI now exposes only `--output` and `--columns` for the output
path.

### Why the shared writer lock is OK (design invariant)

The `TermHandler` mutex is a *serialization* point, **not** a *blocking* point
for child processes. `TermHandler.Handle` runs only inside a stream's drain
worker, i.e. downstream of the **bounded** writelog queue. When the sink is
wedged (slow terminal), the queue fills and **tail-drops** — the producer
(`stream.Write`, i.e. the `os/exec` copy goroutine) returns immediately and
never blocks on the lock. Consequence: high per-stream latency and cross-
stream serialization on the single fd are **accepted** so long as child
processes are never stalled by their own output and output volume is shed
instead of queued. Lock-contention latency is a quality-of-life cost, not a
correctness or liveness one.

### Remaining work (not urgent, deferred)

- **Write-batching coalescer for the shared fd.** The largest remaining
  contention cost is the many streams serializing individual `write(2)` calls
  through the one fd / one mutex. A future change could coalesce output from
  the N streams into a single buffered writer (fewer, larger syscalls). This
  is a larger behavior change and was intentionally **deferred**; it does not
  affect the no-process-blocking invariant above.
- The one remaining per-line allocation (the immutable string handed to the
  async ring) is required — a zero-copy view into the `os/exec` copy buffer
  is unsafe because the drain worker consumes lines after `Write` returns and
  the source buffer is reused. Recovering it needs a buffer pool with
  explicit lifetime tracking across the ring; not worth the risk now.
- Tracked in `docs/TODO.md` (hot-path perf).

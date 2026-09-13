# procman — Release TODO

Purpose: track everything that stands between the current state and a
shippable release. Mirror of the README "Work in Progress" list, cross-checked
against the code as of `d435510` (go mod update, 2026-09-07).

## Status summary

Procfile parsing now lives in its own package (`pkg/procfile`), extracted
from `pkg/process` (2026-09-07); parsing tweaks and their tests land there.

> Note: existing TODO items reference `pkg/process/process.go` line numbers;
> they shifted when `Formation.Load` slimmed down and may drift again with
> the feature work — check 
> `grep -n "func (l *Formation) Run" pkg/process/process.go` rather than
> trusting the numbers.

The README still lists five WIP items and opens with "You probably want
foreman instead for now" — the release is gated on closing out the ones that
are actually open, then refreshing the README so it describes reality.

| README WIP item | Status | Evidence |
|---|---|---|
| Tests, documentation | Mostly done | Unit tests in `pkg/*/**_test.go`; `AGENTS.md` exists; README itself is stale |
| Formation support (replicas per type) | **Not done** — flag is dead | `--formation` parsed in `cmd/procman/main.go:29` but never read; `Process.Index` never set (`pkg/process/process.go:26`) |
| Port allocation | **Not done** — no code | Nothing injects `PORT` anywhere |
| dotenv support | **Not done** — flag is dead | `--env`/`-e` parsed in `cmd/procman/main.go:28` but never read |
| Throttle/discard slow terminal output | **Done** (2026-09-13) | Bounded async queue in `writelog` (tail-drop, default 256); `--columns`, `--output auto|term` live (queue depth is a library default, not a CLI flag — see §5); `trebuchet` E2E-verified; see `docs/THROTTLING.md` |

Also dead in `main.go`: replaced by live flags in the throttle work — `Output`, `Columns` are now parsed and consumed.

Also dead in `main.go`: the `Output` field — never read after parse.

## Pending feature work

### 1. Formation support (replica counts)

**Current state:** `--formation` parses a `type=count` map but nothing consumes
it; `Formation.Run` (`pkg/process/process.go:100`) starts exactly one process
per Procfile line. `Process.Index` exists but is never assigned.

- [ ] Parse `--formation` in `cmd/procman/main.go` and pass it into `Formation`
      (new field, e.g. `Replicas` map from tag to count).
- [ ] Expand processes in `Formation.Run`: `web=2` spawns two `Process` entries;
      assign `Process.Index` (1-based).
- [ ] Decide replica log identity. `writelog.Stream` tags every line with the
      process tag (`pkg/writelog/writelog.go:48`) — replicas will collide on the
      same tag attr. Either use `web.1`/`web.2` as the tag (clear, changes
      `termhandler` group width behavior) or keep `web` and rely on Index only.
      Pick one and cover it with a test.
- [ ] Verify cancellation semantics: `errgroup` cancels the whole group when any
      process exits (see `TestProcess`), so replicas die together — confirm
      with a two-replica integration run.
- [ ] Unit test: expansion, Index assignment, and tag scheme
      (`pkg/process/process_test.go`).

### 2. Port allocation

**Current state:** nothing. No base port, no `PORT` env injection.

- [ ] Decide the strategy — Heroku assigns `$PORT` per dyno; here each replica
      needs a distinct port. Propose a deterministic scheme (e.g. base port
      `5000` + N for the Nth replica in the formation).
- [ ] Inject the chosen port into the child environment (prepend to
      `Process.Environ` so it lands in the env passed at
      `pkg/process/process.go:189` `c.Env = baseEnv(...)`).
- [ ] Make ports compose with formation replicas (item 1): `web=2` yields two
      distinct ports.
- [ ] Unit test the allocator (determinism, no collisions, range clamp); note
      the port in the README.
- [ ] Consider config surface: flag (e.g. `--port`) vs. convention. Default to
      a documented convention unless a flag is explicitly wanted.

### 3. dotenv support

**Current state:** `--env`/`-e` parses a path but nothing reads it.

- [ ] Implement a small dotenv parser (no dependency needed or add one —
      decide in review): `KEY=VALUE` lines, `#` comments, optional `export `
      prefix, quoted values, blank lines. Mirror the comment/blank-line
      skipping already used in `Formation.Load`.
- [ ] Wire `cmd/procman/main.go`: load the file, apply entries to every
      process's `Environ`.
- [ ] Define precedence explicitly: dotenv values are a base layer;
      `baseEnv` (host `PATH`/`HOME`/proxies, `process.go:142`) is appended
      after in `baseEnv(p.Environ...)` so dotenv wins on collision. Confirm and
      document this order.
- [ ] Unit tests for the parser (quotes, comments, `export`, blank lines) +
      one test asserting child env actually receives a dotenv variable
      (`pkg/process/process_test.go`).

### 4. Terminal throttle / discard on slow terminal — DONE (2026-09-13)

Implemented per `docs/THROTTLING.md` (design + implementation record).

- **Policy:** bounded async queue in `pkg/writelog.Stream` (ring buffer,
  `StreamConfig.MaxQueue`, default `DefaultMaxQueue` = 256); when full the
  oldest line is **tail-dropped** so the child is never back-pressured. A
  dedicated worker goroutine drains the queue to the sink; `Close()` waits for
  the drain then flushes a final partial line.
- **`--output auto|term`** wired in `cmd/procman/main.go`: `auto` =
  termhandler on a tty, plain `TextHandler` when piped; `term` = force
  termhandler (color when piped).
- **`--columns N`** sets `TermHandler.Options.Columns`; truncation is now
  actually applied (`Handle` previously computed the length but wrote the
  full buffer).
- **Queue depth is not a CLI flag.** The `--max-log-queue` flag and the
  `Formation.LogQueueSize` → `withLogQueue` plumbing were **removed**; the
  streams always use the `DefaultMaxQueue` (256) default, and
  `writelog.StreamConfig.MaxQueue` remains as a library knob for embedders
  (and deterministic tail-drop tests). See `docs/THROTTLING.md` §9.
- **Tests:** `writelog` (drain order, tail-drop under backpressure, default
  queue), `termhandler` (columns truncation, forced colors, `IsTerminal`).
  E2E: trebuchet under a pty with a slow reader delivers a bounded subset,
  production continues, no "blocked for" abort.
- **Notes:** `TestPerProcessLogLevelOverride` made deterministic (quiet
delays so web echoes first; two racing children were flaky).

### 5. Hot-path performance: writelog + termhandler — partial (2026-09-13)

Analysis + behavioral-optimization pass on the output hot path. Full record in
`docs/THROTTLING.md` §9. Current state:

- **Done:** single-copy line split (REPLACED `bytes.Buffer.ReadBytes`, which
  copied each line twice → 2 allocs/line, with a direct `bytes.IndexByte`
  scan → 1 alloc/line) and batched enqueue+drain (`push`/`pushAll`;
  `drain` pops up to 32 lines per lock hold, logs outside the lock).
  Results: −50% allocations and −25–36% time across the cheap-sink, full-
  term-path, and 16-stream-contention benchmarks; all tests + `-race` green.
- **Accepted invariant:** the single `TermHandler` mutex / single fd is a
  serialization (latency) point, **not** a blocking point for child processes
  — it is only reached through the bounded writelog queue, which tail-drops
  when the sink is wedged, so producers never block on the lock.
- [ ] **Deferred:** write-batching coalescer for the shared fd to cut the
      remaining cross-stream/`write(2)` serialization cost. Larger behavior
      change; not required for the no-process-blocking invariant.
- [ ] **Deferred (optional):** eliminate the last per-line allocation via a
      buffer pool with explicit lifetime tracking across the async ring
      (needed because a zero-copy view into the `os/exec` copy buffer is
      unsafe, and the one copy remains).

## Release readiness checklist

Code, tests, and docs:

- [ ] **Feature items 1–4 above done and tested** — these are the "most of
      these are done" gate from the README.
- [ ] Rewrite `README.md`: drop the "foreman instead for now" caveat and the
      implied unusability; document actual flags (`-f/--procfile`,
      `-w/--workdir`, `-e/--env`, `--formation`, `--output`, `--columns`,
      `--debug`); add a short usage example and note the
      embeddable-library use case next to the CLI use case. (Progress: README
      now has Usage + throttling sections; the caveat stays until formation/
      port/dotenv close.)
- [x] Fix `AGENTS.md` drift: it claimed `c.WaitDelay` is `10 * time.Second`
      but the code sets `1 * time.Second` (`pkg/process/process.go:188`); doc
      updated, and `writelog`/`termhandler` sections refreshed for the
      throttle work.
- [ ] Integration coverage for `tests/Procfile.clean` and
      `tests/Procfile.onefailed`: add a `make integration` (or extend
      `make test`) target that runs the built binary against both Procfiles
      and asserts exit behavior (clean exit cancels formation; failed exit
      propagates).
- [ ] Tooling pass: `go fmt -n` is clean and `go vet ./pkg/process` passes
      today — re-run across `./pkg ./cmd ./tools` and keep clean. Note:
      `go vet .` from the repo root reports "no Go files"; vet per package.
- [ ] CHANGELOG — none exists; add one documenting the current behavior so the
      release has notes to point at.
- [ ] Version + tag. No tags exist yet and `main` is 1 commit ahead of
      `origin/main`. Propose `v1.0.0` for the "all WIP items closed" state or
      `v0.1.0` if scope is cut — decide explicitly (see below).
- [ ] Confirm the `go.mod`/`go.sum` update in `d435510` actually bumped the
      deps intended (go-shellwords, errgroup, terminal, pflag, go-cmp) and
      drop/revert anything accidental.
- [ ] Sweep for other dead CLI surface before tagging: `Output` field
      (pending item 4), unused `Dotenv`/`Formation` (items 3/1) — after the
      feature work these should all be live.

### Scope decision (blocker)

The README's "don't use this yet" stance only lifts once all four WIP items
are closed. If a release is wanted sooner:

- **v0.1.0** — tests/docs + README rewrite only; features 1–4 stay on the WIP
  list but the README is honest that foreman remains the better tool for
  those use cases.
- **v1.0.0** — all four items closed, README rewritten, CHANGELOG added,
  tag pushed. This is the "release ready" state this doc is driving toward.
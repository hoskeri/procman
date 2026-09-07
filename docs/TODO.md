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
| Throttle/discard slow terminal output | Half done | `tools/trebuchet` *measures* blocking; no throttle logic; `--output` flag dead (`main.go:30`) |

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

### 4. Terminal throttle / discard on slow terminal

**Current state:** `tools/trebuchet` measures whether logging blocks
(`blocked` > `--max-block` aborts), but nothing throttles. `TermHandler`
truncates lines only if `Options.Columns > 0`, and nothing sets that option.
`--output auto|term` is parsed but ignored — the termhandler is always used.

- [ ] Define the throttle policy. Options to evaluate (pick in review):
      bounded in-memory queue in `writelog` that coalesces/decays lines when
      the sink is slow; or drop-oldest in `TermHandler.Handle` under
      backpressure. Keep the cancel semantics intact — a slow *output* must
      never block process reaping.
- [ ] Wire `--output` in `cmd/procman/main.go`: `auto` = termhandler when
      stdout is a tty, plain text handler otherwise; `term` = force color.
- [ ] Expose `--columns` (or similar) to set `TermHandler.Options.Columns`
      (`pkg/termhandler/termhandler.go:121`) for line truncation.
- [ ] Acceptance: `trebuchet` passes with `--output term`; a piped/slow sink
      run completes without unbounded buffering; document the tradeoff in the
      README.

## Release readiness checklist

Code, tests, and docs:

- [ ] **Feature items 1–4 above done and tested** — these are the "most of
      these are done" gate from the README.
- [ ] Rewrite `README.md`: drop the "foreman instead for now" caveat and the
      implied unusability; document actual flags (`-f/--procfile`,
      `-w/--workdir`, `-e/--env`, `--formation`, `--output`, `--debug`);
      add a short usage example and note the embeddable-library use case next
      to the CLI use case.
- [ ] Fix `AGENTS.md` drift: it claims `c.WaitDelay` is `10 * time.Second`
      (`AGENTS.md:146`) but the code sets `1 * time.Second`
      (`pkg/process/process.go:188`). Make the doc match reality.
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
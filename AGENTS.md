# Agent Onboarding & Developer Reference Guide

## 1. Project Overview & Purpose

`procman` is a lightweight, embeddable **Procfile runner** written in Go. -
**Goal:** Run Heroku-style `Procfile` process definitions (e.g., `web: go run
main.go`, `worker: python worker.py`). - **Primary Use Case:** Designed
specifically to be embeddable in other Go applications (acting as a library),
while also providing a lightweight CLI binary. - **Inspiration:** It is a
minimal, embeddable alternative to tools like [Foreman][foreman].

[foreman]: https://github.com/ddollar/foreman

## 2. Directory Structure

```
├── cmd/
│   └── procman/              # Command-line entry point
│       └── main.go           # CLI flags parsing, setup, and orchestration
├── pkg/
│   ├── procfile/             # Procfile parsing
│   │   ├── procfile.go       # Parses Procfile text into Tag/command records
│   │   └── procfile_test.go  # Unit tests for parsing
│   ├── process/              # Core process representation and execution
│   │   ├── process.go        # Spawns & monitors processes under errgroup.Group
│   │   └── process_test.go   # Unit tests for execution and formation wiring
│   ├── termhandler/          # Output facade: terminal rendering + child log channels
│   │   ├── termhandler.go    # renderer (colored prefixes) + TermHandler facade (ChildFDs)
│   │   └── termhandler_test.go
│   └── writelog/             # Stream adapters, framers, and relays to slog
│       ├── writelog.go       # Bounded queue; splits streams by newline for slog
│       ├── framer.go         # Binary SOCK_SEQPACKET frames + FramerHandler
│       ├── relay.go          # DualRelay (frame or text) + ChildSinks probe
│       └── *_test.go
├── tests/                    # Integration tests + sample Procfiles
│   ├── integration_test.go   # Execs the built procman/trebuchet (PROCMAN_BIN)
│   ├── Procfile.clean        # Sample Procfile with successfully exiting commands
│   └── Procfile.onefailed    # Sample Procfile where one process fails
├── tools/
│   └── trebuchet/            # High-rate logging & slow-terminal detection tool
│       └── trebuchet.go      # Measures if writing logs blocks execution
├── go.mod / go.sum           # Dependencies (e.g., go-shellwords, errgroup, termhandler)
├── Makefile                  # Build, test, and clean target rules
└── README.md                 # User-facing summary and roadmap
```

## 3. Core Architecture & Components

`procman` is built on four core packages inside `pkg/` that collaborate to
parse, execute, and stream output from processes:

### A. Concurrency & Execution (`pkg/process`)

- **`Process`** (defined in `pkg/process/process.go`): Struct representing a
  single command definition.    - Holds state such as resolved `Environ`
  strings, `CmdArgs` (parsed command & args), working directory (`Workdir`),
  and `LogLevel` overrides.
  - Can execute processes using standard `os/exec` under a specific context
    (`run`), or replace the existing process using `syscall.Exec` (`Exec`).

  - **`Formation`**: Represents the collection of processes from a `Procfile`.
    `LoadFile` (or `New`) resolves the working directory and delegates parsing
    to **`pkg/procfile`**; `Load` converts the parsed records into `Process`
    structs wired to the formation's `Workdir`.
    - Takes a `process.LogSink` (`Logger() *slog.Logger` + `ChildFDs(tag,
      index, resolver)`) as `Formation.Logs`. `Run` asks the sink for per-child
      stdout/stderr descriptors and wires them into each `exec.Cmd`; the sink
      (e.g. `*termhandler.TermHandler`) owns the transport, root/nested
      detection, and relays. When `Logs` is nil, output falls back to the
      `writelog` text pipeline into `slog.Default()`.
    - `Formation.LogLevels` (`process.LogLevels`) carries a default log level
      plus overrides. `Run` passes the sink a `writelog.LevelResolver` built
      from `Process.LogLevel` (which wins) and `LogLevels.ForIdentity`; the
      sink consults it per relayed record, so an override may name a process
      tag (`webhook`), a full component path (`webhook/validate`), or the
      component alone (`validate`). `process.ParseLogLevels(spec, def)` parses
      the `"info,api=debug"` CLI form and seeds `def` so an overrides-only spec
      keeps the caller's default. Overrides for tags that match none of those
      forms are ignored (resolution falls through to Default / the ambient
      sink level).
    - Orchestrates execution inside `Run(ctx)`. Processes are started in
      parallel using an **`golang.org/x/sync/errgroup.Group`**.
    - **Crucial Behavior:** Under the `errgroup`, if any single process exits
      (whether successfully or with an error), the entire group context is
      canceled, resulting in the termination of all other sibling processes.
      This matches Heroku/Foreman behavior.
    - **Exit semantics:** The first process to exit **on its own** (cleanly or
      crashing) is the one that brings the formation down; `Run` returns it as
      an `*ExitError` carrying the process's own exit code (`0` for a clean
      exit, `128+signum` when it died of a signal, `errors.As`-able in callers).
      A process killed because the formation was already torn down observes a
      canceled context and reports nothing, so when the teardown was user
      initiated (context canceled by a signal handler), `Run` returns `nil`.
      `pf` attributes the exit; launch failures are tagged with the process
      name and returned as plain errors.


### B. Procfile Parsing (`pkg/procfile`)

- **`Parse`** (defined in `pkg/procfile/procfile.go`): Parses Procfile text
  into a list of `Record`s (a `Tag` and shellwords-parsed `CmdArgs`).
    - Lines starting with `#` and blank lines are ignored; each remaining line
      is split at the first `:` into `Tag` and `Command` segments.
    - Uses `github.com/mattn/go-shellwords` to parse command-line strings into
      argument slices correctly respecting quotes.
    - Returns a neutral record type — the package does not depend on
      `pkg/process`. Malformed lines (no `:`) report the offending line number.

### C. Output Stream Redirection (`pkg/writelog`)

- **`stream`** (defined in `pkg/writelog/writelog.go`): Implements `io.Writer`.
    - Captures raw `stdout` and `stderr` from the running subprocesses.
    - Buffers bytes using `bytes.Buffer` and reads incoming blocks until it hits
      newline (`\n`) bytes.
    - Each complete line is enqueued onto a **bounded async queue** (default
      depth `DefaultMaxQueue` = 256) drained by a dedicated worker goroutine
      that feeds the line into a structured `slog.Logger` (`s.sink.LogAttrs(...)`)
      at a specified log level. When the queue is full the **oldest line is
      discarded** (tail policy), so a slow sink (terminal) never back-pressures
      the child process.
    - `Stream(sink, tag, lvl, StreamConfig)` returns an `io.WriteCloser`.
      `StreamConfig.MaxQueue` bounds the queue (`<= 0` = default).
    - **Tag path vs. attribute groups:** `writelog` separates a record's
      display **tag path** from its slog **attribute groups**. `TagHandler`
      (`WithTag`) extends the display path — rendered as the prefix and
      carried as `Frame.Tag` — while `WithGroup` always denotes an attribute
      namespace (carried as `Frame.Groups`, and used to qualify attrs at the
      root). `WithTag`/`TagCapable`/`TaggedSink` adapt ordinary `slog`
      handlers (a plain `TextHandler` falls back to a group, and `TaggedSink`
      additionally emits a `tag` attribute so the tag stays visible).
    - **Stream Lifecycle:** Always call `Close()` after the subprocess exits.
      `Close` stops the drain worker after it has emitted every queued line,
      then flushes any partial last line that lacked a trailing newline.
    - **In-process component tagging:** `TaggedLogger(tag string) *slog.Logger`
      (in `tagged.go`) exposes the nested framing multiplexer to code that
      does not run a formation. It probes fd 1/2 for a parent's log channel;
      when nested it sets the record's display tag (`WithTag`) so the parent
      renders the component as `<process>/<tag>` (e.g. `webhook/authn`) and
      preserves the record's attrs. It reuses an already-installed
      `FramerHandler` default (as procman's own `main` sets) before probing,
      and otherwise falls back to `slog.Default().WithGroup(tag)` so it is safe
      stand-alone. The parent resolves the level per component from the tag
      path, so overrides may name the process tag (`webhook`), the full path
      (`webhook/validate`), or the component alone (`validate`).


### D. Output Facade (`pkg/termhandler`)

- **`TermHandler`** (defined in `pkg/termhandler/termhandler.go`): the output
  facade for a formation. Built by `New(ctx, stdin, stdout, stderr, opts)`.
    - Owns **root vs. nested** detection: `seqpacketFd` probes stdout/stderr
      for a parent formation's `SOCK_SEQPACKET` log socket. Root mode renders
      locally; nested mode frames records through `writelog.NewFramer`.
    - Owns **terminal state**: on the first tty stream (stdin preferred) it
      calls `NoEcho` and restores termios on `Close`; the `ctx` triggers
      `Close` via `context.AfterFunc`.
    - Owns **child channels**: `ChildFDs(tag, index, resolver)` creates a
      per-child `SOCK_SEQPACKET` socketpair, starts a `DualRelay` into the
      matching sink, and returns child stdout/stderr `*os.File`s. The
      `writelog.LevelResolver` is attached to the sink handler
      (`WithResolver`), whose `Enabled` consults it with the record's
      component tag path (the tag path minus the leading process tag); a
      matched override wins over `WithOverride` and `Options.Level`. A positive
      `index` becomes a `tag-index` identity for replicas. `Close` drains
      relays, then force-closes after a grace period.
    - `Options.Plain` selects a plain `slog.TextHandler` renderer instead of
      the colored one (`--output auto` on a pipe); `Options.Colors` forces
      color (`--output term`).
- **`renderer`** (unexported): the historical `slog.Handler` that prefixes
  each line with a bold, tag-derived color label and renders attributes in
  logfmt after the message. Checks `terminal.IsTerminal` for auto color,
  truncates to `Options.Columns`, and hashes the innermost tag with FNV-1a for
  a consistent palette entry. Prefixes are padded to 16 columns (e.g.
  `             web | `). `WithTag` extends the display tag path and recomputes
  the prefix; `WithGroup` records an attr namespace only. `Handle` takes a fast
  path (raw message, no formatting) when the record has no attrs and no attr
  namespaces, so high-volume process output is unaffected; otherwise a
  stripped-down `slog.TextHandler` formats the attrs, with the synthetic
  `tag`/`stream` attrs filtered out.


## 4. Helper Tools & Diagnostics

### Trebuchet (`tools/trebuchet`)

- A helper CLI program designed to generate a heavy stream of random base64
  messages at high speeds.
  - **Purpose:** Used as a target in Procfiles to test and measure if logging
    blocks the main application thread for too long.
  - It asserts that message delivery does not exceed `max-block` duration, aiding
    in diagnosing output throttling bottlenecks.

## 5. Standard Tasks & Make Targets

You can manage the build lifecycle using standard shell commands:

- **Build all binaries:**

```bash
make build
```

Builds the `procman` CLI and the `trebuchet` test-fixture utility once each
into `_output/$(GOOS)_$(GOARCH)/bin/` (default `_output/linux_amd64/bin/`),
the single shared binary directory for production and tests.

- **Run all tests (unit + integration):**

```bash
make test
```

Depends on `build`, then runs `go test -count=1 -v ./...` with
`PROCMAN_BIN` pointing at the prebuilt binary. `tests/` is a test-only Go
package (`tests/integration_test.go`) that execs that binary against the
`tests/Procfile.*` fixtures (trebuchet stands in for the user application)
and asserts exit behavior end to end. A bare `go test ./...` skips those
integration tests (no `PROCMAN_BIN`), so unit-only runs still work without
building binaries.
  - **Clean build artifacts:**

```bash
make clean
```

Deletes `_output/` and any legacy root-level binaries (`./procman`).

## 6. Shell Command Rules

- **Always pass `--no-pager` to every `git` invocation.** The terminal
  environment used by agents is non-interactive and will hang waiting for
  pager input otherwise. ```bash # Correct git --no-pager diff git --no-pager
  log -n 10 # Never do this — it will block waiting for user input git diff
  git log ```

## 7. Development Tips & Gotchas for Agents

- **Procfile Parsing**: The parser lives in `pkg/procfile`; `TestParse` there
    covers quoted arguments, comment/blank-line skipping, and malformed lines.
    `TestFormation` in `pkg/process/process_test.go` covers conversion of parsed
    records into `Process` structs wired to the workdir. If you modify Procfile
    parsing, extend `pkg/procfile/procfile_test.go`.
  - **Context Cleanup**: Ensure that any manual signal handling or parent context
    propagation preserves the cancel propagation. When processes exit, their
    processes should be reaped cleanly by the OS. Shutdown is graceful:
    cancellation sends **SIGTERM to the whole process group** first, and a
    scheduled group-wide SIGKILL — plus `os/exec`'s own single-process kill —
    lands after `c.WaitDelay = 1 * time.Second` for processes that ignore
    SIGTERM. Do not replace `Cancel` with a direct SIGKILL; it defeats the soft
    shutdown and orphans grandchildren of TERM-ignoring children.
  - **Environment Setup**: The runner utilizes `baseEnv(...)` to forward a
    whitelist of critical environment variables (`PATH`, `HOME`, `USER`,
    `USERNAME`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `TMPDIR`, `HTTP\_PROXY`,
    etc.) from the host machine to child processes, plus any explicit
    `Process.Environ` entries that win on collision. Anything not in the list
    is deliberately not inherited; pass it via `Process.Environ` if a child
    needs it. When writing tests or running locally, verify these variables are
    present in the host terminal.
  - **Log API Compatibility**: The library uses Go's standard `log/slog` library
    introduced in Go 1.21. All custom handlers and logging interfaces must
    adhere strictly to `slog.Handler`.


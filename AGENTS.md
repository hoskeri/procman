# Agent Onboarding & Developer Reference Guide

## 1. Project Overview & Purpose

`procman` is a lightweight, embeddable **Procfile runner** written in Go.
- **Goal:** Run Heroku-style `Procfile` process definitions (e.g., `web: go run main.go`, `worker: python worker.py`).
- **Primary Use Case:** Designed specifically to be embeddable in other Go applications (acting as a library), while also providing a lightweight CLI binary.
- **Inspiration:** It is a minimal, embeddable alternative to tools like [Foreman](https://github.com/ddollar/foreman).

---

## 2. Directory Structure

```
├── cmd/
│   └── procman/              # Command-line entry point
│       └── main.go           # CLI flags parsing, setup, and orchestration
├── pkg/
│   ├── process/              # Core process representation, parsing, and execution
│   │   ├── process.go        # Spawns & monitors processes under errgroup.Group
│   │   └── process_test.go   # Unit tests for parsing and execution
│   ├── termhandler/          # slog.Handler for colorful process-specific prefixes
│   │   └── termhandler.go    # Prepends bold, colored tags to log lines
│   └── writelog/             # io.Writer adapter to capture and pipe streams to slog
│       ├── writelog.go       # Buffers bytes and splits streams by newline for slog
│       └── writelog_test.go  # Unit & benchmark tests for stream-to-log adapters
├── tests/                    # Sample Procfiles for integration & regression tests
│   ├── Procfile.clean        # Sample Procfile with successfully exiting commands
│   └── Procfile.onefailed    # Sample Procfile where one process fails
├── tools/
│   └── trebuchet/            # High-rate logging & slow-terminal detection tool
│       └── trebuchet.go      # Measures if writing logs blocks execution
├── go.mod / go.sum           # Dependencies (e.g., go-shellwords, errgroup, termhandler)
├── Makefile                  # Build, test, and clean target rules
└── README.md                 # User-facing summary and roadmap
```

---

## 3. Core Architecture & Components

`procman` is built on three core packages inside `pkg/` that collaborate to parse, execute, and stream output from processes:

### A. Concurrency & Execution (`pkg/process`)
- **`Process`** (defined in `pkg/process/process.go`): Struct representing a single command definition.
  - Holds state such as resolved `Environ` strings, `CmdArgs` (parsed command & args), working directory (`Workdir`), and `LogLevel` overrides.
  - Can execute processes using standard `os/exec` under a specific context (`run`), or replace the existing process using `syscall.Exec` (`Exec`).
- **`Formation`**: Represents the collection of processes parsed from a `Procfile`.
  - Parses Procfiles using `LoadFile` or `Load`. Lines starting with `#` are ignored, and each line is split by `:` into `Tag` and `Command` segments.
  - Uses `github.com/mattn/go-shellwords` to parse command-line strings into argument slices correctly respecting quotes.
  - Orchestrates execution inside `Run(ctx)`. Processes are started in parallel using an **`golang.org/x/sync/errgroup.Group`**.
  - **Crucial Behavior:** Under the `errgroup`, if any single process exits (whether successfully or with an error), the entire group context is canceled, resulting in the termination of all other sibling processes. This matches Heroku/Foreman behavior.

### B. Output Stream Redirection (`pkg/writelog`)
- **`stream`** (defined in `pkg/writelog/writelog.go`): Implements `io.Writer`.
  - Captures raw `stdout` and `stderr` from the running subprocesses.
  - Buffers bytes using `bytes.Buffer` and reads incoming blocks until it hits newline (`\n`) bytes.
  - Each extracted line is fed directly into a structured `slog.Logger` (`s.sink.LogAttrs(...)`) at a specified log level.
  - **Tagging:** Every logged line includes the process tag as a group and attribute (`tag="web"`), guaranteeing proper tracing.

### C. Aesthetic Terminal Logging (`pkg/termhandler`)
- **`TermHandler`** (defined in `pkg/termhandler/termhandler.go`): Implements `slog.Handler` on top of a standard file/writer output.
  - Automatically checks if the writer is a terminal using `terminal.IsTerminal` and toggles ANSI color escape codes accordingly.
  - Hashes the process `Tag` using FNV-1a to dynamically assign a consistent, random, high-contrast ANSI foreground color from a pre-defined palette.
  - Prefixes every printed log message with a bold, colorful label (e.g., `             web | `) padded to a fixed width of 16 characters to align the logs nicely.

---

## 4. Helper Tools & Diagnostics

### Trebuchet (`tools/trebuchet`)
- A helper CLI program designed to generate a heavy stream of random base64 messages at high speeds.
- **Purpose:** Used as a target in Procfiles to test and measure if logging blocks the main application thread for too long.
- It asserts that message delivery does not exceed `max-block` duration, aiding in diagnosing output throttling bottlenecks.

---

## 5. Standard Tasks & Make Targets

You can manage the build lifecycle using standard shell commands:

- **Build all binaries:**
  ```bash
  make build
  ```
  Creates the `procman` CLI binary at the root directory and the `trebuchet` benchmark utility at `./tools/trebuchet/trebuchet`.

- **Run the unit tests:**
  ```bash
  make test
  ```
  Runs all Go unit and benchmark tests.

- **Clean build artifacts:**
  ```bash
  make clean
  ```
  Deletes compiled binaries (`procman` and `trebuchet`).

---

## 6. Shell Command Rules

- **Always pass `--no-pager` to every `git` invocation.** The terminal environment used by agents is non-interactive and will hang waiting for pager input otherwise.
  ```bash
  # Correct
  git --no-pager diff
  git --no-pager log -n 10

  # Never do this — it will block waiting for user input
  git diff
  git log
  ```

---

## 7. Development Tips & Gotchas for Agents

1. **Test Formation**: `TestFormation` in `pkg/process/process_test.go` has two subtests covering quoted arguments and comment/blank-line skipping. If you modify Procfile parsing, extend these cases.
2. **Context Cleanup**: Ensure that any manual signal handling or parent context propagation preserves the cancel propagation. When processes exit, their processes should be reaped cleanly by the OS. The `c.WaitDelay` is set to `10 * time.Second` to allow soft shutdown before hard termination.
3. **Environment Setup**: The runner utilizes `baseEnv(...)` to forward critical environment variables (`PATH`, `HOME`, `TERM`, `HTTP_PROXY`, etc.) from the host machine to child processes. When writing tests or running locally, verify these variables are present in the host terminal.
4. **Log API Compatibility**: The library uses Go's standard `log/slog` library introduced in Go 1.21. All custom handlers and logging interfaces must adhere strictly to `slog.Handler`.
5. **Stream Lifecycle**: `writelog.Stream` returns an `io.WriteCloser`. Always call `Close()` after the subprocess exits to flush any partial last line that lacked a trailing newline.

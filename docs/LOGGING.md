# Nested Formation Logging (dedicated log socket)

**Status:** In progress — procman-side core implemented; runkube integration pending.

## Problem

When one procman `Formation` runs a child process that itself embeds a
procman `Formation` (nested procman), every process boundary flattens slog
structure to rendered text. The child's tags, levels, and attributes are lost
as structured data; only the rendered text survives inside the parent's
message payload.

In runkube, this manifests as:

- Node formation's `termhandler` renders `kubelet | <msg>` into its stdout
  pipe. The top formation logs it as `{group: "node-1", msg: "kubelet | <msg>"}`
  — the kubelet tag is a string inside the message; level, attrs, and group
  identity are gone. Double rendering (child + parent) also wastes work.
- `RunCRIServer` logs via the default `slog` (TextHandler → stderr), bypassing
  the formation sink entirely → arrives without any tag.

## Design invariant

**Serialize structure at every process boundary; re-materialize slog records
at the immediate parent; render human text exactly once, at the root.**

## Wire format

Each structured log record is encoded as a JSON object sent over a
**SOCK_SEQPACKET** unix socketpair (one message = one record).

### Frame schema (v1)

```json
{"v":1,"lvl":4,"tag":"kubelet","msg":"starting ...","attrs":{"key":"val"},"groups":["kubelet"]}
```

| Field | Type | Description |
|---|---|---|
| `v` | int | Frame version (1) |
| `lvl` | int | slog.Level as int (debug=-4, info=0, warn=4, error=8) |
| `tag` | string | Innermost process tag (e.g. `"kubelet"`) |
| `msg` | string | Record message text |
| `attrs` | object | Key-value attributes (optional); structured attrs are JSON objects |
| `groups` | array | Current group path from the child handler (e.g. `["kubelet"]`); if the child has deeper nested groups they accumulate here |

- `attrs` and `groups` are omitted when empty.
- The JSON is the complete message payload; no framing marker is needed
  because the socketpair message boundary is the record boundary.
- Max frame size: 16 KiB. Records exceeding this are truncated by trimming
  the message text (attrs are preserved); if still too large, the record is
  dropped silently.

### Why JSON

- Self-describing and debuggable (`` nc -U log-socket `cat` `` reveals frames).
- stdlib `encoding/json` is already on the hot path in `writelog` (line split)
  — no new dependency.
- Future extensibility: add fields at will, gated behind `v`.

### Why SOCK_SEQPACKET

- Message boundaries are the frame boundaries — no newline scanning, no
  PIPE_BUF atomicity worries, no interleaving from concurrent writers. Unlike
  plain DGRAM, the recv socket returns EOF when the last write-end reference
  is closed, so a relay goroutine can detect a child exiting without an
  explicit shutdown protocol.
- SEQPACKET is Linux-only for AF_UNIX; this is acceptable because nested
  procman logging targets the Linux sandbox path. On platforms without it
  the root (non-nested) path is unaffected.
- `O_NONBLOCK` + `EAGAIN`-on-full gives a clean, non-blocking overflow policy
  without a separate bounded queue (the socket buffer provides the queue).

## fd hierarchy

### Convention

Every procman-managed process (child of a Formation) has its log-up socket at
a well-known fd number conveyed by the environment variable `PROCMAN_LOG_FD`.
The default is `3`.

- **Root process** (no `PROCMAN_LOG_FD` in its env): its `Formation.Sink` is
  the user-configured handler (e.g. `termhandler` on `os.Stdout`). It writes
  no frames — all output is rendered directly.
- **Nested process** (`PROCMAN_LOG_FD` set): its `Formation.Sink` is replaced
  by a `writelog.FramerHandler` that encodes each record as a frame and
  writes it to the fd. The fd is a SOCK_SEQPACKET send-side socket whose recv
  side lives in the parent.

### Per-child sockets

Each `Formation` creates **one SOCK_SEQPACKET socketpair per child process**.
The spawn end (send-side) is passed to the child at `PROCMAN_LOG_FD` (via
`exec.Cmd.ExtraFiles` or `os.StartProcess.Files`). The recv end stays in the
parent and is read by a dedicated relay goroutine that knows the child's tag.

This per-child design means:

- The relay knows each child's tag from the process loop — no need for the
  child to stamp its own `PROCMAN_TAG` or for the frame to carry parent
  ancestry; hierarchy is implicit in the socket ownership.
- When the child exits, its send-side end-of-file is naturally detected by
  the relay (SEQPACKET recv sees EOF when all write-end refs are closed).

### Sandbox (runkube) integration

The sandbox replaces the process environment (see `sandbox.go:671-673`). The
log socket fd must survive into the sandbox child and through the exec to the
final command. Since `os.StartProcess.Files` dup2's the parent's fds to the
child's fds in array order, the sandbox must:

1. Include the log socket `*os.File` in the `Files` list at the appropriate
   index (after stdin/stdout/stderr, so the child gets it at `PROCMAN_LOG_FD`).
2. Add `PROCMAN_LOG_FD=N` to the sandbox child's `Env` list (currently at
   `sandbox.go:671-673`).

The fdchan control socket already occupies fd 3 in the sandbox child (via
`Files[3] = os.NewFile(control.ChildFD())`). The log socket must use a
*different* fd number — 4 is clean — and the sandbox must set the env
`PROCMAN_LOG_FD=4` accordingly. Since `fdchan.EnvPair(targetIndex)` accepts
an explicit target fd index, the collision is resolved by advancing the
index.

## Sink model

### Root formation

```
Formation.Sink = whatever the embedder set (e.g. termhandler)
   └─ relay goroutines per child: parse frame → parent.Sink.WithGroup(childTag).LogAttrs(...)
```

All children output frames; the root's relay unwraps them, groups under the
child's tag, and passes to the user's sink for human rendering.

### Nested formation

```
Formation.Sink = FramerHandler(fd_up)   ← detected via PROCMAN_LOG_FD
   └─ relay goroutines per child: parse frame → parent.FramerHandler
```

The framer's `Handle` encodes each record (including re-emitted relay frames)
as a JSON frame and sends it up the socket to the formation's parent.

### Recursion

Each level does the same thing — creates per-child sockets, spawns relays,
and (if itself nested) flattens its sink to a framer to its own parent. The
chain's length is unbounded; group depth grows linearly with depth.

## Overflow policy (no-backpressure invariant)

Per `docs/THROTTLING.md`, child processes must **never** be stalled by a slow
sink. For the dedicated log socket:

- **Send side** (child framer → parent): `O_NONBLOCK`. When the socket's
  send buffer is full (EAGAIN), the frame is silently dropped via a
  per-process drop counter on `FramerHandler`. This mirrors the writelog's
  tail-drop queue policy for text lines.
- **Recv side** (parent relay): blocking read on a `*net.UnixConn`.
  Interruption uses `conn.SetReadDeadline(past)` (a `close(2)` alone cannot
  unblock a concurrent blocking `read` on Linux), which wakes the reader and
  the goroutine exits.
- **Socket buffer size**: default AF_UNIX SOCK_SEQPACKET `SO_RCVBUF`
  (~212 KB) provides ample burst headroom. An explicit `SO_RCVBUF` or
  `SO_SNDBUF` may be set if benchmarks show unnecessary drops; skip for now.

## Relay goroutine

One per child. Signature:

```go
func Relay(recv *net.UnixConn, parentSink *slog.Logger, childTag string, wg *sync.WaitGroup)
```

- Reads messages from `recv` in a loop with a `MaxFrameSize+1024` buffer.
- Decodes each frame via `json.Unmarshal`.
- Re-emits into `parent.Sink` via `sink.WithGroup(childTag).LogAttrs(...)`,
  then for each element of `frame.Groups` appends one more `WithGroup`.
- Tags the record with a `tag` attr drawn from `frame.Tag`.
- Exits on read error/EOF (last writer closes the socket) or when the
  parent sets a past read deadline on teardown.

### Teardown

When `Formation.Run` returns (completes or context is canceled), it sets a
past read deadline on every child's recv `*net.UnixConn` (unblocking the
reader), closes the connectors, and waits on the relay `WaitGroup`. The relay
goroutines exit promptly and no goroutines leak.

## Root rendering: group path

The root terminal handler (`TermHandler`) must render the full group path,
not just the innermost group. Currently `WithGroup` overwrites `name`. This
must change to a group path join.

- Add `groupPath []string` field to `TermHandler`.
- `WithGroup(name)` appends to `groupPath`; returns a shallow copy.
- The line prefix is computed by joining `groupPath` elements with separator
  ` | `, each element right-padded to 16 characters:

  ```
  node-1          | kubelet          | <msg>
  ```

  If the combined prefix exceeds the configured `Columns`, the message is
  truncated as before (but the prefix is always shown in full — bias towards
  identity over message).
- Color is derived from the innermost group (last in `groupPath`), keeping
  the existing FNV hash palette.
- The `tag` attr is set to the innermost group (`<last group>`), mirroring
  today's behavior.

## Per-package change list

### `pkg/writelog` (new files)

1. **`framer.go`** — `Frame` struct + marshal/unmarshal + `FramerHandler`.
   - `WriteFrame(sendFd int, f Frame) (bool, error)` — raw `syscall.Write`
     on the non-blocking SEQPACKET send fd (avoids Go runtime poll), drop on
     EAGAIN + counter.
   - `ReadFrame(in *os.File) (Frame, bool, error)` — read one message; return
     `ok=false` on EOF.
   - `FramerHandler` implementing `slog.Handler`:
     - Fields: out, groupPath, attrs, dropCount, level (from Leveler).
     - `Handle`: encodes record as Frame (calling `rec.Attrs` to iterate
       attrs; collects group path from handler state + rec groups? Actually
       slog.Handler.Handle receives the record; the handler's own group path
       is in the handler state. Groups added via slog.Logger.WithGroup are
       on the handler, not in the record. The framer's own groupPath field
       holds the groups added via `WithGroup`. So Frame.groups = handler's
       groupPath. The tag attr: read from attrs if present else innermost
       group.
     - `WithGroup`: appends to groupPath copy.
     - `WithAttrs`: appends to attrs copy.
     - `Enabled`: level check (same as termhandler).

2. **`relay.go`** — `Relay(recv *net.UnixConn, sink *slog.Logger, childTag string, wg *sync.WaitGroup)`.
   - Blocking read loop; decode; emit into sink with group nesting.
   - Register with wg before starting loop; decrement on exit.
   - On teardown, parent sets a past read deadline → reader wakes → exit.

### `pkg/process` (modified)

3. **`Formation.Run`**:
   - Add nested-mode detection: `os.Getenv("PROCMAN_LOG_FD")`.
   - If nested, replace `l.Sink` with `writelog.NewFramerHandler(fd_from_env)`.
   - For each child:
     - `syscall.Socketpair(AF_UNIX, SOCK_SEQPACKET, 0)` → recvFd/sendFd.
     - Set `O_NONBLOCK` on sendFd; wrap recvFd as a `*net.UnixConn` via
       `writelog.SetupRecvConn`.
     - Pass sendFd to child as ExtraFiles[0] (child gets fd 3) and set env
       `PROCMAN_LOG_FD=3` for the child (merge into child's env).
     - Spawn relay goroutine: `writelog.Relay(recvConn, l.Sink, p.Tag, &relayWg)`.
   - After Run returns, set a past read deadline on every recv connector,
     close the connectors, and wait on relayWg.

### `pkg/termhandler` (modified)

4. **`TermHandler.WithGroup`** — append to groupPath instead of overwriting.
5. **`TermHandler.Handle`** — compute prefix from entire groupPath.
6. **`groupHandler`** — compute color from innermost group, prefix from
   full path.

### `cmd/procman` (maybe modified)

7. No changes expected — the CLI always runs root-level (no `PROCMAN_LOG_FD`),
   so its sink is the user's handler and the relay/framer layers are invisible.

## runkube integration points (not in this repo)

1. **`sandbox.go`** — add log socket to `Files` list at index `PROCMAN_LOG_FD`
   (e.g. 4) and `PROCMAN_LOG_FD=N` to the child's `Env`. Also update the
   fdchan `EnvPair` target index (shift by 1 if needed).
2. **`Node.RunCRIServer`** — route direct slog calls through the node
   formation's sink instead of the default logger. This is a small standalone
   fix regardless of log channel choice: pass a logger into `RunCRIServer`.

## Degradation behavior

- **Non-procman child** (e.g. kubelet binary, etcd): ignores the log socket;
  its stdout/stderr text still goes through writelog streams → parent's
  framer (if nested) or termhandler (if root), logged as text under the
  child's tag. No structure is lost that didn't already exist.
- **Parent crashes before relay reads**: the child's send buffer holds some
  frames; when the recv socket is closed (parent dies), the child's next
  `sendto` returns EPIPE (connection reset) → framer stops (set a "dead"
  flag, silently drop further records). Go returns EPIPE as an error on
  non-SIGPIPE fds (fds other than 1,2) — no process death.
- **No `PROCMAN_LOG_FD` set** (root mode): no framer; text-only output
  through the user sink (today's behavior). The relay still runs for children;
  it feeds the user sink with structured records when children emit frames.
  (If a root has no children at all, there are no relays and no change.)

## Tests

### Frame round-trip (unit)
- Marshal a frame → unmarshal the JSON → verify fields preserved.
- Round-trip with attrs, nested groups, empty msg, all levels.
- Frame truncation: force a message too long → verify truncated but still
  valid JSON.

### FramerHandler → Relay (isolated)
- In one process: socketpair → write side: FramerHandler → send frames (tag
  "alice", various levels/attrs) → read side: relay reads into a
  `slog.Handler` that records calls → verify groups, level, attrs, message
  survived round-trip.
- Test O_NONBLOCK drop: flood with small frames until EAGAIN → verify at
  least one drop occurred (counter > 0).

### Relay teardown
- Launch relay goroutine; set a past read deadline on the recv connector →
  verify goroutine exits promptly (no hang).

### Termhandler group path
- Construct a TermHandler, apply successive WithGroup("a").WithGroup("b"),
  handle a record → output line is `"               a |               b | msg\n"`
  (16-char right-padded each).

### Formation integration (future)
- Test binary that starts a Formation in nested mode (PROCMAN_LOG_FD set),
  spawns a child that logs via slog, and exits. Verify relay delivers the
  record.
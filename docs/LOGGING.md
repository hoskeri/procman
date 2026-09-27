# Nested Formation Logging (stdout-based autodetection)

**Status:** Implemented — procman side complete; runkube integration pending.

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

## Output facade (`pkg/termhandler`)

`termhandler.TermHandler` is the output facade for a formation.  It is built
with the process's standard streams and a lifetime context:

```go
logs := termhandler.New(ctx, os.Stdin, os.Stdout, os.Stderr, &termhandler.Options{...})
defer logs.Close()
```

It owns three concerns that used to be split between `cmd/procman/main.go` and
`Formation.Run`:

1. **Root vs. nested mode.** `New` probes stdout (and stderr) for a
   `SOCK_SEQPACKET` socket.  In root mode it renders records through the
   colored, tag-prefixed renderer (or a plain `slog.TextHandler` when
   `Options.Plain` is set, used by `--output auto` on a pipe).  In nested mode
   it stamps records as binary frames and sends them up the parent's socket.
2. **Terminal state.** In root mode it calls `NoEcho` on the first terminal
   stream (stdin preferred) and restores the previous termios on `Close`; the
   context passed to `New` triggers `Close` automatically via
   `context.AfterFunc`.
3. **Child channels.** `ChildFDs(tag, index, level)` creates a per-child
   `SOCK_SEQPACKET` socketpair, starts a `DualRelay` for the receive side, and
   returns the child-side stdout/stderr `*os.File`s for `exec.Cmd`.  A positive
   `index` is folded into the identity (`web-2`) so replicas get distinct
   prefixes/colors.  `Close` lets relays drain, then force-closes their receive
   sockets after a grace period.

`process.Formation` no longer knows anything about sockets or framing.  It
takes a `process.LogSink` (`Logger() *slog.Logger` + `ChildFDs(...)`), which
`*termhandler.TermHandler` implements, and wires the returned files into each
child.  When `Logs` is nil it falls back to the `writelog` text pipeline into
`slog.Default()`.

### Log levels (`Formation.LogLevels`)

The per-process level policy lives in `pkg/process`:

```go
type LogLevels struct {
    Default slog.Level
    Tags    map[string]slog.Level
}
```

`Formation.Run` resolves each process's level as explicit `Process.LogLevel` >
`Tags[tag]` > `Default`, then hands it to `ChildFDs` (or applies it to the
fallback logger).  `ParseLogLevels(spec, def)` parses the CLI form
`"info,api=debug"`: a bare entry sets `Default`, `tag=level` sets an override,
and `def` seeds `Default` so an overrides-only spec keeps the caller's default
(e.g. a flag default of `error`).

## Approach: repurpose stdout/stderr as the log channels

Instead of a dedicated file descriptor (`PROCMAN_LOG_FD`) and accompanying
environment variable, the nested formation uses **stdout (fd 1) and stderr
(fd 2)** as the structured log channels. This eliminates all extra fd
plumbing and env-var coordination.  Each frame carries a stdout/stderr
discriminator, and the relay attaches it as a `stream` attribute so future
rendering can treat the streams differently without a wire change.

### How it works

**Parent formation:** For each child process and each stream,
`TermHandler.ChildFDs` creates a `SOCK_SEQPACKET` socketpair. The send side
becomes the child's stdout (or stderr); the recv side stays in the parent and
is read by a `DualRelay` dispatcher started by the termhandler.

**Child formation (procman):** `termhandler.New` probes its own stdout and
stderr (fd 1, fd 2) via `getsockopt(SO_TYPE)`. Each fd that is a
`SOCK_SEQPACKET` socket is a nested log channel. It:
1. Sets `O_NONBLOCK` on that fd (safe because the framer is its sole writer)
2. Replaces the matching sink with a `FramerHandler` stamped with
   `StreamStdout`/`StreamStderr` that writes binary frames to that fd

**Non-procman child** (e.g. kubelet binary, trebuchet): Writes plain text
to stdout as usual. The parent's `DualRelay` detects the text (fast byte
prefix gate: first byte != frame version) and falls back to newline-based
line splitting.

**`DualRelay`** reads each message from the recv socket and checks the
first byte:
- `byte == 1` (frame version) → parse as binary frame, re-emit with
  structured groups and attrs into the parent sink
- otherwise → split on `\n`, log each line as text under the child's tag

The gate is a **single byte comparison** — zero overhead for text streams.

## Wire format

Each structured log record is encoded as a **binary frame** sent in one
`SOCK_SEQPACKET` message.

### Binary frame layout

```
Header (fixed 8 bytes):
  [0]     ver      uint8 (1)
  [1]     flags    uint8 (bit0=has_tag, bit1=has_groups, bit2=has_attrs)
  [2:6]   level    int32 little-endian
  [6:8]   msglen   uint16 little-endian (message text length)
  [8:]    message  msglen bytes

Optional sections (present when the corresponding flag is set):
  tag:     [len:uint8][data]
  groups:  [count:uint8]{[len:uint8][data]}...
  attrs:   remaining bytes of the message = raw JSON object
```

| Field | Type | Description |
|---|---|---|
| `ver` | uint8 | Frame version (1) |
| `flags` | uint8 | Bitmap: bit0=has_tag, bit1=has_groups, bit2=has_attrs, bits3-4=stream |
| `level` | int32 LE | slog.Level as int (debug=-4, info=0, warn=4, error=8) |
| `msglen` | uint16 LE | Length of message text (max 65535) |
| `message` | bytes | Record message text |
| `stream` | 2 bits | 0=unset, 1=stdout, 2=stderr (packed into flags[3:5]) |
| `tag` | string | Process tag, extracted from the `"tag"` slog attr |
| `groups` | []string | Handler group path from the child's `WithGroup` |
| `attrs` | JSON | Remaining attributes as JSON object, usually empty |

- `attrs` is omitted when empty (the common case — only `"tag"` is
  typically stamped, and it's promoted to the Tag field).
- Max frame size: 16 KiB. Records exceeding this are truncated by trimming
  the message text; if still too large they are silently dropped.

### Why binary vs. JSON for the frame

- **~65 ns / 2 allocs per encode** vs. ~1 µs+ with JSON marshal + map
- **~75 ns / 5 allocs per decode** vs. ~1 µs+ with JSON unmarshal
- No parser state, no backtracking, no string interning
- Self-delimiting: fields are fixed-size or length-prefixed
- The attrs field (cold path) still uses JSON for simplicity

### Why SOCK_SEQPACKET

- Message boundaries are the frame boundaries — no newline scanning, no
  PIPE_BUF atomicity worries, no interleaving from concurrent writers.
  Unlike plain DGRAM, the recv socket returns EOF when the last write-end
  reference is closed, so a relay goroutine can detect a child exiting
  without an explicit shutdown protocol.
- SEQPACKET is Linux-only for AF_UNIX; this is acceptable because nested
  procman logging targets the Linux sandbox path. On platforms without it
  the root (non-nested) path is unaffected.
- `O_NONBLOCK` + `EAGAIN`-on-full gives a clean, non-blocking overflow
  policy (the socket buffer provides the queue).

## Autodetection

### Child (procman) side

`termhandler.New` in `pkg/termhandler/termhandler.go` probes each stream via
`seqpacketFd` and, when stdout is a socket, builds framers:

```go
if fd, ok := seqpacketFd(stdout); ok {
    h.nested = true
    h.outHandler = writelog.NewFramer(writelog.SetupSendSocket(fd), writelog.StreamStdout, level)
    if efd, ok := seqpacketFd(stderr); ok {
        h.errHandler = writelog.NewFramer(writelog.SetupSendSocket(efd), writelog.StreamStderr, level)
    } else {
        h.errHandler = writelog.NewFramer(writelog.SetupSendSocket(fd), writelog.StreamStderr, level)
    }
    return h
}
```

`writelog.ChildSinks` remains as a low-level primitive for embedders that
implement their own `process.LogSink`, but the CLI and termhandler do not use
it.

### Parent side (DualRelay)

Each child gets one socketpair. The recv side feeds a `DualRelay`
goroutine that reads messages, gates on the first byte, and dispatches:

```
message received
    │
    ├─ byte 0 == 1 → binary frame → parse → emitRelayedFrame(...)
    │
    └─ byte 0 != 1 → text → split "\n" → emitTextLine(...)
```

Text fallback uses a `textBuf` that tracks partial (unterminated) lines
across successive SEQPACKET messages, flushes on EOF.

## fd hierarchy

No dedicated log fd. The protocol uses fd 1 (stdout) and fd 2 (stderr).

- **Root process** (stdio is a terminal or pipe): `termhandler.New` finds no
  socket → renders records directly to the terminal and `ChildFDs` creates
  child channels whose relays feed that renderer.
- **Nested process** (fd 1/2 are SEQPACKET sockets): `termhandler.New` builds
  framers → every record is binary-encoded and sent up the matching fd with
  its stream discriminator.
- **Non-procman child** (writes text to stdout/stderr): text is split on
  newlines by the parent's `DualRelay` and logged under the child's tag with
  a `stream` attr. No structure is lost that didn't already exist.

Spawned processes are detached from the controlling terminal: stdin is the
null device and `SysProcAttr.Setsid` puts them in a new session/process
group, so terminal-generated signals reach only procman.

## Per-child sockets

Each `TermHandler.ChildFDs` call creates **one SOCK_SEQPACKET socketpair per
stream** (stdout and stderr). The send sides become the child's fd 1 and
fd 2 (via `exec.Cmd.Stdout`/`Stderr`). The recv sides stay in the parent and
are read by `DualRelay` goroutines that know the child's tag and stream.

This per-child design means:

- The relay knows each child's tag from the process loop — no need for the
  child to stamp its own tag or carry parent ancestry.
- When the child exits, its send-side end-of-file is naturally detected
  by the relay (SEQPACKET recv sees EOF when all write-end refs are closed).
- Non-procman children write text through the same sockets; the relay
  transparently handles both formats via the byte gate.

## Tag naming convention

Process tags (the labels identifying processes in procman output) must conform
to a strict naming convention enforced at load time:

- **Lowercase letters** (`a`–`z`)
- **Digits** (`0`–`9`)
- **Dashes** (`-`) — interior only, no leading or trailing dashes

Examples: `web`, `kubelet`, `node-1`, `cri-server`.

This restriction allows the root `TermHandler` to render the group path as a
single combined path right-aligned in a 16-character column, followed by ` | `:

```
  node-1/kubelet | register-node
```

Single group:

```
             web | web-message
```

Nested path with "..." middle truncation when a combined path exceeds 16 chars:

```
node-1/...server | started
```

The `procman` meta-tag (used for formation lifecycle messages like "starting"
and "exited") also occupies one column slot:

```
         procman | starting web
```

Color is derived from the innermost group (the last element in the path) using
the existing FNV hash palette; the entire prefix is rendered in bold.

Tags that contain uppercase letters, underscores, dots, spaces, or any other
characters are rejected by `Formation.Load` with a clear error message.  The
same validation is also applied in `Process.run` as a defense-in-depth
measure for direct library callers.

## Overflow policy (no-backpressure invariant)

Per `docs/THROTTLING.md`, child processes must **never** be stalled by a
slow sink. For the log socket:

- **Send side** (child framer → parent): `O_NONBLOCK` (set by the child
  itself after detecting the socket type). When the socket buffer is full
  (EAGAIN), the frame is silently dropped via a per-process drop counter.
  This mirrors the writelog's tail-drop policy for text lines.
- **Non-procman children** see a blocking stdout (socket is created
  blocking).  They experience standard pipe backpressure — if the parent
  stops reading, writes block.  This is the expected behavior for regular
  processes.
- **Recv side** (parent relay): blocking read on a `*net.UnixConn`.
  Interruption uses `conn.SetReadDeadline(past)`, which wakes the reader
  and the goroutine exits.
- **Socket buffer size**: default AF_UNIX SOCK_SEQPACKET `SO_RCVBUF`
  (~212 KB) provides ample burst headroom for framed records.

## Relay goroutine

`DualRelay` in `pkg/writelog/relay.go`. One per child socket, launched from
`TermHandler.ChildFDs`.

```go
func DualRelay(recv *net.UnixConn, parentSink *slog.Logger,
               childTag string, channelStream StreamKind, wg *sync.WaitGroup)
```

- Reads messages from `recv` in a loop with a `MaxFrameSize+1024` buffer.
- Fast gate: `IsFramePrefix(data)` checks byte 0.
- Frame path: `UnmarshalBinary` → `emitRelayedFrame` (frame stream wins;
  channel stream is the fallback).
- Text path: `textBuf.feed` → each line logged with the channel's stream.
- Both paths attach the stream as a `"stream"` attr on the parent record.
- On read error / EOF: flushes any remaining partial text line, exits.
- Teardown: parent sets a past read deadline → reader wakes → exits.

## Degradation behavior

- **Non-procman child** (kubelet, etcd): writes text to stdout/stderr →
  relay splits text on newlines → logged as text under the child's tag with
  its stream. No structured attrs beyond `tag`/`stream`, but no loss vs. a
  non-nested procman.
- **Parent crashes before relay reads**: child's send buffer holds some
  frames; when the recv socket is closed, the child's next `sendto`
  returns EPIPE → framer silently drops further records.
- **No SEQPACKET stdio** (root mode): `termhandler.New` renders locally. The
  relay still runs for children; it feeds the renderer with structured
  records when grandchildren emit frames, or with text for non-procman
  children.

## Known issues

### Non-procman text over SEQPACKET can be silently truncated

The parent reads each socket with a fixed `MaxFrameSize+1024` (17408-byte)
buffer (`dualRelayLoop` in `pkg/writelog/relay.go`). SEQPACKET is
message-oriented: a `read` larger than the buffer is truncated and the
surplus is **silently discarded**, and a single `write` larger than the
socket send buffer fails with `EMSGSIZE`. A non-procman child that writes a
stdout/stderr chunk larger than ~17 KB in one `write(2)` therefore loses
data; a pipe does not have this behaviour.

This affects text children only. Procman children use the framer, which caps
each frame at `MaxFrameSize` and drops on `EAGAIN` rather than truncating.

Related consequences / open work:

- The text path also lost `writelog.Stream`'s bounded, tail-dropping async
  queue: `dualRelayLoop` logs synchronously, so a slow sink can back-pressure
  a blocking (non-procman) child.
- Real fixes are a length-prefixed text envelope, or giving text children a
  plain pipe and reserving the SEQPACKET channel for framed procman output.

Until then, treat multi-KB single-write stdout bursts from non-procman
children as potentially lossy.

## runkube integration points (not in this repo)

1. **`sandbox.go`** — no change needed for the log channel: the sandbox
   already forwards stdout/stderr into the container; the procman child
   inside the sandbox auto-detects them.  No `PROCMAN_LOG_FD`, no
   `ExtraFiles` plumbing for logging.
2. **`Node.RunCRIServer`** — route direct slog calls through the node
   formation's sink instead of the default logger.  This is a small
   standalone fix regardless of log channel choice.

## Tests

### Frame round-trip (unit)
- Binary marshal → unmarshal → verify fields preserved.
- Round-trip with attrs as JSON, empty attrs, nested groups, empty message.
- `IsFramePrefix` gate: true for binary frames, false for text.

### FramerHandler → DualRelay (isolated)
- Socketpair → write frames from `FramerHandler` → `DualRelay` reads and
  re-emits into a capture handler → verify levels, msg, tag, attrs.
- Text fallback: write text lines → verify split and logged under child tag.
- Mixed: interleaved frames and text from the same socket → both handled.

### O_NONBLOCK drop
- Flood with large frames until EAGAIN → verify drop counter > 0.

### Relay teardown
- Set past deadline → verify goroutine exits promptly.

### Text buffer
- `textBuf.feed` across multiple calls with partial final line → verified
  correct splitting and `flush`.

### Formation integration (end-to-end)
- `TestProcfileClean`, `TestProcfileOneFailed`, `TestHighVolumeThroughput`
  — all pass with the stdout-based socketpair design (no env var, no
  ExtraFiles).
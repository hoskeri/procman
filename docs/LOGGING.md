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

## Approach: repurpose stdout as the log channel

Instead of a dedicated file descriptor (`PROCMAN_LOG_FD`) and accompanying
environment variable, the nested formation simply uses **stdout (fd 1)**
as the structured log channel. This eliminates all extra fd plumbing and
env-var coordination.

### How it works

**Parent formation:** For each child process, creates a `SOCK_SEQPACKET`
socketpair. The send side becomes the child's stdout; the recv side stays
in the parent and is read by a `DualRelay` dispatcher.

**Child formation (procman):** Probes its own stdout (fd 1) via
`getsockopt(SO_TYPE)`. If it's a `SOCK_SEQPACKET` socket, the child knows
it's nested under a parent formation. It:
1. Sets `O_NONBLOCK` on stdout (safe because the framer is the sole writer)
2. Replaces its sink with a `FramerHandler` that writes binary frames to fd 1

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
| `flags` | uint8 | Bitmap: bit0=has_tag, bit1=has_groups, bit2=has_attrs |
| `level` | int32 LE | slog.Level as int (debug=-4, info=0, warn=4, error=8) |
| `msglen` | uint16 LE | Length of message text (max 65535) |
| `message` | bytes | Record message text |
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

`NewChildFramer()` in `pkg/writelog/relay.go`:

```go
func NewChildFramer() *slog.Logger {
    typ, err := syscall.GetsockoptInt(1, syscall.SOL_SOCKET, syscall.SO_TYPE)
    if err != nil || typ != syscall.SOCK_SEQPACKET {
        return nil  // not nested → root mode
    }
    syscall.SetNonblock(1, true)
    return slog.New(NewFramer(1, slog.LevelDebug))
}
```

Called from `Formation.Run()`. If stdout is a SEQPACKET socket, the
formation's sink is replaced by a framer.

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

No dedicated log fd. The protocol uses fd 1 (stdout) implicitly.

- **Root process** (stdout is a terminal or pipe): `NewChildFramer()`
  returns nil → uses the user-configured sink (e.g. termhandler on
  `os.Stdout`). No frames — all output is rendered directly.
- **Nested process** (stdout is a SEQPACKET socket): `NewChildFramer()`
  returns a framer → every record is binary-encoded and sent up stdout.
- **Non-procman child** (writes text to stdout): text is split on
  newlines by the parent's `DualRelay` and logged under the child's tag.
  No structure is lost that didn't already exist.

## Per-child sockets

Each `Formation` creates **one SOCK_SEQPACKET socketpair per child process**.
The send side is the child's stdout (fd 1, via `exec.Cmd.Stdout`). The recv
side stays in the parent and is read by a `DualRelay` goroutine that knows
the child's tag.

This per-child design means:

- The relay knows each child's tag from the process loop — no need for the
  child to stamp its own tag or carry parent ancestry.
- When the child exits, its send-side end-of-file is naturally detected
  by the relay (SEQPACKET recv sees EOF when all write-end refs are closed).
- Non-procman children write text through the same socket; the relay
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

`DualRelay` in `pkg/writelog/relay.go`. One per child, launched from
`Formation.Run()`.

```go
func DualRelay(recv *net.UnixConn, parentSink *slog.Logger,
               childTag string, wg *sync.WaitGroup)
```

- Reads messages from `recv` in a loop with a `MaxFrameSize+1024` buffer.
- Fast gate: `IsFramePrefix(data)` checks byte 0.
- Frame path: `UnmarshalBinary` → `emitRelayedFrame`.
- Text path: `textBuf.feed` → `emit` for each complete line.
- On read error / EOF: flushes any remaining partial text line, exits.
- Teardown: parent sets a past read deadline → reader wakes → exits.

## Degradation behavior

- **Non-procman child** (kubelet, etcd): writes text to stdout → relay
  splits text on newlines → logged as text under the child's tag. No
  structured attrs, but no loss vs. a non-nested procman.
- **Parent crashes before relay reads**: child's send buffer holds some
  frames; when the recv socket is closed, the child's next `sendto`
  returns EPIPE → framer silently drops further records.
- **No SEQPACKET stdout** (root mode): `NewChildFramer()` returns nil;
  formation uses the user's sink (termhandler, etc.). The relay still runs
  for children; it feeds the user sink with structured records when
  children emit frames, or with text for non-procman children.

## runkube integration points (not in this repo)

1. **`sandbox.go`** — no change needed for the log channel: the sandbox
   already forwards stdout (fd 1) into the container; the procman child
   inside the sandbox auto-detects it.  No `PROCMAN_LOG_FD`, no
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
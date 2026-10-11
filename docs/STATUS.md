# Status reporting for processes, formations, and sub-components

**Status:** Implemented.

- `pkg/status` — the vocabulary (`State`, `*Error`, `Snapshot`) and the
  child-side `Reporter`.
- `pkg/writelog` — the wire contract: a status is an ordinary framed record
  carrying the reserved `status`/`since`/`detail` attrs; `DualRelay` takes an
  optional `StatusObserver`.
- `pkg/termhandler` — the status registry (`StatusStore`: `Statuses()`,
  `Status(identity)`), fed by the relay, by `ProcessStarted`/`ProcessExited`,
  and by `ReportStatus`; the renderer folds the state into the message as a
  leading label.
- `pkg/process` — `Process.Status`, `Formation.Status`, the interval/backoff
  pollers, `StatusView`/`ComponentStatus`, and `StatusViewFromContext`.

See "As built" (§12) for the deltas from the design below.

Consumer: `~/Code/runkube` (`ControlPlane.Run` builds a `process.Formation` of
etcd / kube-apiserver / controller / scheduler / webhook / kms / jwt-signer /
kubelet / criserver). runkube is the only intended consumer, so every
abstraction below must be usable from `comps.go` + `controlplane.go` without
new dependencies.

---

## 1. Problem

procman today knows exactly two things about a process: its log output, and
whether it is still running. `Formation.Run` starts everything and tears
everything down when the first process exits.

That is not enough for runkube. Several components have a real lifecycle beyond
"running / not running":

- the webhook has a TLS cert chain (`pki.Leaf`) that can become invalid;
- etcd can be up but un-reachable (`etcdctl endpoint health`);
- the controller/jwt-signer depend on a reachable apiserver (their client
  config points at it, so before the apiserver serves they log connection
  errors forever);
- a node's kubelet can be running while its cgroup root does not exist.

Today that information exists only as prose inside log lines, so nothing
upstream can act on it, and it cannot be relayed through a nested formation
(which is exactly the situation in runkube: the node formation is a child
process of the control-plane formation).

We want:

1. a **pull** API: `Status(ctx) error` on a process and on a formation;
2. a **push** API with the same ergonomics as `writelog.TaggedLogger`: a
   `Reporter` a sub-component can hold onto and call when it changes;
3. a typed vocabulary for the answer, so a caller can distinguish
   *healthy / degraded / broken*, and — critically — **broken-recoverable** vs
   **broken-fatal**, i.e. "keep waiting" vs "stop waiting";
4. the answer to survive a process boundary and a nested formation, exactly
   like `Frame.Tag` does for the logger;
5. a **status registry** in the root `termhandler` so a human (or an embedder)
   can see the current state of every component in one place, not just the
   last log line.

## 2. Goals / non-goals

**Goals**

- One vocabulary (`status.Error`) shared by process, formation, and
  sub-component levels.
- Status travels over the *existing* framed log channel — no new socket, no
  new fd, no new wire framing format.
- Poll (status callback) and push (reporter) produce identical records.
- Zero-cost when unused: a formation with no `Status` callbacks and no status
  frames behaves exactly as today (no goroutines, no timers, no allocations
  on the log hot path).
- Works stand-alone (no parent formation) and nested, in-process, and
  across a process boundary.

**Non-goals**

- Not a health-check *scheduler*: procman never probes anything itself; it
  only calls the callbacks the embedder supplied. Interval/backoff policy is
  the caller's (with a helper provided).
- Not a replacement for exit semantics. A process that exits still brings the
  formation down; status never resurrects or masks an exit.
- Not k8s health endpoints. runkube's components keep whatever HTTP probes
  they have; this is the *in-process* view.

## 3. Vocabulary

### 3.1 Severity

```go
package status // github.com/hoskeri/procman/pkg/status

// State is the coarse health classification of a component.
type State uint8

const (
    // StateUnknown is the zero value: the component exists but has never
    // reported a status.  It carries a live pid (see Snapshot.PID).
    StateUnknown State = iota
    // StateUnknownProcess is "there is no live process here": no pid, or the
    // process exited and was reaped, and no report has been received.  It is
    // the specialization of Unknown that makes a missing component
    // distinguishable from a silent one, and it is always timestamped (the
    // moment the child channel closed) so staleness is visible.
    StateUnknownProcess
    // StateStarting means "not ready yet, this is expected" (no cert, waiting
    // for a dependency).  Never fatal.
    StateStarting
    // StateHealthy means the component is serving.
    StateHealthy
    // StateDegraded means serving, but something is wrong (slow, retrying).
    // Never fatal.
    StateDegraded
    // StateUnhealthy means not serving right now, but a retry may fix it
    // (waiting for etcd to come back).  Recoverable.
    StateUnhealthy
    // StateFailed means the component is broken in a way procman should not
    // wait out (bad flags, missing PKI material, permanent config error).
    // Fatal.
    StateFailed
)
```

`State` is *orthogonal to lifecycle impact* only for the last three; the
mapping is:

| State        | Recoverable | Teardown if unrecovered |
|--------------|-------------|--------------------------|
| `Unknown`    | yes         | no                       |
| `UnknownProcess` | yes     | no   (see below) |
| `Starting`   | yes         | no                       |
| `Healthy`    | n/a (no error) | no                   |
| `Degraded`   | yes         | no                       |
| `Unhealthy`  | yes         | no                       |
| `Failed`     | no          | yes                      |

`Recoverable()` on `State` returns `true` for everything except `StateFailed`;
that single method is the "should the higher level system keep waiting" bit
the ask asks for, and it is the *only* thing the aggregation logic in §6 reads.

**Process liveness is part of the status, not a separate channel.** The unix
process state is folded into the same vocabulary and snapshot (see §5): a
`Formation` knows each child's pid the moment it spawns it, so the registry can
distinguish "process never reported" (`StateUnknown`, pid live) from "process is
gone" (`StateUnknownProcess`, no pid, `Since` = child-exit time). A pid that
cannot be reaped, plus the exit code when it can, is surfaced the same way —
the registry entry for an exited child is `StateUnknownProcess` with
`ExitCode`, not a deleted row, so an operator sees *why* a component is
missing.

### 3.2 Error type

`Status` must return `error` (Go convention, `errors.Is/As`-able, nil == healthy),
so the classification is carried by a concrete error type:

```go
// Error is the canonical status error.  A nil *Error means healthy.
//
//	nil                        -> StateHealthy
//	&Error{State: ...}         -> that state
//	any other non-nil error    -> StateUnhealthy + Recoverable() == true
type Error struct {
    State    State
    Reason   string        // short, operator-facing summary
    Detail   string        // optional longer text
    Err      error         // wrapped cause, for errors.Is/As chains
    Since    time.Time     // when the condition was first observed (0 = now)
    Attrs    []slog.Attr   // structured extras (see below)
}
```

Rules:

- `Error()` renders `Reason` (plus `": " + Detail` when Detail is set) — it is
  the message that ends up in the log line.
- `Unwrap()` returns `Err`.
- `Recoverable() bool` — simply `e.State.Recoverable()`. There is no override
  knob: a caller that wants "unhealthy but keep waiting" uses `StateUnhealthy`;
  "broken, stop waiting" uses `StateFailed`. YAGNI.
- `Is(target error)`: `errors.Is(err, status.ErrFailed)` matches **any**
  `*Error` whose `State == StateFailed`; likewise `status.ErrUnhealthy`,
  `status.ErrDegraded`, `status.ErrStarting`, `status.ErrHealthy`. Exact
  value matches (pointer identity / a sentinel wrapped as `Err`) also work
  through the normal chain.

Constructors, one per state, so callers never spell out the struct:

```go
func Starting(format string, a ...any) *Error
func Degraded(cause error, format string, a ...any) *Error
func Unhealthy(cause error, format string, a ...any) *Error   // recoverable
func Failed(cause error, format string, a ...any) *Error      // fatal
func Wrap(err error, st State, format string, a ...any) *Error
```

`*Error` also implements `slog.LogValuer`, so a status can be attached as a
single structured attribute and still render:

```go
func (e *Error) LogValue() slog.Value   // group: state, reason, detail, since, cause
```

### 3.3 Formatter (renderer)

A status record renders as one line, distinct from log lines but produced by
the same handler pipeline:

```
                 api | UNHEALTHY etcd client: connection refused (2m14s)
          etcd | HEALTHY   serving 127.0.0.1:2379
        webhook | DEGRADED cert not yet loaded, retrying
```

Implementation: the status record is a normal framed `slog` record with

- the component tag path in `Frame.Tag` (identical to logging), and
- two reserved attributes: `status` (`"healthy"|"starting"|"degraded"|
  "unhealthy"|"failed"` — reserved name, like `tag`/`stream` today) and
  `since` (duration since first observed), plus a level mapping:

| State      | slog.Level | why |
|------------|-----------|-----|
| `Healthy`  | `Debug`    | chatty, only interesting in `--log-level debug` |
| `Starting` | `Info`     | normal |
| `Degraded` | `Warn`     | |
| `Unhealthy`| `Error`    | |
| `Failed`   | `Error`    | |

So a status costs *nothing new* on the wire: it is a record. The level is the
only thing that needs a stable mapping, which means it is automatically subject
to the existing `--log-level` resolver (a `webhook` override to `error` will
hide its `degraded` chatter — desirable). **Statuses are never filtered out of
the registry** (§5), only out of the log line.

## 4. Child-side API (push), mirroring the logger

`writelog.TaggedLogger(tag)` probes fd 1/2, reuses an installed `FramerHandler`,
and falls back to `slog.Default().WithGroup(tag)`. The status twin is
`status.Reporter`:

```go
// package status

// Reporter publishes component status.  Zero value is not usable; build one
// with TaggedReporter or NewReporter.
type Reporter interface {
    // OK reports healthy, clearing any previous condition.
    OK(reason string, attrs ...slog.Attr)
    // Starting / Degraded / Unhealthy / Failed mirror the constructors; they
    // take an error-or-nil cause plus a format string.
    Starting(format string, a ...any)
    Degraded(err error, format string, a ...any)
    Unhealthy(err error, format string, a ...any)
    Failed(err error, format string, a ...any)
    // Snapshot returns the most recently reported status (StateUnknown if
    // none).
    Snapshot() Snapshot
}
```

Obtained exactly like the logger:

```go
rep := status.TaggedReporter("validate")  // runkube webhook component
```

`TaggedReporter` reuses `writelog.ChildSinks` (so it probes the same fds and
gets the same framers), and when it is already nested under a
`FramerHandler`-carrying default, it reuses that handler — the mirror of
`taggedLogger`'s reuse branch. When not nested, it degrades to
`slog.Default()` so runkube's webhook binary works both under runkube and
standalone (`runkube webhook` alone, or under `go run`).

`Reporter` is stateless apart from the last `Snapshot`, which it publishes
into the registry (§5) so an embedder can read it back. It is safe for
concurrent use and never blocks: `Handle` on a framer drops on `EAGAIN`, and
status records are low-frequency by construction.

## 5. Parent side: the status registry

`termhandler.TermHandler` already owns the parent's receive sockets per child
(`openChannel`) and the relay. Add a registry next to `relayState`:

```go
// StatusStore is implemented by TermHandler and exposes the current status
// of every component seen in the formation.
type StatusStore interface {
    // Statuses returns a snapshot copy of the registry keyed by identity
    // ("webhook/validate", "api-2", ...).
    Statuses() []status.Snapshot
    // Status returns the snapshot for one identity, ok=false if unknown.
    Status(identity string) (status.Snapshot, bool)
}

// Snapshot is one component's latest known state, whether that state came
// from a status report or from the formation observing the unix process.
type Snapshot struct {
    Identity string        // full display path relative to the formation
    State    status.State
    Reason   string        // "" for a liveness-derived state
    Detail   string
    Since    time.Time     // when this state began (never zero; see below)
    Age      time.Duration // computed at read time
    Recoverable bool

    // Process facts, filled in by the formation/sink rather than reported:
    //   PID > 0 and Exited == false -> StateUnknown means "alive but silent"
    //   Exited == true              -> State is StateUnknownProcess
    PID      int
    Exited   bool
    ExitCode int  // -1 when unknown (signalled/not yet observed)
}
```

Every child gets a registry row **at spawn time**, before any report arrives:
`ChildFDs` registers the identity with `PID` (passed alongside `tag`/`index`)
in state `StateUnknown`. `DualRelay` already decodes every record and knows
`childTag` + `frame.Tag`; when the decoded record carries the reserved
`status` attr, the relay:

1. stores/merges the snapshot under `childTag + "/" + frame.Tag`
   (mirroring `LogLevels.ForIdentity`'s path convention, so a single status
   path style is used everywhere), and
2. re-emits it into the parent sink as an ordinary record (unchanged
   behaviour).

When a child's channel closes (or `exec.Cmd.Wait` returns), the formation
marks that identity — and only the process-level identity, i.e. the row whose
`PID` equals the reaped pid — as `Exited`, clears `PID`, and (if the row never
moved past `StateUnknown`) sets `State = StateUnknownProcess` with
`Since = exit time` and `ExitCode` from the `WaitStatus`. If the child had
reported, its last state is preserved (a process can die *while* healthy, and
that is a meaningful fact), but `Exited` stays set and `Status()` on that
identity reports the exit alongside the last state. So "unknown" is fully
specialized:

| Pid      | Reported? | State                 | Since         |
|----------|-----------|-----------------------|---------------|
| live     | no        | `StateUnknown`        | spawn time    |
| live     | yes       | reported state        | state began   |
| gone     | no        | `StateUnknownProcess` | exit time     |
| gone     | yes       | last reported state   | state began   |
| not spawned / no channel | `StateUnknownProcess` | process start time |

Every row is timestamped, so `Age` is always meaningful — including for a
component that has never said anything.

**Nested formations for free.** When the nested formation's own
`TermHandler` is nested, its records are framed and go through its parent's
relay, which runs the same two steps. A status from a kubelet sub-component
lands in the control-plane root registry as `node-1/kubelet/certwatch`.
The relay is the *only* new code needed — no per-level plumbing.

**Sub-components for free.** Same reason: `Frame.Tag` already carries the
component path, and sub-components share the process's channel.

Registry semantics to keep it cheap and correct:

- keyed map + `sync.Mutex`, copy-on-read; no goroutine, no timer.
- `OK` on the same identity clears the condition (`State = StateHealthy`,
  `Since = now`); a repeated report of the same state only refreshes
  `Reason`/`Detail` (and keeps `Since`), so the "2m14s" suffix is meaningful.
- rows are created on spawn and updated on exit, never silently deleted, so
  `Statuses()` has a row for every process the formation ran — including the
  ones that died or never spoke. Unknown/never-reported identities can be
  filtered by the caller on `State` (a `Health()` convenience listing every
  child with liveness filled in is unnecessary: `Statuses()` already is that).
- the liveness half of a row is owned by the formation (which knows the pid
  and the `WaitStatus`); the reported half is owned by the relay. The two
  update different fields, so there is no contention beyond the map mutex.
- **level-independent**: registry updates happen even when the log line is
  filtered out by the resolver. This is the one deliberate divergence from the
  log path and it is what makes `healthy` observable under `--log-level error`.

## 6. Lifecycle: who is allowed to tear the formation down

Per the decision taken for this design:

> Process-level status is reported upward and aggregated; only the Formation's
> own `Status` callback can return a fatal error that ends `Run`.

### 6.1 Process level

```go
type Process struct {
    ...
    // Status, when non-nil, is polled on the formation's status interval.
    // A nil error means healthy.  The result is published to the status
    // channel/registry.  A fatal (*status.Error with StateFailed) result is
    // surfaced to the formation's Status (see below); it never by itself ends
    // Run.
    Status func(ctx context.Context) error
}
```

`Formation.Run` starts **one** poller per process with a `Status` callback
(and, when at least one process has one, a shared "one of these went
unhealthy" retry/backoff schedule):

```
poll every StatusInterval (default 5s; Formation.StatusInterval overrides)
  err := p.Status(ctx)
  if err == nil           -> reporter.OK("") ; mark healthy
  else                    -> classify; publish; update backoff
```

Backoff: on a **recoverable** failure, poll again after
`StatusBackoff` (default 1s) for up to `StatusBackoffMax` (default 30s),
capped by the same interval; the *observable* state stays `Unhealthy`/`Starting`
the whole time (so the registry shows "unhealthy for 2m14s"), only the poll
rate changes.

**Fatal states keep being polled, at the slow backoff rate.** A `Failed`
result is not final: the cause may be external and fixable (a cert reloaded by
another component, a dependency restarted), so the poller keeps checking at
`StatusBackoffMax` instead of stopping. If the callback later returns nil the
component goes back to `StateHealthy` and the registry is never left stuck on
a stale `Failed`. Retrying is cheap because a poll is an in-process call, not
a network probe chosen by procman.

**Polling stops when there is nothing left to poll.** A poll is skipped (and
the row left as `StateUnknownProcess`, per §5) when the process is gone:
`pidAlive(pid)` (signal 0) plus the formation's own reaped-exit set are both
consulted, so a dead process is never polled and a pid that the OS has
recycled cannot make a dead child look alive.

**A process with no `Status` callback still gets a state from its children.**
A supervisor may have no probe of its own while its real components run deeper
(a node whose kubelet and CRI server are probed inside a sandbox). Those
sub-components report on the process's child channel as
`<process>/<component>`, and the relay hook (`process.ChildStatusSource`,
implemented by `termhandler.TermHandler.AddStatusObserver`) folds them onto the
process's track: the worst sub-component state wins, and an own report (when
present) always takes precedence. Sub-component statuses are also
level-independent (see §5): a child's `healthy` line crosses a nested boundary
even under `--log-level error`, because it is the parent's registry that
observes it, while the parent still applies its own level when rendering.

`pollInterval` continues to exist unchanged for exit polling; status is
orthogonal.

### 6.2 Formation level

```go
type Formation struct {
    ...
    // Status, when non-nil, is called by Run on each poll tick after the
    // per-process callbacks have been evaluated.  It receives the aggregate
    // view of the formation's components.
    //
    // Returning a fatal *status.Error ends Run and is returned to the caller
    // (siblings are torn down exactly as on an exit).  Returning any other
    // error — including a recoverable one — only updates the reported status;
    // Run keeps waiting.
    Status func(ctx context.Context) error
}
```

The poller skips any process that is known-dead (see §6.1) and passes that
fact on, so the embedder can distinguish "unhealthy and trying" from "gone".
The embedder decides policy by reading the aggregate. To make that ergonomic
without forcing a `StatusStore` dependency into `pkg/process`, the aggregate
is passed as a plain struct:

```go
// in package process
type StatusView struct {
    // Components maps process identity ("api", "api-2") to the last reported
    // status of that process's root callback, in Formation.Processes order.
    Components []ComponentStatus
}
type ComponentStatus struct {
    Tag    string
    Index  int
    State  status.State  // reported state, or Unknown/UnknownProcess
    Reason string
    Since  time.Time
    Recoverable bool
    Fatal  bool   // State == Failed: the component will not recover on its own

    // Liveness (unix process state), filled in by the formation:
    PID      int
    Exited   bool
    ExitCode int  // -1 when unknown
}
```

So runkube writes the policy it actually wants:

```go
fm := &process.Formation{
    ...
    Status: func(ctx context.Context) error {
        for _, c := range view.Components() {
            if !c.Fatal {
                continue
            }
            switch c.Tag {
            case "webhook":
                return status.Failed(nil, "webhook cert %s unusable", reason)
            case "etcd":
                return status.Failed(nil, "etcd data dir %s", reason)
            }
        }
        return nil
    },
}
```

and a component that is merely `Unhealthy` and expected to recover never
brings anything down — the "keep waiting" bit the ask asks for is expressed by
*which* state the callback returns, and the process-level callback cannot cause
a teardown by construction.

### 6.3 Sub-components inside one process

A sub-component (runkube's webhook `validate`/`mutate` components) never gets a
`Formation`. It holds a `status.Reporter` and publishes when it changes; the
frames ride its own stdout and land in the parent registry. It has no teardown
power: only the formation's own `Status` can end `Run`. That is the same
division of authority the design asks for.

### 6.4 Interaction with exit semantics

Unchanged. `pf()`/`ExitError` and the errgroup cancel rule stay exactly as they
are; `Run` returns either an `*ExitError` (first self-exit) or the fatal
`*status.Error` from the formation's `Status`. To keep the two distinguishable
for callers:

- `*process.ExitError` → the formation ended because a process exited.
- `*status.Error` → the formation ended because the embedder said so; carries
  the failing `ComponentStatus` in `Attrs`/`Detail` so `errors.As` gives the
  full story.

If the user cancels `ctx`, `Run` returns `nil` (unchanged), and a fatal status
racing with the cancel is dropped (ctx.Err() != nil ⇒ not attributed), exactly
like `Process.run` does today.

## 7. Wiring sketch

```go
// pkg/process/process.go
type Process struct { ...; Status func(ctx context.Context) error }
type Formation struct {
    ...
    // StatusInterval / StatusBackoff / StatusBackoffMax as above.
    // Status is the formation-level policy callback (§6.2).
    Status func(ctx context.Context) error
}

func (l *Formation) Run(ctx context.Context) error {
    ...
    if l.hasStatus() {
        l.startStatusLoop(ctx, eg)  // per-process pollers + formation policy
    }
    // On exit each poller's process row is closed out with its WaitStatus
    // (Exited, ExitCode, State -> UnknownProcess when never reported).
    return eg.Wait()   // fatal *status.Error wins over *ExitError if both arrive
}
```

```go
// pkg/termhandler/termhandler.go
func (h *TermHandler) Statuses() []status.Snapshot   // StatusStore
func (h *TermHandler) Status(identity string) (status.Snapshot, bool)
// ChildFDs gains a pid argument (or a ChildSpec struct) so a row can be
// created at spawn time in StateUnknown with that pid.
```

```go
// pkg/writelog/relay.go (dualRelayLoop / emitRelayedFrame)
// after decoding: if st, ok := frameStatus(frame); ok { h.registry.set(childTag, frame.Tag, st) }
```

`cmd/procman/main.go`: no behaviour change is required. A `--status` flag could
later print `TermHandler.Statuses()` on SIGUSR1; out of scope for v1.

## 8. Non-log rendering (optional, v2)

v1 is "log line + registry API". If runkube later wants a periodic dashboard,
the registry is enough to add it (a ticker in the root `TermHandler` printing
a one-line-per-identity summary to stderr), with no protocol change. Explicitly
deferred so v1 stays small.

## 9. Testing

Unit (`pkg/status`): constructors set State/Reason; `Error()`/`Unwrap`;
`errors.Is(err, status.ErrFailed)` matches only fatal; `LogValue` shape;
`State.Recoverable` table.

Unit (`pkg/writelog`): a `FramerHandler` + `status.Reporter` produces a frame
carrying the reserved `status` attr and correct level; round-trip through
`DualRelay` yields the same attr (mirrors the existing
`TestChildFDsRendersInlineGroupAttrs` pattern with injected socketpairs).

Unit (`pkg/termhandler`): relay populates `Statuses()` under
`webhook/validate`; a level-filtered `healthy` still updates the registry;
channel close removes the identity.

Unit (`pkg/process`): poll interval fires; fatal process status does **not**
end `Run`; formation `Status` returning fatal does; recoverable does not;
`StatusBackoff` shrinks the poll rate after a recoverable failure and a `Failed`
state keeps polling at `StatusBackoffMax`; a dead process is not polled and its
`ComponentStatus` reports `Exited` + exit code; ctx cancel returns `nil`.

Unit (`pkg/termhandler`): a spawn registers the identity with a pid in
`StateUnknown`; the child's channel closing without any report yields
`StateUnknownProcess` with `Since` set and `ExitCode` populated; a child that
reported and then exited keeps its reported state but has `Exited == true`.

Integration (`tests/`): a fixture Procfile whose child uses
`status.TaggedReporter` (a tiny Go fixture under `tools/`, alongside
`trebuchet`) reports healthy→unhealthy→healthy through a real socketpair and
the integration test asserts the parent registry saw all three.

## 10. Decisions taken (was: open questions)

1. **Registry lifetime across a status-free child — resolved.** Unix-level
   process state is part of the status itself, so `Unknown` is *specialized*:
   `StateUnknown` (pid live, never reported) vs `StateUnknownProcess` (no pid
   / exited), and every process status is timestamped (`Since` = spawn time at
   the latest). Rows are created at spawn and updated at exit rather than
   deleted. Implemented in §3.1 (`State`), §5 (`Snapshot`, the
   pid/reported matrix) and §6.1 (skip polling a dead process).

2. **Should a fatal *process* status be re-checked — resolved.** Yes: keep
   polling at `StatusBackoffMax`, and stop as soon as the process itself is
   dead (there is no point polling a nonexistent process). Implemented in
   §6.1.

3. **Wire compatibility — confirmed as designed.** No new frame format; status
   is an ordinary framed record with reserved attr names, so mixed parent/child
   builds interoperate. The reserved-attr semantics need a note in `AGENTS.md`
   §C when this is implemented.

4. **`TermHandler.Reporter` — deferred, recorded for a future extension.**
   The poll callback is sufficient for v1. When wanted it is trivial —
   `status.NewReporter(writelog.WithTag(h.Logger(), tag))` — and it would let
   a formation report a component out-of-band (e.g. etcd readiness after a
   retry loop succeeds). Not in this change; noted here so the registry
   (`Statuses()` as the single read API) is designed to accommodate it.

## 11. Follow-ups (done, see §12)

- `status.StateUnknownProcess` must render as something like
  `  —     ` in the log line (nothing reported, process gone) so a
  status-free child does not produce a burst of `Unknown` lines at spawn.
- `pidAlive` needs care on a recycled pid: prefer the formation's reaped-exit
  set over the signal-0 probe, and only treat a pid as live if it was never
  reaped by this formation.
- Document the reserved `status` / `since` attr names (and the level map) in
  `AGENTS.md` §C next to `tag`/`stream`.


## 12. As built — deltas from the design

Everything in §3–§6 is implemented as described. The deltas:

1. **The view travels on the context, not the signature.**
   `Formation.Status` stays `func(ctx context.Context) error`; the aggregate is
   attached to the ctx by `Run` and read with
   `process.StatusViewFromContext(ctx) (StatusView, bool)`.

2. **`DualRelay`'s observer is variadic.** `DualRelay(recv, sink, tag, stream,
   wg, obs ...StatusObserver)` so existing callers are unchanged and a nil
   observer means "ignore statuses".

3. **`writelog` stays independent of `pkg/status`.** The relay reports
   statuses in wire terms (`writelog.StatusRecord{State, Reason, Detail,
   Since}`, the state still a string) and `termhandler` maps it to the domain
   types; `status` depends on `writelog` (it needs `ChildLogger` for
   `TaggedReporter`), not the reverse.

4. **The state is rendered by the handler, not baked into the message.** The
   reporter sends the clean reason as the record message plus a reserved
   `status` attr; `renderer.renderPayload` prepends the upper-cased state
   (`UNHEALTHY etcd unreachable`) and filters the reserved attrs. So the wire
   message stays usable as the registry's reason, and a plain `TextHandler`
   sink still shows the state as an attribute.

5. **`Process.run` is now Start/Wait, not Run.** That gives `Run` the exact pid
   (published as `ProcessStarted`) and the exact reap moment (published as
   `ProcessExited` with the exit code), which is what makes "silent process"
   vs "gone process" observable rather than inferred.

6. **The "is the process gone" signal is the formation's own reap**, not a
   `signal 0` probe: a `statusTrack` holds a `done` channel closed by the
   process goroutine. This is immune to pid recycling and needs no syscall.

7. **Poller lifetimes are separate from the errgroup.** Status goroutines get
   their own cancel and are waited for after `eg.Wait()`, so `Run` never leaks
   a poller and never waits on one while returning.

8. **The renderer filter list grew:** `status`, `since`, `detail` are reserved
   alongside `tag`/`stream`. (Decisions §10.3 called for documenting this in
   `AGENTS.md` §C — done.)

9. **The CLI reports a fatal formation status explicitly** (`cmd/procman`:
   `errors.AsType[*status.Error]` + `Fatal()` -> exit 1), distinct from an
   `ExitError`'s exit code.

10. **`TermHandler.Reporter` remains deferred** (decision §10.4), and v1 has no
    status dashboard beyond the registry API — unchanged from the design.

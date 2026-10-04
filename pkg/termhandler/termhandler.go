// Package termhandler renders procman's process output to a terminal and owns
// the child log channels used to capture it.
//
// TermHandler is the output facade for a formation.  It is constructed with
// the process's standard streams and a context, detects whether the process is
// nested inside a parent formation, renders (or frames) its own records, and
// vends per-child descriptors via ChildFDs.  The context tears the facade down
// on cancellation: terminal state (echo) is restored and relay goroutines are
// stopped.
package termhandler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/nerdmaster/terminal"

	"github.com/hoskeri/procman/pkg/writelog"
)

// ANSI modes
const (
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"
)

// echoFlag is the ECHO bit of c_lflag; its value (0x8) is identical across
// the Unices procman targets (see syscall/zerrors_*; Linux only defines the
// ECHO* variants, so it is spelled out here).
const echoFlag = 0x8

var fgcolors = []string{
	"\033[38;5;1m",
	"\033[38;5;2m",
	"\033[38;5;3m",
	"\033[38;5;4m",
	"\033[38;5;5m",
	"\033[38;5;6m",
	"\033[38;5;9m",
	"\033[38;5;10m",
	"\033[38;5;11m",
	"\033[38;5;12m",
	"\033[38;5;13m",
	"\033[38;5;15m",
}

// fgcolors256 spreads 16 hues around the wheel on the 6x6x6 color cube,
// skipping the grays and whites (7, 15, 231, 251-255) so every entry stays
// saturated and readable on a dark background.
var fgcolors256 = []string{
	"\033[38;5;196m", // red
	"\033[38;5;208m", // orange
	"\033[38;5;214m", // goldenrod
	"\033[38;5;226m", // yellow
	"\033[38;5;154m", // chartreuse
	"\033[38;5;46m",  // green
	"\033[38;5;48m",  // spring green
	"\033[38;5;51m",  // cyan
	"\033[38;5;45m",  // turquoise
	"\033[38;5;39m",  // azure
	"\033[38;5;27m",  // blue
	"\033[38;5;63m",  // periwinkle
	"\033[38;5;99m",  // violet
	"\033[38;5;129m", // purple
	"\033[38;5;165m", // magenta
	"\033[38;5;201m", // pink
}

// fgcolorsTrue is the 24-bit analogue: saturated hues evenly spaced around
// the wheel, none of them white or near-white.
var fgcolorsTrue = []string{
	"\033[38;2;255;0;0m",     // red
	"\033[38;2;255;128;0m",   // orange
	"\033[38;2;255;192;64m",  // amber
	"\033[38;2;255;255;0m",   // yellow
	"\033[38;2;160;255;64m",  // chartreuse
	"\033[38;2;0;255;0m",     // green
	"\033[38;2;0;255;160m",   // spring green
	"\033[38;2;0;255;255m",   // cyan
	"\033[38;2;0;192;255m",   // sky
	"\033[38;2;0;120;255m",   // azure
	"\033[38;2;48;80;255m",   // blue
	"\033[38;2;120;72;255m",  // periwinkle
	"\033[38;2;180;0;255m",   // purple
	"\033[38;2;255;0;255m",   // magenta
	"\033[38;2;255;96;192m",  // pink
	"\033[38;2;255;128;128m", // coral
}

// colorSupport probes the environment the way common terminal tools do:
// COLORTERM=truecolor|24bit selects 24-bit color, COLORTERM=256color or a
// TERM ending in -256color selects the 256-color palette, and everything else
// falls back to the basic 16-color ANSI range.
func colorSupport() int {
	ct := os.Getenv("COLORTERM")
	if ct == "truecolor" || ct == "24bit" {
		return 2
	}
	if ct == "256color" || strings.Contains(os.Getenv("TERM"), "256color") {
		return 1
	}
	return 0
}

// PaletteFor returns the ANSI prefix palette for a color depth from
// [colorSupport]: 0 → 16 colors, 1 → 256 colors, 2 → 24-bit truecolor.
func PaletteFor(depth int) []string {
	if depth >= 2 {
		return fgcolorsTrue
	}
	if depth == 1 {
		return fgcolors256
	}
	return fgcolors
}

// Options configures a TermHandler.
type Options struct {
	Level slog.Leveler
	// Columns truncates each emitted line (prefix included) to this many
	// bytes. 0 (the default) auto-resolves to the terminal width when the
	// output is a tty, leaving truncation off otherwise; negative values
	// disable truncation explicitly.
	Columns int
	Colors  bool
	// Plain selects a plain slog.TextHandler renderer instead of the colored,
	// tag-prefixed term renderer.  It is ignored in nested mode (frames are
	// always used) and does not affect child log relays, which keep their
	// per-process identity.
	Plain bool
}

// resolverSetter is implemented by slog.Handlers that accept a per-identity
// level policy (renderer and writelog.FramerHandler).  The sink resolves the
// threshold from the component tag path, so a single process can carry
// different levels for its components.
type resolverSetter interface {
	WithResolver(resolver writelog.LevelResolver) slog.Handler
}

// TerminalWidth reports the width in columns of f when it is a terminal, or 0
// otherwise (including when f is nil or the size query fails). It backs the
// Options.Columns auto-resolution in New.
func TerminalWidth(f *os.File) int {
	if f == nil {
		return 0
	}
	conn, err := f.SyscallConn()
	if err != nil {
		return 0
	}
	var width int
	_ = conn.Control(func(fd uintptr) {
		if w, _, werr := terminal.GetSize(int(fd)); werr == nil && w > 0 {
			width = w
		}
	})
	return width
}

// IsTerminal reports whether f is a terminal. It mirrors the check the
// renderer performs internally so callers can branch on tty-ness (e.g. choosing
// an output handler) without constructing a handler.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	conn, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var isTerm bool
	_ = conn.Control(func(fd uintptr) {
		isTerm = terminal.IsTerminal(int(fd))
	})
	return isTerm
}

// NoEcho disables terminal echo on f when f is a terminal and returns a
// function that restores the previous terminal state. It returns a no-op when
// f is not a terminal or the termios round-trip fails. procman never reads
// stdin, so without this, keystrokes on the controlling terminal are echoed
// into the streaming log output — the caller restores the state on every exit
// path (os.Exit bypasses defer).
func NoEcho(f *os.File) func() {
	if f == nil {
		return func() {}
	}
	conn, err := f.SyscallConn()
	if err != nil {
		return func() {}
	}
	var saved syscall.Termios
	var ok bool
	_ = conn.Control(func(fd uintptr) {
		if _, _, ierr := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&saved)), 0, 0, 0); ierr == 0 {
			off := saved
			off.Lflag &^= echoFlag
			if _, _, oerr := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&off)), 0, 0, 0); oerr == 0 {
				ok = true
			}
		}
	})
	if !ok {
		return func() {}
	}
	return func() {
		_ = conn.Control(func(fd uintptr) {
			syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&saved)), 0, 0, 0)
		})
	}
}

// noEchoFirst returns the NoEcho restore function for the first stream that is
// a terminal (stdin first: echo is an input property), or a no-op.
func noEchoFirst(files ...*os.File) func() {
	for _, f := range files {
		if IsTerminal(f) {
			return NoEcho(f)
		}
	}
	return func() {}
}

// --- renderer: the colored, tag-prefixed text handler used in root mode ---

// renderer is the historical TermHandler implementation: a slog.Handler that
// prefixes each line with a bold, colored process tag.  It is an internal
// detail of the TermHandler facade.
type renderer struct {
	opts       Options
	tagPath    []string               // display tag path (innermost is last)
	groupPath  []string               // attr namespace path (innermost is last)
	override   slog.Leveler           // per-process level threshold (via WithOverride)
	resolver   writelog.LevelResolver // per-identity threshold policy (via WithResolver)
	color      string                 // ANSI color for the innermost tag
	attrs      []slog.Attr
	palette    []string // ANSI prefixes selected by colorSupport
	linePrefix string   // cached render of the tag path
	prefixVis  int      // visible width of linePrefix (see buildPrefix)
	mu         *sync.Mutex
	out        io.Writer
}

var _ slog.Handler = &renderer{}

// newRenderer builds a renderer writing to f (io.Discard when f is nil).  It
// resolves color auto-detection, palette depth, and column width from f.
func newRenderer(f *os.File, opts Options) *renderer {
	r := &renderer{out: f, mu: &sync.Mutex{}, opts: opts}
	if r.out == nil {
		r.out = io.Discard
	}
	// Colors auto-detect: enabled only when out is a terminal; --output term
	// forces them via Options.Colors = true.
	r.opts.Colors = r.opts.Colors || IsTerminal(f)
	r.palette = PaletteFor(colorSupport())
	// Columns auto-resolve: 0 (the default) means "terminal width" when out is
	// a tty, else no truncation. Negative values keep truncation off.
	if r.opts.Columns == 0 {
		r.opts.Columns = TerminalWidth(f)
	}
	return r
}

// colorFor hashes tag with FNV-1a to assign a consistent entry from the
// handler's palette (chosen by colorSupport at construction).
func (h *renderer) colorFor(tag string) string {
	hv := fnv.New32()
	hv.Write([]byte(tag))
	return h.palette[int(hv.Sum32())%len(h.palette)]
}

// buildPrefix returns the display tag prefix for the current tagPath,
// optionally colored. Tags are joined with "/" — the restricted tag
// character set (lowercase alphanumeric + dash) guarantees no ambiguity.
func (h *renderer) buildPrefix() string {
	// innermost tag for color
	innermost := ""
	if len(h.tagPath) > 0 {
		innermost = h.tagPath[len(h.tagPath)-1]
	}

	// Combine all tags into a single path, right-justified in 16 columns.
	combined := strings.Join(h.tagPath, "/")
	padded := fmt.Sprintf("%16s", shortenMiddle(combined, 16))
	// The uncolored prefix is padded (always exactly 16 ASCII bytes, since
	// tags are restricted to lowercase alphanumerics and dashes) plus
	// " | " — so its visible width is known without any escape scanning.
	h.prefixVis = len(padded) + 3
	prefix := padded + " | "

	if h.opts.Colors && innermost != "" {
		c := h.colorFor(innermost)
		return string(ansiBold) + c + prefix + ansiReset
	}
	return prefix
}

// shortenMiddle truncates s to fit maxLen by replacing the middle section
// with "...".  If s already fits it is returned unchanged.
func shortenMiddle(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	ellipsis := "..."
	avail := maxLen - len(ellipsis)
	if avail < 2 {
		return s[:maxLen]
	}
	left := (avail + 1) / 2 // left-heavy bias
	right := avail - left
	return s[:left] + ellipsis + s[len(s)-right:]
}

// WithOverride returns a handler that overrides the minimum log level for the
// upcoming group (set by the caller's subsequent WithGroup).  Unlike the old
// behavior, WithOverride does NOT set the group path — it only stores the level
// threshold; the group is added by the relay's WithGroup, avoiding group name
// duplication.
func (h *renderer) WithOverride(name string, lvl slog.Leveler) slog.Handler {
	h2 := h.clone()
	h2.override = lvl
	return h2
}

// WithResolver implements the resolverSetter interface.  The resolver is
// consulted in Enabled with the component tag path (everything after the
// leading process tag); a matched override wins over WithOverride and the
// ambient Options.Level.
func (h *renderer) WithResolver(resolver writelog.LevelResolver) slog.Handler {
	h2 := h.clone()
	h2.resolver = resolver
	return h2
}

// WithTag implements writelog.TagHandler.  It extends the display tag path and
// recomputes the prefix/color; slog WithGroup no longer affects the prefix.
func (h *renderer) WithTag(name string) slog.Handler {
	h2 := h.clone()
	h2.tagPath = append(h2.tagPath, name)
	h2.linePrefix = h2.buildPrefix()
	return h2
}

func (h *renderer) clone() *renderer {
	h2 := *h
	h2.tagPath = append([]string(nil), h.tagPath...)
	h2.groupPath = append([]string(nil), h.groupPath...)
	h2.attrs = append([]slog.Attr(nil), h.attrs...)
	return &h2
}

func (h *renderer) Enabled(ctx context.Context, l slog.Level) bool {
	if len(h.tagPath) == 0 && len(h.groupPath) == 0 {
		// Bare root handler — no display tag or attr group yet; slog never
		// calls Handle on it.
		return false
	}
	threshold := h.opts.Level
	if h.override != nil {
		threshold = h.override
	}
	if h.resolver != nil {
		if lvl, ok := h.resolver(writelog.RelativeTagPath(h.tagPath)); ok {
			threshold = lvl
		}
	}
	return l >= threshold.Level()
}

func (h *renderer) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message == "" && len(h.attrs) == 0 && rec.NumAttrs() == 0 {
		return nil
	}

	msg := h.renderPayload(ctx, rec)

	// Trim the payload *before* prepending the colored prefix: the prefix's
	// visible width is fixed (prefixVis), so the budget left for the payload
	// is Columns minus that. No escape scanning is needed — the ANSI codes
	// are added afterwards and never land in the trimmed region. Escapes
	// inside child output still count as bytes here (they are rare, and a
	// cut inside one is self-healing: an ESC begins every new line and
	// aborts any dangling sequence).
	if h.opts.Columns > 0 {
		avail := max(h.opts.Columns-h.prefixVis, 0)
		if len(msg) > avail {
			msg = msg[:avail]
		}
	}

	out := []byte(h.linePrefix + msg)
	if len(out) == 0 {
		return nil
	}
	if out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.out.Write(out)
	return err
}

// renderPayload returns the record's message plus its attributes rendered in
// slog's logfmt style.  The fast path (no attrs and no attr namespaces) returns
// the message unchanged, so high-volume raw process output pays no formatting
// cost.  Attributes are rendered with a stripped-down slog.TextHandler so
// quoting, value kinds, and nested groups are handled by the standard library.
// The synthetic tag/stream attrs added by the relay are not displayed (the tag
// is the prefix; the stream is metadata).
func (h *renderer) renderPayload(ctx context.Context, rec slog.Record) string {
	if len(h.attrs) == 0 && len(h.groupPath) == 0 && rec.NumAttrs() == 0 {
		return rec.Message
	}

	opts := &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey, slog.LevelKey:
				if len(groups) == 0 {
					return slog.Attr{}
				}
			case "tag", "stream":
				// Synthetic attrs added by the relay; the tag is the prefix
				// and the stream is metadata.  Reserved names.
				return slog.Attr{}
			}
			return a
		},
	}

	var buf bytes.Buffer
	var hh slog.Handler = slog.NewTextHandler(&buf, opts)
	for _, g := range h.groupPath {
		hh = hh.WithGroup(g)
	}
	if len(h.attrs) > 0 {
		hh = hh.WithAttrs(h.attrs)
	}

	// Format attrs only, then strip TextHandler's always-present empty msg.
	r := slog.NewRecord(time.Time{}, rec.Level, "", 0)
	rec.Attrs(func(a slog.Attr) bool {
		r.AddAttrs(a)
		return true
	})
	if err := hh.Handle(ctx, r); err != nil {
		return rec.Message
	}

	attrs := strings.TrimSpace(buf.String())
	attrs = strings.TrimSpace(strings.TrimPrefix(attrs, `msg=""`))
	if attrs == "" {
		return rec.Message
	}
	if rec.Message == "" {
		return attrs
	}
	if strings.HasSuffix(rec.Message, "\n") {
		return strings.TrimSuffix(rec.Message, "\n") + " " + attrs + "\n"
	}
	return rec.Message + " " + attrs
}

func (h *renderer) WithAttrs(attrs []slog.Attr) slog.Handler {
	h2 := h.clone()
	h2.attrs = append(h2.attrs, attrs...)
	return h2
}

// WithGroup implements the slog.Handler interface.  It records an attribute
// namespace (used to qualify attrs) and never changes the display prefix.
func (h *renderer) WithGroup(name string) slog.Handler {
	h2 := h.clone()
	h2.groupPath = append(h2.groupPath, name)
	return h2
}

// --- TermHandler: the output facade ---

// relayState tracks the per-child relay goroutines and their receive sockets
// so Close can unblock and wait for them.  It is held by pointer so the
// renderer-like clones produced by WithGroup share one lifecycle.
type relayState struct {
	once   sync.Once
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  []*net.UnixConn
	closed bool
}

// TermHandler is the output facade for a procman formation.  It owns the
// process's standard streams, terminal state, and child log channels.
//
// In root mode (stdout is not a parent formation's log socket) records are
// rendered to the terminal.  In nested mode (stdout is a SOCK_SEQPACKET log
// socket) records are encoded as binary frames and sent to the parent.
//
// ChildFDs creates a per-child SOCK_SEQPACKET channel and starts a relay that
// feeds the child's stdout/stderr back into this handler's sinks.
type TermHandler struct {
	nested     bool
	outHandler slog.Handler // sink for this process's stdout stream
	errHandler slog.Handler // sink for this process's stderr stream

	restore func()
	relays  *relayState
}

// New builds the output facade for a formation.
//
// The standard streams are used for terminal rendering, nested-mode detection,
// and child wiring; ctx cancels the facade (restoring terminal state and
// stopping relay goroutines) and should be the formation's lifetime context.
//
// Nested mode is detected by probing stdout: when it is a SOCK_SEQPACKET
// socket created by a parent formation, this process frames its own records
// instead of rendering.  stderr is probed independently; when it is not a
// socket, stderr frames are sent on the stdout channel.
func New(ctx context.Context, stdin, stdout, stderr *os.File, opts *Options) *TermHandler {
	h := &TermHandler{relays: &relayState{}}
	var o Options
	if opts != nil {
		o = *opts
	}
	if o.Level == nil {
		o.Level = slog.LevelInfo
	}

	// Nested mode: stdout is a parent formation's log socket.
	if fd, ok := seqpacketFd(stdout); ok {
		h.nested = true
		h.outHandler = writelog.NewFramer(writelog.SetupSendSocket(fd), writelog.StreamStdout, o.Level)
		if efd, ok := seqpacketFd(stderr); ok {
			h.errHandler = writelog.NewFramer(writelog.SetupSendSocket(efd), writelog.StreamStderr, o.Level)
		} else {
			// No stderr channel: keep the stream discriminator by framing on
			// the stdout socket with the stderr stamp.
			h.errHandler = writelog.NewFramer(writelog.SetupSendSocket(fd), writelog.StreamStderr, o.Level)
		}
		context.AfterFunc(ctx, h.Close)
		return h
	}

	// Root mode: render records to the terminal.
	if o.Plain {
		h.outHandler = slog.NewTextHandler(fileWriter(stdout, io.Discard), textOptions(o))
		h.errHandler = slog.NewTextHandler(fileWriter(stderr, io.Discard), textOptions(o))
	} else {
		h.outHandler = newRenderer(stdout, o)
		h.errHandler = newRenderer(stderr, o)
	}
	h.restore = noEchoFirst(stdin, stdout, stderr)
	context.AfterFunc(ctx, h.Close)
	return h
}

// Logger returns the sink for the formation's own records (stdout stream).
func (h *TermHandler) Logger() *slog.Logger {
	return slog.New(h.outHandler)
}

// ErrLogger returns the sink for the stderr stream.  In nested mode this is a
// framer; in root mode it is a terminal renderer.  Note that the root renderer
// ignores ungrouped records, so it is not suitable as a process-wide default
// logger; use it for child relays and tagged output only.
func (h *TermHandler) ErrLogger() *slog.Logger {
	return slog.New(h.errHandler)
}

// Nested reports whether the process was started by a parent formation (its
// stdout is a log socket).
func (h *TermHandler) Nested() bool { return h.nested }

// ChildFDs creates the child-side stdout and stderr descriptors for a process
// and starts the parent-side relays into this handler.  The caller wires the
// returned files into exec.Cmd.Stdout/Stderr and closes them once the process
// has exited (the relay exits on EOF, or on Close).
//
// tag and index identify the process; a positive index is folded into the
// identity ("web-2") so replicas get distinct prefixes and colors.  resolver
// is the process's log-level policy: the parent-side sink consults it per
// record with the component tag path, so component overrides layer on top of
// the process tag and formation default.  A nil resolver leaves the sink's own
// level in place.
func (h *TermHandler) ChildFDs(tag string, index int, resolver writelog.LevelResolver) (stdout, stderr *os.File, err error) {
	identity := tag
	if index > 0 {
		identity = fmt.Sprintf("%s-%d", tag, index)
	}

	stdout, err = h.openChannel(identity, writelog.StreamStdout, childSink(h.outHandler, resolver))
	if err != nil {
		return nil, nil, err
	}
	stderr, err = h.openChannel(identity, writelog.StreamStderr, childSink(h.errHandler, resolver))
	if err != nil {
		stdout.Close()
		return nil, nil, err
	}
	return stdout, stderr, nil
}

// relayDrainGrace bounds how long Close waits for relays to drain after their
// children close their send ends before force-closing the receive sockets.
const relayDrainGrace = 2 * time.Second

// Close restores terminal state and stops every relay goroutine.  Relays are
// given a grace period to drain naturally (their child send ends are expected
// to be closed); if one is still blocked after that, its receive socket is
// closed to unblock it.  Safe to call more than once and invoked automatically
// when the context passed to New is canceled.
func (h *TermHandler) Close() {
	if h.relays == nil {
		if h.restore != nil {
			h.restore()
		}
		return
	}
	h.relays.once.Do(func() {
		if h.restore != nil {
			h.restore()
		}
		// No new child channels after Close.
		h.relays.mu.Lock()
		h.relays.closed = true
		h.relays.mu.Unlock()

		done := make(chan struct{})
		go func() {
			h.relays.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(relayDrainGrace):
			h.relays.mu.Lock()
			conns := h.relays.conns
			h.relays.conns = nil
			h.relays.mu.Unlock()
			for _, c := range conns {
				c.Close()
			}
			<-done
		}

		h.relays.mu.Lock()
		h.relays.conns = nil
		h.relays.mu.Unlock()
	})
}

// openChannel creates one SOCK_SEQPACKET socketpair, starts a DualRelay for the
// receive side into sink, and returns the child-side send file.
func (h *TermHandler) openChannel(tag string, stream writelog.StreamKind, sink *slog.Logger) (*os.File, error) {
	if h.relays == nil {
		return nil, errors.New("termhandler: handler not initialized")
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, fmt.Errorf("termhandler: socketpair %s/%s: %w", tag, stream, err)
	}
	// Keep the send side blocking: non-procman children expect a blocking
	// stdout.  Procman children set O_NONBLOCK themselves after detecting the
	// socket type.
	recv := writelog.SetupRecvConn(fds[0])
	if recv == nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		return nil, fmt.Errorf("termhandler: setup recv %s/%s", tag, stream)
	}

	h.relays.mu.Lock()
	if h.relays.closed {
		h.relays.mu.Unlock()
		recv.Close()
		syscall.Close(fds[1])
		return nil, errors.New("termhandler: closed")
	}
	h.relays.conns = append(h.relays.conns, recv)
	// Start the relay while holding the lock: DualRelay's wg.Add must be
	// ordered before Close's wg.Wait.  Close flips closed under this same
	// lock and only starts Wait after releasing it, so either this Add is
	// visible to Wait or the closed check above rejects the channel.
	writelog.DualRelay(recv, sink, tag, stream, &h.relays.wg)
	h.relays.mu.Unlock()

	return os.NewFile(uintptr(fds[1]), fmt.Sprintf("%s-%s", stream, tag)), nil
}

// childSink applies a per-identity level policy to a handler when the handler
// supports it (renderer / FramerHandler).  A nil resolver means "no policy".
func childSink(handler slog.Handler, resolver writelog.LevelResolver) *slog.Logger {
	if resolver != nil {
		if rs, ok := handler.(resolverSetter); ok {
			handler = rs.WithResolver(resolver)
		}
	}
	return slog.New(handler)
}

// seqpacketFd returns the fd of f when it is a SOCK_SEQPACKET socket (a
// parent formation's log channel).
func seqpacketFd(f *os.File) (int, bool) {
	if f == nil {
		return 0, false
	}
	fd := int(f.Fd())
	if fd < 0 {
		return 0, false
	}
	typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil || typ != syscall.SOCK_SEQPACKET {
		return 0, false
	}
	return fd, true
}

// fileWriter returns f, or fallback when f is nil.
func fileWriter(f *os.File, fallback io.Writer) io.Writer {
	if f == nil {
		return fallback
	}
	return f
}

// textOptions maps Options onto slog.HandlerOptions for Plain mode.
func textOptions(o Options) *slog.HandlerOptions {
	return &slog.HandlerOptions{AddSource: false, Level: o.Level}
}

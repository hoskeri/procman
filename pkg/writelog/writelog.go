package writelog

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
)

// DefaultMaxQueue is the default bound on the number of complete lines a
// stream buffers between the subprocess (producer) and the slog drain worker.
// It is chosen as a sane default against a high-rate producer like
// tools/trebuchet: large enough to smooth bursts, small enough that sustained
// overload sheds load (tail-drop) instead of growing without bound.
const DefaultMaxQueue = 256

// StreamConfig tunes a writelog Stream.
type StreamConfig struct {
	// MaxQueue bounds the number of complete lines buffered awaiting the
	// drain worker. When the bound is exceeded the oldest line is discarded
	// (tail policy) so the subprocess is never back-pressured by a slow sink.
	// A zero value uses DefaultMaxQueue.
	MaxQueue int
}

type stream struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	sink     *slog.Logger
	lvl      slog.Level

	// buf holds raw bytes between newlines that have not yet been split into
	// complete lines. It is only touched by the producer goroutine (the
	// os/exec copy for this stream) and by Close, which runs after the
	// producer has stopped, so it needs no separate lock.
	buf *bytes.Buffer

	// ring is the bounded async queue of complete lines awaiting the drain
	// worker. head/count implement a FIFO ring buffer of capacity maxQ.
	ring   []string
	maxQ   int
	head   int
	count  int
	closed bool

	// wg tracks the drain worker goroutine so Close can wait for the queue
	// to empty before returning.
	wg sync.WaitGroup
}

// pushAll enqueues a batch of complete lines using the tail-drop policy: when
// the ring is full, the oldest line is discarded so the producer is never
// blocked. The whole batch is enqueued under a single lock hold and a single
// wakeup instead of one lock + Signal per line.
func (s *stream) pushAll(lines []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		// Late writes after Close: drop to avoid logging on a torn-down
		// stream. In practice Close only runs after the subprocess exits.
		return
	}

	for _, line := range lines {
		if s.count == s.maxQ {
			// Drop the oldest line (tail policy).
			s.head = (s.head + 1) % s.maxQ
			s.count--
		}
		tail := (s.head + s.count) % s.maxQ
		s.ring[tail] = line
		s.count++
	}
	s.notEmpty.Signal()
}

// pop removes and returns the oldest line. Caller holds s.mu and must check
// count first. The slot is zeroed to avoid retaining the string.
func (s *stream) pop() string {
	line := s.ring[s.head]
	s.ring[s.head] = ""
	s.head = (s.head + 1) % s.maxQ
	s.count--
	return line
}

// drainBatch is the max number of lines the drain worker pops from the ring
// per lock hold, so a burst is consumed with amortized locking over the sink.
const drainBatch = 32

func (s *stream) drain() {
	defer s.wg.Done()
	batch := make([]string, 0, drainBatch)
	for {
		s.mu.Lock()
		for s.count == 0 && !s.closed {
			s.notEmpty.Wait()
		}
		if s.count == 0 && s.closed {
			s.mu.Unlock()
			return
		}
		batch = batch[:0]
		n := s.count
		if n > drainBatch {
			n = drainBatch
		}
		for i := 0; i < n; i++ {
			batch = append(batch, s.pop())
		}
		s.mu.Unlock()

		// Log outside the lock so a slow sink does not block dequeueing.
		for _, l := range batch {
			s.sink.LogAttrs(context.Background(), s.lvl, l)
		}
		clear(batch)
	}
}

func (s *stream) Write(p []byte) (int, error) {
	// Split incoming bytes into complete lines, enqueue the batch, and return
	// promptly. The producer (os/exec copy) is never blocked by a slow sink:
	// when the ring is full the oldest line is tail-dropped (see pushAll).
	//
	// We scan the buffer with IndexByte and copy each line straight into an
	// immutable string. Unlike bytes.Buffer.ReadBytes, this avoids a redundant
	// intermediate slice copy per line (ReadBytes returns a fresh slice that is
	// then converted to a string), cutting the hot path to one allocation per
	// line instead of two.
	s.mu.Lock()
	_, _ = s.buf.Write(p) // bytes.Buffer.Write never errors
	var lines []string
	b := s.buf.Bytes()
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			// No newline: the remainder is the partial tail, left in s.buf for
			// the next write or for Close to flush.
			break
		}
		lines = append(lines, string(b[:i+1]))
		s.buf.Next(i + 1) // consume the complete line
		b = s.buf.Bytes()
	}
	s.mu.Unlock()

	if len(lines) > 0 {
		s.pushAll(lines)
	}
	return len(p), nil
}

// Close flushes any remaining buffered bytes that were not terminated by a
// newline (e.g. the last line of output from a process that exits without a
// trailing newline) and waits for the drain worker to emit all queued lines.
//
// The final partial line is emitted directly (bypassing the bounded queue) so
// it is never tail-dropped. Complete lines always drain before the partial
// tail, preserving write order.
func (s *stream) Close() error {
	s.mu.Lock()
	s.closed = true
	partial := ""
	if s.buf.Len() > 0 {
		partial = s.buf.String()
		s.buf.Reset()
	}
	// Wake the drain worker in case it is parked waiting for lines.
	s.notEmpty.Broadcast()
	s.mu.Unlock()

	// Wait for the worker to drain every complete line first. This blocks
	// only on in-flight LogAttrs (i.e. while the subprocess is already dead),
	// so it never interferes with process reaping/cancellation.
	s.wg.Wait()

	// Then emit the final partial tail directly so it cannot be dropped.
	if partial != "" {
		s.sink.LogAttrs(context.Background(), s.lvl, partial)
	}
	return nil
}

// Stream returns an io.WriteCloser that forwards each newline-delimited line
// of subprocess output to sink as a structured log record tagged with tag.
//
// Writes are decoupled from the sink: each complete line is enqueued onto a
// bounded async queue drained by a dedicated worker. When the queue is full
// the oldest line is tail-dropped, so a slow terminal never back-pressures the
// subprocess. The caller should Close() the writer after the subprocess exits
// to flush any final partial line and wait for queued output to drain.
func Stream(sink *slog.Logger, tag string, lvl slog.Level, cfg StreamConfig) io.WriteCloser {
	maxQ := cfg.MaxQueue
	if maxQ <= 0 {
		maxQ = DefaultMaxQueue
	}

	s := &stream{
		buf:  bytes.NewBuffer(make([]byte, 0, 256)),
		sink: TaggedSink(sink, tag),
		lvl:  lvl,
		ring: make([]string, maxQ),
		maxQ: maxQ,
	}
	s.notEmpty = sync.NewCond(&s.mu)
	s.wg.Add(1)
	go s.drain()
	return s
}

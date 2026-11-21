package regfront

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// A body moves in reads and writes of 32 KB, some 33 thousand per GB.
// Resetting a timer and a write deadline on each cost as much again in
// timer calls; the stall bounds below touch neither on most of them.

// stallStep is how often a write deadline is pushed while a body moves: a
// stalled client is cut within stall+stallStep, one that moves never.
func stallStep(stall time.Duration) time.Duration { return min(time.Second, stall/10) }

// stallReader calls stop when its body moves no byte for stall. stop ends
// the registry request, which fails the read under way.
//
// A read only notes the time; the timer, which fires once per stall while
// the body moves, checks it and waits the rest of stall again.
type stallReader struct {
	io.ReadCloser
	stall time.Duration
	stop  func()
	start time.Time    // the clock moved counts from (monotonic)
	moved atomic.Int64 // when a read last began or ended, since start

	mu     sync.Mutex // orders check against the constructor and Close
	timer  *time.Timer
	closed bool
}

func newStallReader(body io.ReadCloser, stall time.Duration, stop func()) io.ReadCloser {
	r := &stallReader{ReadCloser: body, stall: stall, stop: stop, start: time.Now()}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timer = time.AfterFunc(stall, r.check)
	return r
}

// Read notes the time before and after the read: a read that waits counts
// from its start, and the time the proxy then spends writing to the client
// from its end (the client's own bound is stallWriter's).
func (r *stallReader) Read(p []byte) (int, error) {
	r.moved.Store(int64(time.Since(r.start)))
	n, err := r.ReadCloser.Read(p)
	r.moved.Store(int64(time.Since(r.start)))
	return n, err
}

func (r *stallReader) check() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	idle := time.Since(r.start) - time.Duration(r.moved.Load())
	if idle < r.stall {
		r.timer.Reset(r.stall - idle)
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	r.stop()
}

func (r *stallReader) Close() error {
	r.mu.Lock()
	r.closed = true
	r.timer.Stop()
	r.mu.Unlock()
	return r.ReadCloser.Close()
}

// stallWriter gives each write to the client stall to complete, so a client
// that stops reading is dropped while one that keeps reading is never cut.
//
// The deadline is pushed to stall+step ahead at most once per step: a
// write that starts before the next push still has stall to complete.
type stallWriter struct {
	http.ResponseWriter
	rc     *http.ResponseController
	stall  time.Duration
	step   time.Duration
	pushed time.Time // when the deadline was last pushed; zero is never
}

func newStallWriter(w http.ResponseWriter, stall time.Duration) http.ResponseWriter {
	if stall <= 0 {
		return w
	}
	rc := http.NewResponseController(w)
	// net/http keeps a write deadline across requests on a connection: one
	// left from the previous request may have passed already.
	_ = rc.SetWriteDeadline(time.Time{})
	return &stallWriter{ResponseWriter: w, rc: rc, stall: stall, step: stallStep(stall)}
}

func (w *stallWriter) push() {
	now := time.Now()
	if now.Sub(w.pushed) < w.step {
		return
	}
	w.pushed = now
	_ = w.rc.SetWriteDeadline(now.Add(w.stall + w.step))
}

func (w *stallWriter) Write(p []byte) (int, error) {
	w.push()
	return w.ResponseWriter.Write(p)
}

// FlushError is what http.ResponseController.Flush calls: the flush that
// sends buffered bytes is a write too.
func (w *stallWriter) FlushError() error {
	w.push()
	return w.rc.Flush()
}

// Unwrap lets http.ResponseController reach the connection's writer.
func (w *stallWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

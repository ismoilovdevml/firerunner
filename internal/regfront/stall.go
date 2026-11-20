package regfront

import (
	"io"
	"net/http"
	"time"
)

// stallReader calls stop when its body moves no byte for stall. stop ends
// the registry request, which fails the read under way.
type stallReader struct {
	io.ReadCloser
	timer *time.Timer
	stall time.Duration
}

func newStallReader(body io.ReadCloser, stall time.Duration, stop func()) io.ReadCloser {
	return &stallReader{ReadCloser: body, timer: time.AfterFunc(stall, stop), stall: stall}
}

func (r *stallReader) Read(p []byte) (int, error) {
	r.timer.Reset(r.stall)
	n, err := r.ReadCloser.Read(p)
	r.timer.Reset(r.stall)
	return n, err
}

func (r *stallReader) Close() error {
	r.timer.Stop()
	return r.ReadCloser.Close()
}

// stallWriter gives each write to the client stall to complete, so a client
// that stops reading is dropped while one that keeps reading is never cut.
type stallWriter struct {
	http.ResponseWriter
	rc    *http.ResponseController
	stall time.Duration
}

func newStallWriter(w http.ResponseWriter, stall time.Duration) http.ResponseWriter {
	if stall <= 0 {
		return w
	}
	rc := http.NewResponseController(w)
	// net/http keeps a write deadline across requests on a connection: one
	// left from the previous request may have passed already.
	_ = rc.SetWriteDeadline(time.Time{})
	return &stallWriter{ResponseWriter: w, rc: rc, stall: stall}
}

func (w *stallWriter) Write(p []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.stall))
	return w.ResponseWriter.Write(p)
}

// FlushError is what http.ResponseController.Flush calls: the flush that
// sends buffered bytes is a write too.
func (w *stallWriter) FlushError() error {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.stall))
	return w.rc.Flush()
}

// Unwrap lets http.ResponseController reach the connection's writer.
func (w *stallWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

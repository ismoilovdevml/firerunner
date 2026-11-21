package regfront_test

import (
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/ismoilovdevml/firerunner/internal/regfront"
)

// BenchmarkBlobThroughput streams a 256 MB blob from a registry through the
// front to a client that reads as fast as it can: the cost the front adds
// per byte of a layer pull (MB/s is the blob's bytes over the time per op).
// "no-stall-bound" runs without the stall bound, the floor its cost is
// measured against.
func BenchmarkBlobThroughput(b *testing.B) {
	for _, c := range []struct {
		name  string
		stall bool
	}{{"stall-bound", true}, {"no-stall-bound", false}} {
		b.Run(c.name, func(b *testing.B) {
			lim := testLimits()
			lim.Stall = 0
			if c.stall {
				lim.Stall = regfront.DefaultLimits().Stall
			}
			benchmarkBlob(b, lim)
		})
	}
}

func benchmarkBlob(b *testing.B, lim regfront.Limits) {
	const size = 256 << 20
	chunk := pattern(1 << 20)
	addr, _ := startFront(b, startUpstream(b, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		for sent := 0; sent < size; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}), lim)
	c := client(b)
	b.SetBytes(size)
	b.ReportAllocs()
	for b.Loop() {
		resp, err := c.Get("http://" + addr + blobPath)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil || n != size {
			b.Fatalf("got %d bytes (%v), want %d", n, err, size)
		}
	}
}

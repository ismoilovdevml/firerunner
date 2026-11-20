package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The help of firerunner_flintlock_errors_total names every RPC its op label
// takes, so a reader of /metrics knows the values to expect.
func TestFlintlockErrorsHelpNamesEveryOp(t *testing.T) {
	d, srv := newTestDaemon(t)
	ctx := context.Background()
	srv.FailList(status.Error(codes.Unavailable, "down"))
	srv.FailDelete(status.Error(codes.Unavailable, "down"))
	_, _ = d.fl.List(ctx)
	_ = d.fl.Delete(ctx, "u1")
	_, _ = d.fl.Get(ctx, "no-such-uid") // boots ask for a VM's tap this way

	descs := make(chan *prometheus.Desc, 1)
	d.metrics.flintlockErrors.Describe(descs)
	help := (<-descs).String()

	ch := make(chan prometheus.Metric, 100)
	d.metrics.flintlockErrors.Collect(ch)
	close(ch)
	ops := map[string]bool{}
	for m := range ch {
		var out dto.Metric
		if err := m.Write(&out); err != nil {
			t.Fatal(err)
		}
		for _, l := range out.GetLabel() {
			if l.GetName() == "op" {
				ops[l.GetValue()] = true
			}
		}
	}
	if len(ops) != 3 {
		t.Fatalf("recorded ops %v, want the three failed calls", ops)
	}
	for op := range ops {
		if !strings.Contains(help, op) {
			t.Errorf("op %s is recorded but the help does not name it: %s", op, help)
		}
	}
}

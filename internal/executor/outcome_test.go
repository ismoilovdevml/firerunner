package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

func TestStageOutcome(t *testing.T) {
	for _, c := range []struct {
		stage          string
		code           int
		lost           bool
		err            error
		result, reason string
	}{
		{"build_script", 0, false, nil, daemon.ResultSuccess, ""},
		{"step_script", 1, false, nil, daemon.ResultScriptFailure, ""},
		{"build_script", 2, false, nil, daemon.ResultScriptFailure, ""},
		{"after_script", 1, false, nil, daemon.ResultSuccess, ""}, // GitLab ignores it
		{"build_script", 255, true, nil, daemon.ResultSystemFailure, "ssh_lost"},
		{"step_script", 255, false, nil, daemon.ResultScriptFailure, ""}, // the script's own 255
		{"after_script", 255, false, nil, daemon.ResultSuccess, ""},
		{"get_sources", 255, true, nil, daemon.ResultSystemFailure, "ssh_lost"},
		{"get_sources", 128, false, nil, daemon.ResultSystemFailure, "helper_stage"},
		{"upload_artifacts_on_success", 1, false, nil, daemon.ResultSystemFailure, "helper_stage"},
		{"build_script", -1, false, errors.New("exec: ssh not found"), daemon.ResultSystemFailure, "other"},
	} {
		result, reason := stageOutcome(c.stage, c.code, c.lost, c.err)
		if result != c.result || reason != c.reason {
			t.Errorf("stageOutcome(%s, %d, lost %v, %v) = %s/%s, want %s/%s", c.stage, c.code, c.lost, c.err, result, reason, c.result, c.reason)
		}
	}
}

func TestPrepareReason(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "canceled"},
		{fmt.Errorf("waiting: %w", context.DeadlineExceeded), "canceled"},
		{fmt.Errorf("%w for another microVM within 3m0s", errNoMemory), "admission_timeout"},
		{fmt.Errorf("creating microVM job-1: %w", status.Error(codes.Unavailable, "connection refused")), "flintlock_error"},
		{status.Error(codes.Unknown, "failed reading from content store"), "flintlock_error"},
		{errors.New("something else"), "other"},
	} {
		if got := prepareReason(c.err); got != c.want {
			t.Errorf("prepareReason(%v) = %s, want %s", c.err, got, c.want)
		}
	}
	// vm.Boot's timeouts are recognisable without matching message text.
	if got := prepareReason(fmt.Errorf("boot: %w", vm.ErrNotReady)); got != "vm_boot" {
		t.Errorf("prepareReason(ErrNotReady) = %s, want vm_boot", got)
	}
}

func TestFirstFailureWins(t *testing.T) {
	st := &vm.JobState{}
	recordFailure(st, daemon.ResultSuccess, "")
	recordFailure(st, daemon.ResultSystemFailure, "ssh_lost")
	recordFailure(st, daemon.ResultScriptFailure, "")
	if r, why := finishResult(st); r != daemon.ResultSystemFailure || why != "ssh_lost" {
		t.Fatalf("finishResult = %s/%s, want system_failure/ssh_lost", r, why)
	}
	st = &vm.JobState{}
	recordFailure(st, daemon.ResultScriptFailure, "")
	if !st.Failed {
		t.Fatal("a script failure must also set Failed for older readers")
	}
}

// State files written by the previous executor only have "failed".
func TestFinishResultOfOlderStateFiles(t *testing.T) {
	var st vm.JobState
	if err := json.Unmarshal([]byte(`{"id":"job-1","uid":"u","failed":true}`), &st); err != nil {
		t.Fatal(err)
	}
	if r, _ := finishResult(&st); r != daemon.ResultScriptFailure {
		t.Fatalf("finishResult(old failed state) = %s", r)
	}
	if r, _ := finishResult(&vm.JobState{}); r != daemon.ResultSuccess {
		t.Fatalf("finishResult(empty) = %s", r)
	}
}

func TestErrTextIsOneBoundedLine(t *testing.T) {
	got := errText(errors.New("line one\nline two\t" + strings.Repeat("x", 600)))
	if strings.ContainsAny(got, "\n\t") || len(got) > 512 {
		t.Fatalf("errText = %q (%d bytes)", got[:40], len(got))
	}
	if errText(nil) != "" {
		t.Fatal("errText(nil) must be empty")
	}
}

// fakeDaemon records the events the executor sends over the daemon socket.
func fakeDaemon(t *testing.T, socket string) func() []daemon.Event {
	t.Helper()
	var mu sync.Mutex
	var events []daemon.Event
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e daemon.Event
		if r.URL.Path == "/event" && json.NewDecoder(r.Body).Decode(&e) == nil {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return func() []daemon.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]daemon.Event(nil), events...)
	}
}

// A job whose VM died mid-stage is a FireRunner failure, not the project's,
// even though GitLab shows only "system failure".
func TestCleanupReportsTheRecordedFailure(t *testing.T) {
	cfg, _ := cleanupFixture(t)
	events := withEvents(t, &cfg)
	st, err := vm.LoadJobState(statePath("job-555"))
	if err != nil {
		t.Fatal(err)
	}
	recordFailure(st, daemon.ResultSystemFailure, "ssh_lost")
	if err := vm.SaveJobState(statePath("job-555"), st); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(cfg); err != nil {
		t.Fatal(err)
	}
	got := events()
	if len(got) != 1 {
		t.Fatalf("events %+v, want one finish event", got)
	}
	e := got[0]
	if e.Kind != "finish" || e.Result != daemon.ResultSystemFailure || e.Reason != "ssh_lost" ||
		e.Job != "555" || e.Project != "1" || e.VM != "job-555" {
		t.Fatalf("finish event %+v", e)
	}
	if e.Err != "" {
		t.Fatalf("finish event of a job whose VM was deleted carries an error: %q", e.Err)
	}
}

// withEvents gives the cleanup fixture a fake daemon and returns what it received.
func withEvents(t *testing.T, cfg *config.Config) func() []daemon.Event {
	t.Helper()
	dir, err := os.MkdirTemp("", "fre") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg.Daemon.Socket = filepath.Join(dir, "d.sock")
	return fakeDaemon(t, cfg.Daemon.Socket)
}

// The daemon's log is where an operator looks for a job's leaked VM: the
// finish event says why its delete failed.
func TestCleanupReportsAFailedDelete(t *testing.T) {
	cfg, srv := cleanupFixture(t)
	events := withEvents(t, &cfg)
	down := status.Error(codes.Unavailable, "flintlockd is down")
	srv.FailDelete(down, down, down)
	if err := Cleanup(cfg); err == nil {
		t.Fatal("Cleanup succeeded although every delete failed")
	}
	got := events()
	if len(got) != 1 || got[0].Kind != "finish" {
		t.Fatalf("events %+v, want one finish event", got)
	}
	if !strings.Contains(got[0].Err, "flintlockd is down") {
		t.Fatalf("finish event Err = %q, want the delete error", got[0].Err)
	}
}

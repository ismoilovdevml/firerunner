package executor

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/daemon"
	"github.com/ismoilovdevml/firerunner/internal/vm"
)

// errNoMemory starts the error of a job that waited vm.boot_timeout for host memory.
var errNoMemory = errors.New("no host memory")

// stageOutcome classifies a finished stage for the daemon's job metrics: did
// the job's own commands fail, or did FireRunner (or the host) fail the job?
// lost is an exit code 255 that came from ssh itself, not from the script.
func stageOutcome(stage string, code int, lost bool, err error) (result, reason string) {
	switch {
	case err != nil:
		return daemon.ResultSystemFailure, "other" // ssh could not even start
	case lost:
		return daemon.ResultSystemFailure, "ssh_lost" // the VM is gone or unreachable
	case code == 0:
		return daemon.ResultSuccess, ""
	case stage == "after_script":
		// GitLab ignores after_script's exit code; so do the metrics.
		return daemon.ResultSuccess, ""
	case isUserStage(stage):
		return daemon.ResultScriptFailure, ""
	default:
		return daemon.ResultSystemFailure, "helper_stage"
	}
}

// prepareReason says why prepare could not get a microVM.
func prepareReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, errNoMemory), errors.Is(err, vm.ErrAdmissionBusy):
		// No room, or other admissions held the lock, until the deadline:
		// the host is busy admitting microVMs, flintlock did not fail.
		return "admission_timeout"
	case errors.Is(err, vm.ErrNotReady):
		return "vm_boot"
	}
	if _, ok := status.FromError(err); ok {
		return "flintlock_error"
	}
	return "other"
}

// recordFailure keeps the first failure of the job in its state file; cleanup
// reports it.
func recordFailure(st *vm.JobState, result, reason string) {
	if result == daemon.ResultSuccess || st.Result != "" {
		return
	}
	st.Result, st.Reason = result, reason
	if result == daemon.ResultScriptFailure {
		st.Failed = true
	}
}

// finishResult is the job's result as cleanup reports it. State files written
// before Result existed only know Failed.
func finishResult(st *vm.JobState) (result, reason string) {
	switch {
	case st.Result != "":
		return st.Result, st.Reason
	case st.Failed:
		return daemon.ResultScriptFailure, ""
	}
	return daemon.ResultSuccess, ""
}

// errText is an error for the daemon's log: one line, bounded.
func errText(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// jobNumber is the GitLab job id of a microVM id "job-<n>".
func jobNumber(id string) string { return strings.TrimPrefix(id, "job-") }

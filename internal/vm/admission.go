package vm

import (
	"context"
	"fmt"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock"
	"github.com/ismoilovdevml/firerunner/internal/host"
)

// overheadPct is Firecracker's per-VM memory overhead on top of the guest RAM.
const overheadPct = 5

// memTotalMB is a variable only so tests can pin host memory; production
// never reassigns it.
var memTotalMB = host.MemTotalMB

// Capacity is the host memory microVMs may commit: MemTotal minus the reserve.
func Capacity(cfg config.Config) (int, error) {
	total, err := memTotalMB()
	if err != nil {
		return 0, err
	}
	return total - cfg.VM.HostReserveMB, nil
}

// CommittedMB sums the guest memory promised to existing microVMs.
// Guest RAM is allocated lazily, so free memory right now says nothing about
// what running VMs will use later; committed memory does.
func CommittedMB(ctx context.Context, fl *flintlock.Client) (int, error) {
	vms, err := fl.List(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, v := range vms {
		switch v.GetStatus().GetState() {
		case types.MicroVMStatus_FAILED, types.MicroVMStatus_DELETING:
			continue
		}
		total += withOverhead(int(v.GetSpec().GetMemoryInMb()))
	}
	return total, nil
}

// Fits reports whether one more microVM of the configured size fits, given
// extraMB already promised but not yet visible in flintlock.
func Fits(ctx context.Context, cfg config.Config, fl *flintlock.Client, extraMB int) (bool, string, error) {
	capacity, err := Capacity(cfg)
	if err != nil {
		return true, "", err // cannot tell: do not block
	}
	committed, err := CommittedMB(ctx, fl)
	if err != nil {
		return false, "", err
	}
	need := withOverhead(cfg.VM.MemoryMB)
	msg := fmt.Sprintf("committed %d MB + new %d MB, capacity %d MB", committed+extraMB, need, capacity)
	return committed+extraMB+need <= capacity, msg, nil
}

func withOverhead(mb int) int { return mb * (100 + overheadPct) / 100 }

package host

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// prSetMemoryMerge is PR_SET_MEMORY_MERGE (Linux 6.4, backported to RHEL 9.4+).
const prSetMemoryMerge = 67

// ExecWithKSM marks this process for kernel samepage merging and replaces it
// with argv. The mark is inherited by children, so every Firecracker VM that
// flintlockd starts shares identical guest pages (same kernel, rootfs and
// images) with the others. Without kernel support it just runs argv.
func ExecWithKSM(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("usage: firerunner ksm-exec <command> [args...]")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetMemoryMerge, 1, 0, 0, 0, 0); errno != 0 {
		fmt.Fprintf(os.Stderr, "firerunner: kernel samepage merging not available (%v); starting without it\n", errno)
	}
	return syscall.Exec(path, argv, os.Environ())
}

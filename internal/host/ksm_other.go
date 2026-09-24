//go:build !linux

package host

import "errors"

// ExecWithKSM is Linux only.
func ExecWithKSM([]string) error { return errors.New("ksm-exec is only supported on Linux") }

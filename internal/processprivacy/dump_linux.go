//go:build linux

package processprivacy

import (
	"fmt"
	"syscall"
)

const (
	prGetDumpable = 3
	prSetDumpable = 4
)

func Dumpability() (uintptr, error) {
	value, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prGetDumpable, 0, 0, 0, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return value, nil
}

func DisableDumpability() error {
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetDumpable, 0, 0, 0, 0, 0); errno != 0 {
		return errno
	}
	value, err := Dumpability()
	if err != nil {
		return err
	}
	if value != 0 {
		return fmt.Errorf("PR_GET_DUMPABLE returned %d after disable", value)
	}
	return nil
}

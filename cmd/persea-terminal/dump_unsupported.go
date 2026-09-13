//go:build !linux

package main

import "fmt"

func disableProcessDumpability() error {
	return fmt.Errorf("unified terminal development mode requires Linux PR_SET_DUMPABLE")
}

//go:build !linux

package processprivacy

import "fmt"

func DisableDumpability() error {
	return fmt.Errorf("unified terminal development mode requires Linux PR_SET_DUMPABLE")
}

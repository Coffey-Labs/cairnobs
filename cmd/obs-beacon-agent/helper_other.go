//go:build !linux

package main

import "errors"

// runHelper: the root helper is Linux's. On Windows the agent reads what
// it needs itself (docs/windows.md).
func runHelper([]string) error {
	return errors.New("there's no helper on this platform: the agent reads what it needs itself")
}

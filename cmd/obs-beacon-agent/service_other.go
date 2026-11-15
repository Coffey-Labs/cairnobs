//go:build !windows

package main

import "errors"

// On Linux the package installs the systemd units; there's no service
// command and the agent is never a Windows service.
func runService([]string) error {
	return errors.New("only on Windows: on Linux the package installs the systemd units")
}

func serviceLogging() bool           { return false }
func runAsService(cfg *config) error { return nil }

// prepareJoin: on Linux the package made the folders.
func prepareJoin(cfgPath, stateDir string) error { return nil }

//go:build !windows

package main

import "github.com/cairnobs/cairnobs/internal/report"

// readHyperV: Hyper-V is Windows'.
func (a *agent) readHyperV() *report.VMs { return nil }

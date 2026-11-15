//go:build !windows

package main

import (
	"context"

	"github.com/cairnobs/cairnobs/internal/report"
)

const (
	defaultConfig   = "/etc/obs-beacon/agent.yaml"
	defaultStateDir = "/var/lib/obs-beacon-agent"
	examplesDir     = "/usr/share/doc/obs-beacon-agent/examples"
	hasHelper       = true // the root helper: containers, the map sample, libvirt
)

// localMap: on Linux the map sample needs root, so the helper takes it.
var localMap func(context.Context) (*report.Map, error)

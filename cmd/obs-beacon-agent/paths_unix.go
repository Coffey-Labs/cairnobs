//go:build !windows

package main

const (
	defaultConfig   = "/etc/obs-beacon/agent.yaml"
	defaultStateDir = "/var/lib/obs-beacon-agent"
	examplesDir     = "/usr/share/doc/obs-beacon-agent/examples"
	hasHelper       = true // the root helper: containers, the map sample, libvirt
)

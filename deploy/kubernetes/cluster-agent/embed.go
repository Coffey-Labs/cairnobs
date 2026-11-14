// Package clusteragent holds the cluster agent's manifest, so obs-beacond
// can hand out the file that's in the repo (docs/cluster-watcher.md,
// Security).
package clusteragent

import _ "embed"

// Manifest is cluster-agent.yaml: @IMAGE@ is the image to fill in, and
// the line #@SECRET@ is where the join token's Secret goes, before the
// Deployment that reads it.
//
//go:embed cluster-agent.yaml
var Manifest string

// CertRenew is cert-renew.yaml: the optional role that lets the agent ask
// cert-manager to renew a Certificate (docs/cert-rotation.md, step 3).
//
//go:embed cert-renew.yaml
var CertRenew string

// Command is the agent's command line in Manifest; CertRenewCommand is it
// with -cert-renew.
const (
	Command          = "command: [/usr/bin/obs-beacon-agent, cluster]"
	CertRenewCommand = "command: [/usr/bin/obs-beacon-agent, cluster, -cert-renew]"
)

// Names in the manifest that obs-beacond and the agent share.
const (
	Namespace = "cairn-obs-agent"
	Secret    = "obs-beacon-agent" // the join token, then the key and certificate

	SecretMarker = "#@SECRET@"
)

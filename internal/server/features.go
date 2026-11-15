package server

// What's set up, for the web interface: the monitoring side shows only
// configured pieces, and the Configure area (the control plane) says where
// each one is set and how to set up the rest.

import (
	"net/http"
	"time"
)

// features says which monitoring pages have something to show.
func (srv *Server) features() map[string]bool {
	containers, mapped := false, false
	served := false
	for _, h := range srv.store.Snapshot() {
		containers = containers || h.last.Containers != nil
		mapped = mapped || h.mp != nil
		served = served || h.certs != nil && len(h.certs.List) > 0 || h.files != nil && len(h.files.List) > 0
	}
	checks := false
	if c := srv.currentChecks(); c != nil && len(c.Checks) > 0 {
		checks = true
	}
	return map[string]bool{
		"maps":       srv.cfg.Topology != "" || mapped, // drawn from map samples, or by hand
		"mail":       srv.cfg.Mail != nil,
		"data":       srv.cfg.DataLayer != nil,
		"containers": containers || len(srv.currentExpected().Hosts) > 0,
		"backups":    srv.cfg.Backups != "",
		"cloudflare": srv.cfg.Cloudflare != nil,
		"akamai":     srv.cfg.Akamai != nil,
		"checks":     checks,
		"reports":    srv.db != nil,
		"workloads":  len(srv.clusterAgents()) > 0 || len(srv.cfg.Vault) > 0,
		// The inventory (docs/cert-inventory.md): hosts' ports and files,
		// Vault, site checks, Cloudflare and clusters.
		"certificates": served || len(srv.cfg.Vault) > 0 || checks || srv.cfg.Cloudflare != nil || srv.cfg.Akamai != nil || len(srv.clusterAgents()) > 0,
	}
}

// handleSettingsSummary is the Configure overview: each area, where it's
// set ("file", "ui", or "" for nowhere to keep it), and how much is in it.
func (srv *Server) handleSettingsSummary(w http.ResponseWriter, r *http.Request) {
	nChecks := 0
	if c := srv.currentChecks(); c != nil {
		nChecks = len(c.Checks)
	}
	exp := srv.currentExpected()
	providers := []string{}
	for _, p := range srv.cfg.Signin.Providers {
		providers = append(providers, p.Type)
	}
	file := func(set bool) string {
		if set {
			return sourceFile
		}
		return ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"database": srv.db != nil,
		"hosts":    len(srv.agentList()),
		"clusters": len(srv.clusterAgents()),
		"sections": map[string]any{
			"checks":      map[string]any{"source": srv.checksSource(), "file": srv.cfg.Checks, "count": nChecks},
			"maintenance": map[string]any{"source": srv.maintenanceSource(), "count": len(srv.windows())},
			"channels":    map[string]any{"source": srv.channelsSource(), "count": len(srv.alerts.chans())},
			"expected":    map[string]any{"source": srv.expectedSource(), "file": srv.cfg.Expected, "count": len(exp.Hosts)},
		},
		// Set in obs-beacond.yaml (or a file it names) for now; editing
		// them here comes with the add-ons.
		"integrations": map[string]any{
			"cloudflare": file(srv.cfg.Cloudflare != nil),
			"akamai":     file(srv.cfg.Akamai != nil),
			"mail":       file(srv.cfg.Mail != nil),
			"datalayer":  file(srv.cfg.DataLayer != nil),
			"backups":    file(srv.cfg.Backups != ""),
			"topology":   file(srv.cfg.Topology != ""),
			"digest":     file(srv.cfg.Reports != nil && srv.cfg.Reports.Digest != nil),
		},
		"signin": providers,
		"now":    time.Now(),
	})
}

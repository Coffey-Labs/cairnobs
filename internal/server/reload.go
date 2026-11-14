package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Reloads and renewals (docs/cert-rotation.md, steps 2 and 3). An admin
// asks a host's agent to reload a unit whose port serves an older
// certificate than its renewed file, or a cluster agent to have
// cert-manager renew a Certificate now. obs-beacond offers only what the
// inventory found and the agent says it may do (the host's cert_reload,
// the cluster agent's -cert-renew); the agent checks it all again.

// reloadArgs are what the agent gets (cmd/obs-beacon-agent/reload.go).
type reloadArgs struct {
	Unit string `json:"unit"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

func (a reloadArgs) of(r CertReload) bool {
	return a.Unit == r.Unit && a.Addr == r.Addr && a.Port == r.Port
}

// renewArgs are what a cluster agent gets (clusterwatch.RenewArgs).
type renewArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// lastActions sets each offer's last action asked for, from the newest
// actions.
func (srv *Server) lastActions(rows []*CertInv, now time.Time) {
	if srv.db == nil {
		return
	}
	offered := false
	for _, c := range rows {
		offered = offered || len(c.Reloads) > 0 || len(c.Renews) > 0
	}
	if !offered {
		return
	}
	as, err := srv.db.Actions(200)
	if err != nil {
		return
	}
	last := func(agent, kind string, match func(json.RawMessage) bool) *ActionView {
		for _, a := range as { // newest first
			if a.Kind == kind && a.Agent == agent && match(json.RawMessage(a.Args)) {
				v := actionView(a, now)
				return &v
			}
		}
		return nil
	}
	for _, c := range rows {
		for i, o := range c.Reloads {
			c.Reloads[i].Last = last(o.Host, "reload", func(raw json.RawMessage) bool {
				var args reloadArgs
				return json.Unmarshal(raw, &args) == nil && args.of(o)
			})
		}
		for i, o := range c.Renews {
			c.Renews[i].Last = last(o.Source, "renew", func(raw json.RawMessage) bool {
				var args renewArgs
				return json.Unmarshal(raw, &args) == nil && args.Namespace == o.Namespace && args.Name == o.Certificate
			})
		}
	}
}

// handleCertReload asks for a reload the inventory offers: {"host", "unit",
// "addr", "port"}.
func (srv *Server) handleCertReload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host string `json:"host"`
		reloadArgs
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Host == "" || req.Unit == "" || req.Port <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send host, unit, addr and port"})
		return
	}
	if srv.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "reloads need a database"})
		return
	}
	now := time.Now()
	var offer *CertReload
	var cert *CertInv
	for _, c := range srv.certificates(now) {
		for i, o := range c.Reloads {
			if o.Host == req.Host && req.reloadArgs.of(o) {
				offer, cert = &c.Reloads[i], c
			}
		}
	}
	if offer == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "that reload isn't on offer: the port serves the renewed certificate already, or the host's cert_reload doesn't list the unit"})
		return
	}
	srv.lastActions([]*CertInv{cert}, now)
	if l := offer.Last; l != nil && (l.State == "waiting" || l.State == "taken") {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a reload of " + offer.Unit + " on " + offer.Host + " is already " + l.State})
		return
	}
	fp := strings.ToUpper(cert.Fingerprint)
	if len(fp) > 16 {
		fp = fp[:16]
	}
	what := fmt.Sprintf("reload %s, as %s serves %s (%s) though %s is renewed", offer.Unit, offer.Where, cert.Subject, fp, offer.File)
	id, err := srv.queueAction(offer.Host, "reload", req.reloadArgs, srv.who(r), what)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "can't ask: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleCertRenew asks a cluster agent to have cert-manager renew a
// Certificate the inventory offers: {"source", "namespace", "certificate"}.
func (srv *Server) handleCertRenew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source      string `json:"source"`
		Namespace   string `json:"namespace"`
		Certificate string `json:"certificate"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Source == "" || req.Namespace == "" || req.Certificate == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "send source, namespace and certificate"})
		return
	}
	if srv.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "renewals need a database"})
		return
	}
	now := time.Now()
	var offer *CertRenew
	var cert *CertInv
	for _, c := range srv.certificates(now) {
		for i, o := range c.Renews {
			if o.Source == req.Source && o.Namespace == req.Namespace && o.Certificate == req.Certificate {
				offer, cert = &c.Renews[i], c
			}
		}
	}
	if offer == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "that renewal isn't on offer: cert-manager is renewing it already, it isn't close to its end, or the cluster agent runs without -cert-renew"})
		return
	}
	srv.lastActions([]*CertInv{cert}, now)
	if l := offer.Last; l != nil && (l.State == "waiting" || l.State == "taken") {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a renewal of " + offer.Namespace + "/" + offer.Certificate + " is already " + l.State})
		return
	}
	what := fmt.Sprintf("renew Certificate %s/%s (%s, %s) now", offer.Namespace, offer.Certificate, cert.Subject, strings.ToLower(cert.Word))
	id, err := srv.queueAction(offer.Source, "renew", renewArgs{Namespace: offer.Namespace, Name: offer.Certificate}, srv.who(r), what)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "can't ask: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

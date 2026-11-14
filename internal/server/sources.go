package server

import (
	"log"
	"regexp"
	"slices"
	"strings"
)

// A host agent can read several things beside its host (Proxmox, a
// vCenter, Prism Central, arrays: docs/pure.md, decision 1). Each is a
// source of its own, kept under report.SourceKey: "mgmt1:vcsa-01".

// sourceName is what an agent may call something it reads: a host name,
// as agents' names are, dots allowed.
var sourceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

// sourceAgent is the agent a source key belongs to.
func sourceAgent(key string) string {
	a, _, _ := strings.Cut(key, ":")
	return a
}

// forgetSources forgets what a host agent no longer reads: named sources
// of its that aren't in reads, and one under its bare name (sent before
// sources were named). reads is nil from an older agent: nothing is
// forgotten then.
func (srv *Server) forgetSources(agent string, reads []string) {
	if reads == nil || srv.clusterAgent(agent) {
		return
	}
	var gone []string
	ws := &srv.wl
	ws.mu.RLock()
	for key := range ws.src {
		a, name, named := strings.Cut(key, ":")
		if a == agent && (!named || !slices.Contains(reads, name)) {
			gone = append(gone, key)
		}
	}
	ws.mu.RUnlock()
	srv.dropSources(gone, "its agent no longer reads it")
}

// forgetAgentSources forgets everything an agent sent, named or not:
// when the agent itself is removed.
func (srv *Server) forgetAgentSources(agent string) {
	var gone []string
	ws := &srv.wl
	ws.mu.RLock()
	for key := range ws.src {
		if sourceAgent(key) == agent {
			gone = append(gone, key)
		}
	}
	ws.mu.RUnlock()
	srv.dropSources(gone, "its agent was removed")
}

func (srv *Server) dropSources(keys []string, why string) {
	if len(keys) == 0 {
		return
	}
	for _, key := range keys {
		log.Printf("workloads: forgetting %s: %s", key, why)
		if srv.db == nil {
			srv.wl.mu.Lock()
			delete(srv.wl.src, key)
			srv.wl.mu.Unlock()
			continue
		}
		if err := srv.db.DeleteWorkloadSource(key); err != nil {
			log.Printf("workloads: forgetting %s: %v", key, err)
		}
	}
	srv.syncWorkloads(true)
	srv.publish(noticeWorkloads, keys[0])
}

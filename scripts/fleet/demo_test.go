package main

import (
	"path/filepath"
	"testing"

	"github.com/cairnobs/cairnobs/internal/server"
)

// The files fleet config -demo writes load as obs-beacond loads them.
func TestDemoFilesLoad(t *testing.T) {
	const n = 120
	dir := t.TempDir()
	writeDemo(dir, n)
	var agents []server.AgentConfig
	for i := range n {
		agents = append(agents, server.AgentConfig{Name: hostName(i)})
	}
	if _, err := server.LoadChecks(filepath.Join(dir, "checks.yaml"), agents); err != nil {
		t.Fatal(err)
	}
	if _, err := server.LoadExpected(filepath.Join(dir, "expected.yaml")); err != nil {
		t.Fatal(err)
	}
	topo, err := server.LoadTopology(filepath.Join(dir, "topology.yaml"), agents)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range demoChecks(n) {
		ids[c.id] = true
	}
	for _, s := range topo.Services {
		for _, c := range s.Checks {
			if !ids[c] {
				t.Errorf("service %s names check %s, which the demo doesn't have", s.ID, c)
			}
		}
	}
}

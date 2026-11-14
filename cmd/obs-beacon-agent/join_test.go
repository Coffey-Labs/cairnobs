package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cairnobs/cairnobs/internal/jointoken"
)

func TestJoinedConfigLoads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "obs-beacon", "agent.yaml")
	tok := jointoken.Token{Server: "https://monitor.example.org:8461", CAFingerprint: "sha256:" + strings.Repeat("ab", 32), Secret: "x"}
	if err := writeJoinedConfig(p, tok, "web1", "/srv/agent-state", 0, false); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode: %v %v", st, err)
	}
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server != tok.Server || c.CAFingerprint != tok.CAFingerprint || c.Host != "web1" || c.TokenFile != "" || c.StateDir != "/srv/agent-state" {
		t.Fatalf("config: %+v", c)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), tok.Secret+"\n") {
		t.Fatal("the secret was written")
	}
}

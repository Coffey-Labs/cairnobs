package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install.ps1 comes with this server's version and package address in
// PowerShell quotes, and the agent's zip is served from packages_dir.
func TestInstallPS1(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"SHA256SUMS", "obs-beacon-agent_0.1.0_windows_amd64.zip", "obs-beacon-agent_0.1.0_windows_arm64.zip", "other.zip", "obs-beacond-linux-amd64"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := &Server{cfg: &Config{}}
	srv.cfg.Join.PackagesDir = dir
	srv.cfg.Alerts.PublicURL = "https://obs.example.org/it's"
	w := httptest.NewRecorder()
	srv.handleInstallPS1(w, httptest.NewRequest("GET", "/install.ps1", nil))
	body := w.Body.String()
	if strings.Contains(body, "@VERSION@") || strings.Contains(body, "@PACKAGES@") {
		t.Fatal("placeholders left in install.ps1")
	}
	if !strings.Contains(body, "$Packages = 'https://obs.example.org/it''s/agent-packages'") {
		t.Fatalf("packages line:\n%s", body)
	}
	if !strings.HasPrefix(body, "# Cairn OBS") || !strings.Contains(body, "param([string]$Join") {
		t.Fatal("not the script")
	}
	for f, want := range map[string]int{
		"obs-beacon-agent_0.1.0_windows_amd64.zip": 200, "obs-beacon-agent_0.1.0_windows_arm64.zip": 200, "SHA256SUMS": 200,
		"other.zip": 404, "obs-beacond-linux-amd64": 404,
	} {
		r := httptest.NewRequest("GET", "/agent-packages/"+f, nil)
		r.SetPathValue("file", f)
		w := httptest.NewRecorder()
		srv.handleAgentPackage(w, r)
		if w.Code != want {
			t.Errorf("%s: %d, want %d", f, w.Code, want)
		}
	}
}

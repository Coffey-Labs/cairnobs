//go:build linux

package netmap

import (
	"encoding/json"
	"os"
	"testing"
)

// TestSampleLive takes a real sample of this host and prints it, when
// NETMAP_LIVE is set (run it as root to see WireGuard and every owner).
func TestSampleLive(t *testing.T) {
	if os.Getenv("NETMAP_LIVE") == "" {
		t.Skip("set NETMAP_LIVE=1 to sample this host")
	}
	b, _ := json.MarshalIndent((&Sampler{}).Sample(), "", "  ")
	t.Logf("%s", b)
}

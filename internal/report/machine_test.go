package report

import "testing"

func TestSwappedUUID(t *testing.T) {
	if got := SwappedUUID("4221a2b3-c4d5-e6f7-8899-aabbccddeeff"); got != "b3a22142-d5c4-f7e6-8899-aabbccddeeff" {
		t.Errorf("swapped: %s", got)
	}
	if got := SwappedUUID(SwappedUUID("4221a2b3-c4d5-e6f7-8899-aabbccddeeff")); got != "4221a2b3-c4d5-e6f7-8899-aabbccddeeff" {
		t.Errorf("twice: %s", got)
	}
	if SwappedUUID("") != "" {
		t.Error("empty")
	}
}

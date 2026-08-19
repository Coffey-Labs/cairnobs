package sessioncheck

import "testing"

func TestRoleSatisfies(t *testing.T) {
	cases := []struct {
		role, min string
		want      bool
	}{
		{"viewer", "editor", false},
		{"editor", "editor", true},
		{"admin", "editor", true},
		{"owner", "editor", true},
		{"", "editor", false}, // unknown/empty role never satisfies a real floor
	}
	for _, c := range cases {
		if got := roleSatisfies(c.role, c.min); got != c.want {
			t.Errorf("roleSatisfies(%q, %q) = %v, want %v", c.role, c.min, got, c.want)
		}
	}
}

func TestIsReadOnly(t *testing.T) {
	if !isReadOnly("GET") || !isReadOnly("HEAD") {
		t.Error("GET/HEAD should be read-only")
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if isReadOnly(m) {
			t.Errorf("%s should not be read-only", m)
		}
	}
}

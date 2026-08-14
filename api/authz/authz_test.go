package authz

import "testing"

func TestRoleSatisfies(t *testing.T) {
	tests := []struct {
		have, want Role
		satisfies  bool
	}{
		{RoleViewer, RoleViewer, true},
		{RoleEditor, RoleViewer, true},
		{RoleAdmin, RoleViewer, true},
		{RoleOwner, RoleViewer, true},
		{RoleViewer, RoleEditor, false},
		{RoleViewer, RoleAdmin, false},
		{RoleEditor, RoleAdmin, false},
		{RoleAdmin, RoleOwner, false},
		{RoleOwner, RoleOwner, true},
		// RoleService is a separate lane, not on the human rank scale --
		// per /docs/phase-4-isolation-design.md's alerting service
		// identity, it must never satisfy a human role requirement no
		// matter how "high" that might look on paper, and a human role
		// (even Owner) must never satisfy a RoleService requirement.
		{RoleService, RoleViewer, false},
		{RoleService, RoleOwner, false},
		{RoleOwner, RoleService, false},
		{RoleViewer, RoleService, false},
		{RoleService, RoleService, true},
	}
	for _, tt := range tests {
		got := tt.have.Satisfies(tt.want)
		if got != tt.satisfies {
			t.Errorf("Role(%q).Satisfies(%q) = %v, want %v", tt.have, tt.want, got, tt.satisfies)
		}
	}
}

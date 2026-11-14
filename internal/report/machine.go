package report

import (
	"regexp"
	"strings"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// placeholderUUIDs are what firmware says when nobody set a UUID: shared
// by every machine of a model, so they link nothing.
var placeholderUUIDs = map[string]bool{
	"00000000-0000-0000-0000-000000000000": true,
	"ffffffff-ffff-ffff-ffff-ffffffffffff": true,
	"03000200-0400-0500-0006-000700080009": true, // "To Be Filled By O.E.M."
	"00020003-0004-0005-0006-000700080009": true,
}

// MachineUUID is an SMBIOS or firmware UUID in one form, lower case, or ""
// when it isn't one or is a placeholder.
func MachineUUID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !uuidRE.MatchString(s) || placeholderUUIDs[s] {
		return ""
	}
	return s
}

// SwappedUUID is u with its first three fields' bytes reversed: what a
// guest on older VMware virtual hardware reads as its SMBIOS UUID for a
// vCenter BIOS UUID of u (docs/cluster-watcher.md, step 8). u is a
// MachineUUID.
func SwappedUUID(u string) string {
	if len(u) != 36 {
		return ""
	}
	rev := func(s string) string {
		b := []byte(s)
		for i, j := 0, len(b)-2; i < j; i, j = i+2, j-2 {
			b[i], b[i+1], b[j], b[j+1] = b[j], b[j+1], b[i], b[i+1]
		}
		return string(b)
	}
	return rev(u[0:8]) + "-" + rev(u[9:13]) + "-" + rev(u[14:18]) + u[18:]
}

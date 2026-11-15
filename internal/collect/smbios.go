package collect

import (
	"encoding/binary"
	"fmt"
)

// SMBIOSUUID finds the system UUID in RawSMBIOSData (an 8-byte header,
// then the structure table). The first three fields are little-endian
// since SMBIOS 2.6, as Linux prints them.
func SMBIOSUUID(raw []byte) (string, bool) {
	if len(raw) < 8 {
		return "", false
	}
	length := int(binary.LittleEndian.Uint32(raw[4:8]))
	t := raw[8:]
	if length < len(t) {
		t = t[:length]
	}
	for len(t) >= 4 {
		typ, size := t[0], int(t[1])
		if size < 4 || size > len(t) {
			return "", false
		}
		if typ == 1 && size >= 0x18 {
			u := t[8:24]
			allSame := true
			for _, b := range u[1:] {
				if b != u[0] {
					allSame = false
				}
			}
			if allSame && (u[0] == 0 || u[0] == 0xff) {
				return "", false // "not set" or "not present"
			}
			return fmt.Sprintf("%08x-%04x-%04x-%x-%x",
				binary.LittleEndian.Uint32(u[0:4]), binary.LittleEndian.Uint16(u[4:6]),
				binary.LittleEndian.Uint16(u[6:8]), u[8:10], u[10:16]), true
		}
		if typ == 127 {
			break
		}
		// the strings after the formatted part end in a double NUL
		i := size
		for i+1 < len(t) && (t[i] != 0 || t[i+1] != 0) {
			i++
		}
		if i+2 > len(t) {
			break
		}
		t = t[i+2:]
	}
	return "", false
}

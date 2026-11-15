package collect

import "testing"

func TestSMBIOSUUID(t *testing.T) {
	// a type 0 (BIOS) structure with one string, then type 1 (system)
	bios := []byte{0, 0x12, 0, 0, 1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 'S', 'e', 'a', 'B', 'I', 'O', 'S', 0, 0}
	sys := []byte{1, 0x1b, 1, 0, 1, 2, 3, 4,
		0x33, 0x22, 0x11, 0x00, 0x55, 0x44, 0x77, 0x66, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff,
		6, 0, 0, 0}
	sys = append(sys, 'Q', 'E', 'M', 'U', 0, 0)
	end := []byte{127, 4, 2, 0, 0, 0}
	table := append(append(append([]byte{}, bios...), sys...), end...)
	raw := append([]byte{0, 3, 0, 0, byte(len(table)), 0, 0, 0}, table...)
	got, ok := SMBIOSUUID(raw)
	if want := "00112233-4455-6677-8899-aabbccddeeff"; !ok || got != want {
		t.Fatalf("got %q %v, want %q", got, ok, want)
	}
	unset := append([]byte{}, raw...)
	for i := 8 + len(bios) + 8; i < 8+len(bios)+24; i++ {
		unset[i] = 0xff
	}
	if got, ok := SMBIOSUUID(unset); ok {
		t.Fatalf("an unset UUID gave %q", got)
	}
	if _, ok := SMBIOSUUID(raw[:10]); ok {
		t.Fatal("a cut-off table gave a UUID")
	}
}

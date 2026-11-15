//go:build unix

package backup

import "syscall"

// usedPct is how full the filesystem holding path is; -1 when it has no
// blocks to speak of.
func usedPct(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Blocks == 0 {
		return -1, nil
	}
	return 100 * (1 - float64(st.Bavail)/float64(st.Blocks)), nil
}

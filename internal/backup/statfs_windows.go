package backup

import "golang.org/x/sys/windows"

// usedPct is how full the volume holding path is; -1 when it has no size.
func usedPct(path string) (float64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	if total == 0 {
		return -1, nil
	}
	return 100 * (1 - float64(avail)/float64(total)), nil
}

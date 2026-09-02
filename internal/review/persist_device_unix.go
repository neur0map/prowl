//go:build unix

package review

import "golang.org/x/sys/unix"

// sameDevice reports whether paths a and b reside on the same filesystem device,
// so an os.Rename between them is atomic. It is the seam a PlanStore uses to
// refuse a snapshot lease on a different device before attempting a
// non-atomic move.
func sameDevice(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}

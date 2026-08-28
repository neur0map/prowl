//go:build linux

package boundedio

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// readlinkNoFollow reads the final symbolic link's target tied to a descriptor
// so the bytes cannot be re-resolved out from under the read. O_PATH|O_NOFOLLOW
// opens the link object itself (never its target); readlinkat with an empty
// path then reads through that exact descriptor. A final entry that is not a
// symlink, or that was swapped for a non-symlink, surfaces as a typed error.
func readlinkNoFollow(root *os.Root, comps []string, name string) (string, error) {
	dirFd, release, err := walkParents(root, comps, name)
	if err != nil {
		return "", err
	}
	defer release()

	last := comps[len(comps)-1]
	fd, err := unix.Openat(dirFd, last, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", &os.PathError{Op: "openat", Path: name, Err: err}
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", &os.PathError{Op: "fstat", Path: name, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return "", &os.PathError{Op: "readlink", Path: name, Err: unix.EINVAL}
	}
	target, err := readlinkatGrow(fd, "", name)
	if err != nil {
		return "", err
	}
	if target == "" {
		return "", fmt.Errorf("%w: %s: empty symlink target", os.ErrInvalid, name)
	}
	return target, nil
}

//go:build aix || darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package boundedio

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// readlinkNoFollow reads the final symbolic link's target. These platforms lack
// a portable O_PATH descriptor for a symlink, so the read is bracketed by two
// no-follow stats of the same descriptor-relative name: the target is accepted
// only if the entry was a symlink both before and after the read and its
// identity did not change. A concurrent swap surfaces as ErrChangedIdentity and
// the target is never opened or followed.
func readlinkNoFollow(root *os.Root, comps []string, name string) (string, error) {
	dirFd, release, err := walkParents(root, comps, name)
	if err != nil {
		return "", err
	}
	defer release()

	last := comps[len(comps)-1]
	var before unix.Stat_t
	if err := unix.Fstatat(dirFd, last, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", &os.PathError{Op: "fstatat", Path: name, Err: err}
	}
	if before.Mode&unix.S_IFMT != unix.S_IFLNK {
		return "", &os.PathError{Op: "readlink", Path: name, Err: unix.EINVAL}
	}
	target, err := readlinkatGrow(dirFd, last, name)
	if err != nil {
		return "", err
	}
	var after unix.Stat_t
	if err := unix.Fstatat(dirFd, last, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", &os.PathError{Op: "fstatat", Path: name, Err: err}
	}
	if after.Mode&unix.S_IFMT != unix.S_IFLNK || before.Dev != after.Dev || before.Ino != after.Ino {
		return "", fmt.Errorf("%w: %s", ErrChangedIdentity, name)
	}
	if target == "" {
		return "", fmt.Errorf("%w: %s: empty symlink target", os.ErrInvalid, name)
	}
	return target, nil
}

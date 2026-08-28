//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package boundedio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func openReadOnlyNonblocking(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// pathComponents splits a Unix relative path on '/' only. A backslash is an
// ordinary filename byte on Unix and must not act as a separator.
func pathComponents(name string) ([]string, error) {
	if strings.HasPrefix(name, "/") {
		return nil, fmt.Errorf("%w: absolute name %q", os.ErrInvalid, name)
	}
	return strings.Split(name, "/"), nil
}

// walkParents opens each parent directory descriptor relative to the previous
// one, rejecting a symbolic link at every intermediate component and verifying
// that the opened directory's identity matches the one that was checked. The
// caller owns the returned descriptor and must close it via release. The root
// descriptor is obtained from root itself so the walk is anchored to a trusted
// handle rather than a re-resolved path.
func walkParents(root *os.Root, comps []string, name string) (int, func(), error) {
	rootDir, err := root.Open(".")
	if err != nil {
		return -1, nil, err
	}
	dirFd := int(rootDir.Fd())
	closers := []func(){func() { rootDir.Close() }}
	release := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	for _, comp := range comps[:len(comps)-1] {
		var checked unix.Stat_t
		if err := unix.Fstatat(dirFd, comp, &checked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			release()
			return -1, nil, &os.PathError{Op: "fstatat", Path: name, Err: err}
		}
		if checked.Mode&unix.S_IFMT == unix.S_IFLNK {
			release()
			return -1, nil, fmt.Errorf("%w: %s", ErrSymlink, name)
		}
		if checked.Mode&unix.S_IFMT != unix.S_IFDIR {
			release()
			return -1, nil, fmt.Errorf("%w: %s", ErrNotDirectory, name)
		}
		nfd, err := unix.Openat(dirFd, comp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			release()
			if err == unix.ELOOP {
				return -1, nil, fmt.Errorf("%w: %s", ErrSymlink, name)
			}
			return -1, nil, &os.PathError{Op: "openat", Path: name, Err: err}
		}
		fd := nfd
		closers = append(closers, func() { unix.Close(fd) })
		var opened unix.Stat_t
		if err := unix.Fstat(fd, &opened); err != nil {
			release()
			return -1, nil, &os.PathError{Op: "fstat", Path: name, Err: err}
		}
		if opened.Dev != checked.Dev || opened.Ino != checked.Ino {
			release()
			return -1, nil, fmt.Errorf("%w: %s", ErrChangedIdentity, name)
		}
		dirFd = fd
	}
	return dirFd, release, nil
}

func openRegularNoFollow(root *os.Root, comps []string, name string) (*os.File, error) {
	dirFd, release, err := walkParents(root, comps, name)
	if err != nil {
		return nil, err
	}
	defer release()

	last := comps[len(comps)-1]
	var checked unix.Stat_t
	if err := unix.Fstatat(dirFd, last, &checked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &os.PathError{Op: "fstatat", Path: name, Err: err}
	}
	if checked.Mode&unix.S_IFMT == unix.S_IFLNK {
		return nil, fmt.Errorf("%w: %s", ErrSymlink, name)
	}
	if checked.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("%w: %s", ErrNonRegular, name)
	}
	fd, err := unix.Openat(dirFd, last, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if err == unix.ELOOP {
			return nil, fmt.Errorf("%w: %s", ErrSymlink, name)
		}
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		unix.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: name, Err: err}
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: %s", ErrNonRegular, name)
	}
	if opened.Dev != checked.Dev || opened.Ino != checked.Ino {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: %s", ErrChangedIdentity, name)
	}
	return os.NewFile(uintptr(fd), filepath.Join(root.Name(), name)), nil
}

// readlinkatGrow reads a symlink target relative to dirFd, growing the buffer
// until the target fits. Readlinkat truncates silently when the destination is
// too small, signalled by a full buffer. An empty linkName reads the symlink
// referred to by dirFd itself (used with an O_PATH|O_NOFOLLOW descriptor).
func readlinkatGrow(dirFd int, linkName, name string) (string, error) {
	for size := 256; size <= 64<<10; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(dirFd, linkName, buf)
		if err != nil {
			return "", &os.PathError{Op: "readlinkat", Path: name, Err: err}
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
	return "", fmt.Errorf("%w: %s: symlink target too long", os.ErrInvalid, name)
}

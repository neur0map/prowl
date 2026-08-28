//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package boundedio

import "os"

func openReadOnlyNonblocking(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}

// openRegularNoFollow fails closed: platforms without a verified no-follow
// guarantee must never fall back to an ordinary path open.
func openRegularNoFollow(root *os.Root, comps []string, name string) (*os.File, error) {
	return nil, ErrUnsupported
}

func readlinkNoFollow(root *os.Root, comps []string, name string) (string, error) {
	return "", ErrUnsupported
}

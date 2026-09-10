//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package boundedio

import (
	"fmt"
	"os"
	"strings"
)

func openReadOnlyNonblocking(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}

// pathComponents splits on '/' only; these platforms have no verified no-follow
// guarantee, so the split is only used for validation before failing closed.
func pathComponents(name string) ([]string, error) {
	if strings.HasPrefix(name, "/") {
		return nil, fmt.Errorf("%w: absolute name %q", os.ErrInvalid, name)
	}
	return strings.Split(name, "/"), nil
}

// openRegularNoFollow fails closed: platforms without a verified no-follow
// guarantee must never fall back to an ordinary path open.
func openRegularNoFollow(root *os.Root, comps []string, name string) (*os.File, error) {
	return nil, ErrUnsupported
}

func readlinkNoFollow(root *os.Root, comps []string, name string) (string, error) {
	return "", ErrUnsupported
}

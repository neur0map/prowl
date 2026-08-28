//go:build aix || darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package boundedio

import "os"

// readlinkNoFollow fails closed on non-Linux Unix. These platforms lack a
// portable way to read a symbolic link through a no-follow descriptor tied to
// the link object, so any name-relative read is vulnerable to an ABA swap of
// the final entry. Rather than return a target that may belong to a
// concurrently substituted link, the operation reports ErrUnsupported.
func readlinkNoFollow(root *os.Root, comps []string, name string) (string, error) {
	return "", ErrUnsupported
}

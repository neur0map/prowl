// Package boundedio provides rooted descriptor opens for deadline-sensitive local reads.
package boundedio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNonRegular reports that a bounded input resolved to a special file.
var ErrNonRegular = errors.New("bounded input is not a regular file")

// ErrNotDirectory reports that a traversed entry changed away from a directory.
var ErrNotDirectory = errors.New("bounded input is not a directory")

// ErrTooLarge reports that a bounded input exceeded its byte limit.
var ErrTooLarge = errors.New("bounded input exceeds byte limit")

// ErrSymlink reports that a bounded input, or one of its path components,
// resolved to a symbolic link. No-follow reads never traverse it.
var ErrSymlink = errors.New("bounded input traverses symbolic link")

// ErrChangedIdentity reports that a traversed component's identity changed
// between verification and open. It signals concurrent modification and is
// never a reason to follow a replacement target.
var ErrChangedIdentity = errors.New("bounded input identity changed")

// ErrUnsupported reports that no-follow reads have no verified implementation
// on the current platform. Callers must fail closed rather than fall back to an
// ordinary path open that could follow a symbolic link.
var ErrUnsupported = errors.New("bounded no-follow reads unsupported on this platform")

// OpenRegularNoFollow opens name relative to root, rejecting a symbolic link at
// any path component. Each component is walked descriptor-relative with no-
// follow semantics and the final descriptor is verified to be a regular file
// whose identity matches the pre-open check. It never follows a link and never
// falls back to an ordinary path open.
func OpenRegularNoFollow(root *os.Root, name string) (*os.File, error) {
	comps, err := splitComponents(name)
	if err != nil {
		return nil, err
	}
	return openRegularNoFollow(root, comps, name)
}

// ReadlinkNoFollow reads the target of the symbolic link at name relative to
// root without opening or following it. Intermediate components are walked with
// the same no-follow guarantees as OpenRegularNoFollow.
func ReadlinkNoFollow(root *os.Root, name string) (string, error) {
	comps, err := splitComponents(name)
	if err != nil {
		return "", err
	}
	return readlinkNoFollow(root, comps, name)
}

// splitComponents validates name as a relative, traversal-free path and splits
// it into ordinary path components. Separator, absolute, and volume rules are
// platform-specific (a backslash is an ordinary byte on Unix), so the split is
// delegated to pathComponents; parent traversal and empty parts are rejected
// uniformly here.
func splitComponents(name string) ([]string, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: empty name", os.ErrInvalid)
	}
	raw, err := pathComponents(name)
	if err != nil {
		return nil, err
	}
	comps := make([]string, 0, len(raw))
	for _, comp := range raw {
		switch comp {
		case "", ".":
			continue
		case "..":
			return nil, fmt.Errorf("%w: %q traverses parent", os.ErrInvalid, name)
		default:
			comps = append(comps, comp)
		}
	}
	if len(comps) == 0 {
		return nil, fmt.Errorf("%w: %q has no components", os.ErrInvalid, name)
	}
	return comps, nil
}

func OpenRegular(root *os.Root, name string) (*os.File, error) {
	file, err := openReadOnlyNonblocking(root, name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%w: %s", ErrNonRegular, name)
	}
	return file, nil
}

func OpenDirectory(root *os.Root, name string) (*os.File, error) {
	file, err := openReadOnlyNonblocking(root, name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.IsDir() {
		file.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotDirectory, name)
	}
	return file, nil
}

// ReadAllContext reads at most maxBytes from an already validated descriptor.
func ReadAllContext(ctx context.Context, file *os.File, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("bounded read limit must be positive")
	}
	result := make([]byte, 0, min(maxBytes, 128*1024))
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, err := file.Read(buffer)
		if count > 0 {
			if int64(len(result))+int64(count) > maxBytes {
				return nil, ErrTooLarge
			}
			result = append(result, buffer[:count]...)
		}
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

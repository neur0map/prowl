//go:build windows

package boundedio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openReadOnlyNonblocking(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}

// pathComponents splits a Windows relative path on both '/' and '\' and rejects
// anything that could escape the root: absolute paths, drive/UNC volume
// prefixes, and a ':' in any component (drive-relative or alternate data
// stream).
func pathComponents(name string) ([]string, error) {
	if filepath.VolumeName(name) != "" {
		return nil, fmt.Errorf("%w: %q has a volume or UNC prefix", os.ErrInvalid, name)
	}
	if name[0] == '/' || name[0] == '\\' {
		return nil, fmt.Errorf("%w: absolute name %q", os.ErrInvalid, name)
	}
	raw := strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' })
	for _, comp := range raw {
		if strings.ContainsRune(comp, ':') {
			return nil, fmt.Errorf("%w: %q has a drive or alternate-data-stream component", os.ErrInvalid, name)
		}
	}
	return raw, nil
}

// ntOpenChild opens a single path component relative to a parent directory
// handle using the NT namespace. FILE_OPEN_REPARSE_POINT makes the open stop at
// a reparse point instead of traversing it, so a symlink, junction, or mount
// point is opened as itself and then rejected. The returned handle's by-handle
// information is the verified identity of the opened object.
func ntOpenChild(parent windows.Handle, comp string, dir bool) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(comp)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    name,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	oa.Length = uint32(unsafe.Sizeof(oa))

	access := uint32(windows.FILE_GENERIC_READ | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if dir {
		access = windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)

	var iosb windows.IO_STATUS_BLOCK
	var handle windows.Handle
	if err := windows.NtCreateFile(&handle, access, &oa, &iosb, nil, 0, share, windows.FILE_OPEN, options, 0, 0); err != nil {
		return 0, mapNTStatus(err)
	}
	return handle, nil
}

func mapNTStatus(err error) error {
	switch err {
	case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND:
		return os.ErrNotExist
	case windows.STATUS_NOT_A_DIRECTORY:
		return ErrNotDirectory
	case windows.STATUS_FILE_IS_A_DIRECTORY:
		return ErrNonRegular
	default:
		return err
	}
}

// walkParentsWindows opens each parent directory relative to the previous one,
// rejecting any reparse point and verifying that every opened directory is a
// real directory. The caller owns the returned handle and must close it.
func walkParentsWindows(root *os.Root, comps []string, name string) (windows.Handle, func(), error) {
	rootDir, err := root.Open(".")
	if err != nil {
		return 0, nil, err
	}
	parent := windows.Handle(rootDir.Fd())
	closers := []func(){func() { rootDir.Close() }}
	release := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	for _, comp := range comps[:len(comps)-1] {
		handle, err := ntOpenChild(parent, comp, true)
		if err != nil {
			release()
			return 0, nil, &os.PathError{Op: "NtCreateFile", Path: name, Err: err}
		}
		h := handle
		closers = append(closers, func() { windows.CloseHandle(h) })
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			release()
			return 0, nil, &os.PathError{Op: "GetFileInformationByHandle", Path: name, Err: err}
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			release()
			return 0, nil, fmt.Errorf("%w: %s", ErrSymlink, name)
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			release()
			return 0, nil, fmt.Errorf("%w: %s", ErrNotDirectory, name)
		}
		parent = handle
	}
	return parent, release, nil
}

func openRegularNoFollow(root *os.Root, comps []string, name string) (*os.File, error) {
	parent, release, err := walkParentsWindows(root, comps, name)
	if err != nil {
		return nil, err
	}
	defer release()

	last := comps[len(comps)-1]
	handle, err := ntOpenChild(parent, last, false)
	if err != nil {
		return nil, &os.PathError{Op: "NtCreateFile", Path: name, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle)
		return nil, &os.PathError{Op: "GetFileInformationByHandle", Path: name, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("%w: %s", ErrSymlink, name)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("%w: %s", ErrNonRegular, name)
	}
	// info carries the verified identity (volume serial + file index) of the
	// object behind the handle we now own; there is no re-resolved path to race.
	return os.NewFile(uintptr(handle), filepath.Join(root.Name(), name)), nil
}

type reparseDataHeader struct {
	ReparseTag        uint32
	ReparseDataLength uint16
	Reserved          uint16
}

type symbolicLinkReparseBuffer struct {
	SubstituteNameOffset uint16
	SubstituteNameLength uint16
	PrintNameOffset      uint16
	PrintNameLength      uint16
	Flags                uint32
	PathBuffer           [1]uint16
}

type mountPointReparseBuffer struct {
	SubstituteNameOffset uint16
	SubstituteNameLength uint16
	PrintNameOffset      uint16
	PrintNameLength      uint16
	PathBuffer           [1]uint16
}

func decodeReparseTarget(buf []byte) (string, error) {
	if len(buf) < int(unsafe.Sizeof(reparseDataHeader{})) {
		return "", fmt.Errorf("%w: truncated reparse buffer", os.ErrInvalid)
	}
	header := (*reparseDataHeader)(unsafe.Pointer(&buf[0]))
	body := unsafe.Pointer(&buf[unsafe.Sizeof(reparseDataHeader{})])
	pick := func(pathBuffer *uint16, printOff, printLen, substOff, substLen uint16) string {
		off, length := printOff, printLen
		if length == 0 {
			off, length = substOff, substLen
		}
		words := (*[0xffff]uint16)(unsafe.Pointer(pathBuffer))
		start := off / 2
		return windows.UTF16ToString(words[start : start+length/2])
	}
	switch header.ReparseTag {
	case windows.IO_REPARSE_TAG_SYMLINK:
		data := (*symbolicLinkReparseBuffer)(body)
		return pick(&data.PathBuffer[0], data.PrintNameOffset, data.PrintNameLength, data.SubstituteNameOffset, data.SubstituteNameLength), nil
	case windows.IO_REPARSE_TAG_MOUNT_POINT:
		data := (*mountPointReparseBuffer)(body)
		return pick(&data.PathBuffer[0], data.PrintNameOffset, data.PrintNameLength, data.SubstituteNameOffset, data.SubstituteNameLength), nil
	default:
		return "", fmt.Errorf("%w: not a symbolic link", os.ErrInvalid)
	}
}

func readlinkNoFollow(root *os.Root, comps []string, name string) (string, error) {
	parent, release, err := walkParentsWindows(root, comps, name)
	if err != nil {
		return "", err
	}
	defer release()

	last := comps[len(comps)-1]
	handle, err := ntOpenChild(parent, last, false)
	if err != nil {
		// A reparse point may refuse FILE_NON_DIRECTORY_FILE when it targets a
		// directory; retry allowing either kind but still not traversing it.
		handle, err = ntOpenChildAny(parent, last)
		if err != nil {
			return "", &os.PathError{Op: "NtCreateFile", Path: name, Err: err}
		}
	}
	defer windows.CloseHandle(handle)

	buf := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var returned uint32
	if err := windows.DeviceIoControl(handle, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &buf[0], uint32(len(buf)), &returned, nil); err != nil {
		return "", &os.PathError{Op: "FSCTL_GET_REPARSE_POINT", Path: name, Err: err}
	}
	return decodeReparseTarget(buf[:returned])
}

func ntOpenChildAny(parent windows.Handle, comp string) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(comp)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	var iosb windows.IO_STATUS_BLOCK
	var handle windows.Handle
	if err := windows.NtCreateFile(&handle, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, &oa, &iosb, nil, 0, share, windows.FILE_OPEN, options, 0, 0); err != nil {
		return 0, mapNTStatus(err)
	}
	return handle, nil
}

//go:build windows

package uploader

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// isWindowsJunction reports whether path is a directory reparse point - a
// junction or a directory symlink: the shapes os.Stat follows but
// filepath.WalkDir refuses to descend, because filepath.EvalSymlinks
// leaves junctions unresolved (it returns the junction path itself).
func isWindowsJunction(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	// Lstat of a junction reports neither a directory nor a symlink: Go
	// surfaces FILE_ATTRIBUTE_REPARSE_POINT as ModeIrregular (the entry
	// looks like an irregular file), while os.Stat already followed it
	// and buildPlan saw the directory behind it. That asymmetry is
	// exactly the bug; detect the irregular-not-symlink shape.
	if info.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	return !info.Mode().IsRegular() && info.Mode()&fs.ModeIrregular != 0
}

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetFinalPathNameByHandleW = modkernel32.NewProc("GetFinalPathNameByHandleW")
)

// resolveWindowsJunction returns the directory a junction points at by
// asking the object manager for the final path of the opened handle - the
// same resolution os.Stat performs. The returned path carries the
// extended-length prefix; trim it for everyday use.
func resolveWindowsJunction(path string) (string, error) {
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := syscall.CreateFile(p16, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(handle)

	const bufLen = 32768
	var buf [bufLen]uint16
	// GetFinalPathNameByHandleW(handle, buf, bufLen, VOLUME_NAME_DOS)
	n, _, _ := procGetFinalPathNameByHandleW.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(bufLen),
		0,
	)
	if n == 0 || n >= bufLen {
		return "", fmt.Errorf("GetFinalPathNameByHandle failed for %s", path)
	}
	resolved := trimFinalPath(syscall.UTF16ToString(buf[:n]))
	if resolved == "" {
		return "", fmt.Errorf("junction %s resolved to an empty path", path)
	}
	return resolved, nil
}

// trimFinalPath reduces the extended-length form GetFinalPathNameByHandleW
// returns to the spelling every other API accepts: the \\?\ prefix comes
// off a local volume path (a drive path keeps its drive), and a network
// path's \\?\UNC\ prefix becomes the ordinary \\server\share UNC form
// rather than being stripped wholesale, which would corrupt it into a
// bogus relative local path.
func trimFinalPath(resolved string) string {
	switch {
	case strings.HasPrefix(resolved, `\\?\UNC\`):
		resolved = `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`)
	case strings.HasPrefix(resolved, `\\?\`):
		resolved = strings.TrimPrefix(resolved, `\\?\`)
	}
	return filepath.Clean(resolved)
}

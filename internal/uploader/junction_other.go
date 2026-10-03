//go:build !windows

package uploader

// isWindowsJunction is a Windows concept; nothing to detect elsewhere.
func isWindowsJunction(string) bool { return false }

// resolveWindowsJunction is never called off Windows; isWindowsJunction
// guards the only call site.
func resolveWindowsJunction(path string) (string, error) {
	return path, nil
}

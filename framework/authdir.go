package framework

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultAuthDir is the host's auth directory when its configuration names none.
const DefaultAuthDir = "~/.cli-proxy-api"

// ResolveAuthDir expands a leading ~ to the home directory, accepting either
// path separator after it, and cleans the path. An empty dir stays empty.
func ResolveAuthDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if strings.HasPrefix(dir, "~") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			rest := strings.TrimLeft(strings.TrimPrefix(dir, "~"), "/\\")
			if rest == "" {
				return filepath.Clean(home)
			}
			return filepath.Clean(filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(rest, "\\", "/"))))
		}
	}
	if dir == "" {
		return ""
	}
	return filepath.Clean(dir)
}

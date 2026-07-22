package grounding

import (
	"path/filepath"
	"strings"
)

// InferProjectID derives a project_id from a ~/.claude/projects/<encoded-path>/
// directory name. Example: "-home-sophi-dev-corpos-toolkit" -> "corpos-toolkit"
// (everything after the "dev" segment, joined with "-"). Falls back to the last
// hyphen-separated segment when "dev" is absent.
func InferProjectID(dir string) string {
	name := filepath.Base(filepath.Clean(dir))
	if name == "" || name == "." || name == "/" {
		return "unknown"
	}
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if p == "dev" && i+1 < len(parts) {
			return strings.Join(parts[i+1:], "-")
		}
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return parts[len(parts)-1]
}

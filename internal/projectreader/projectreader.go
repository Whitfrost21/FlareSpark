// Package projectreader walks a project directory on disk and returns a
// token-efficient representation for use with Groq.
//
// It never uses Zed RPC — the agent binary runs on the same machine so we
// read the filesystem directly with os.ReadDir.
//
// Two-pass workflow (orchestrated by the handler):
//
//  1. Tree(root) → compact file-tree string   (≈1 token per path)
//  2. Groq picks which files to read
//  3. ReadFiles(root, paths) → file contents  (only what's needed)
//
// Secret files are NEVER included in the tree or read, regardless of how
// they are requested. This is enforced in both Walk and ReadFiles.
package projectreader

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// rootMarkers are filenames that indicate a project root.
var rootMarkers = []string{
	"go.mod",
	"package.json",
	"Cargo.toml",
	"pyproject.toml",
	"setup.py",
	".git",
	"Makefile",
	"CMakeLists.txt",
}

// skipDirs are directories we never descend into.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	"out":          true,
	".next":        true,
	".nuxt":        true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".idea":        true,
	".vscode":      true,
	"target":       true, // Rust
	"bin":          true,
	"obj":          true, // .NET
	"secrets":      true,
	"credentials":  true,
}

// skipExts are file extensions we skip in the tree and never read.
var skipExts = map[string]bool{
	// Binaries / media
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".svg": true, ".ico": true, ".woff": true, ".woff2": true, ".ttf": true,
	".eot": true, ".mp4": true, ".mp3": true, ".wav": true, ".ogg": true,
	".zip": true, ".tar": true, ".gz": true, ".rar": true, ".7z": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".a": true,
	".o": true, ".pyc": true, ".class": true,
	".lock": true, // package-lock.json, yarn.lock, go.sum — noise not signal
	// Secret / credential file extensions
	".pem": true, ".key": true, ".p12": true, ".pfx": true,
	".cer": true, ".crt": true, ".der": true,
	".jks": true, // Java keystores
}

// secretFileNames are exact filenames (case-insensitive) that are NEVER read
// or shown in the tree, no matter what. This list is checked in both Walk
// and ReadFiles so Groq can never request them either.
var secretFileNames = map[string]bool{
	// dotenv variants
	".env":             true,
	".env.local":       true,
	".env.development": true,
	".env.production":  true,
	".env.staging":     true,
	".env.test":        true,
	".env.example":     false, // example files are safe — explicitly allowed
	".envrc":           true,  // direnv
	// Secret / credential files
	".netrc":       true,
	".npmrc":       true, // may contain auth tokens
	".pypirc":      true,
	".htpasswd":    true,
	"credentials":  true,
	"secrets.yaml": true,
	"secrets.yml":  true,
	"secrets.json": true,
	"secrets.toml": true,
	"secret.yaml":  true,
	"secret.yml":   true,
	"secret.json":  true,
	// Cloud / infra credentials
	"terraform.tfvars":    true,
	".terraform.lock.hcl": false, // lock file, not secrets
	"kubeconfig":          true,
	"serviceaccount.json": true,
	// SSH / GPG
	"id_rsa":      true,
	"id_ed25519":  true,
	"id_ecdsa":    true,
	"id_dsa":      true,
	"known_hosts": true,
}

// maxFileSizeBytes — files larger than this are truncated, not fully read.
const maxFileSizeBytes = 32 * 1024 // 32 KB

// isSecret returns true if a filename should never be read or shown.
// Checks exact name match (case-insensitive) and secret extensions.
func isSecret(name string) bool {
	lower := strings.ToLower(name)
	// Exact name match.
	if blocked, exists := secretFileNames[lower]; exists {
		return blocked
	}
	// Any file whose name starts with ".env" (catches .env.anything).
	if strings.HasPrefix(lower, ".env") && lower != ".env.example" {
		return true
	}
	// Secret extensions.
	ext := filepath.Ext(lower)
	return skipExts[ext] && (ext == ".pem" || ext == ".key" || ext == ".p12" ||
		ext == ".pfx" || ext == ".cer" || ext == ".crt" || ext == ".der" || ext == ".jks")
}

// DetectRoot walks upward from any known file URI until it finds a root marker.
// Returns "" if no marker is found — caller should prompt user for explicit root.
func DetectRoot(fileURIs []string) string {
	for _, uri := range fileURIs {
		path := strings.TrimPrefix(uri, "file://")
		dir := filepath.Dir(path)
		if root := walkUpForRoot(dir); root != "" {
			return root
		}
	}
	return ""
}

// ResolveRoot returns an explicit path if provided, otherwise falls back to
// DetectRoot. The explicit path is cleaned and verified to exist.
// This is the single entry point handlers should use.
func ResolveRoot(explicitPath string, fileURIs []string) (string, error) {
	if explicitPath != "" {
		clean := filepath.Clean(explicitPath)
		info, err := os.Stat(clean)
		if err != nil {
			return "", fmt.Errorf("path %q does not exist: %w", clean, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("path %q is not a directory", clean)
		}
		return clean, nil
	}
	root := DetectRoot(fileURIs)
	if root == "" {
		return "", nil
	}
	return root, nil
}

func walkUpForRoot(dir string) string {
	prev := ""
	for dir != prev {
		for _, marker := range rootMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		prev = dir
		dir = filepath.Dir(dir)
	}
	return ""
}

// Entry is a single file in the project tree.
type Entry struct {
	// RelPath is the path relative to the project root (e.g. "internal/foo/bar.go").
	RelPath string
	// Size in bytes.
	Size int64
}

// Walk returns all non-ignored files under root, sorted by path.
// maxFiles caps the result so the tree stays token-safe (default: 400).
func Walk(root string, maxFiles int) ([]Entry, error) {
	if maxFiles <= 0 {
		maxFiles = 400
	}

	var entries []Entry

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable paths silently
		}

		name := d.Name()

		if d.IsDir() {
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		// Block secrets before anything else — hard stop, no exceptions.
		if isSecret(name) {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(name))
		if skipExts[ext] {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		entries = append(entries, Entry{RelPath: rel, Size: info.Size()})

		if len(entries) >= maxFiles {
			return filepath.SkipAll
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].RelPath < entries[j].RelPath
	})

	return entries, nil
}

// Tree returns a compact, human-readable file tree string suitable for
// sending to Groq as context (~1 token per path).
//
// Format:
//
//	internal/
//	  filereader/
//	    filereader.go          (2.1 KB)
//	  handler/
//	    handler.go             (8.4 KB)
//	main.go                    (3.2 KB)
func Tree(root string, maxFiles int) (string, error) {
	entries, err := Walk(root, maxFiles)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "(empty project)", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Project root: %s\n", root))
	sb.WriteString(fmt.Sprintf("Files: %d\n\n", len(entries)))

	// Render as indented tree grouped by directory.
	prevParts := []string{}
	for _, e := range entries {
		parts := strings.Split(e.RelPath, string(filepath.Separator))
		// Print any new directory segments.
		for i := 0; i < len(parts)-1; i++ {
			if i >= len(prevParts) || parts[i] != prevParts[i] {
				sb.WriteString(strings.Repeat("  ", i))
				sb.WriteString(parts[i] + "/\n")
			}
		}
		// Print the file with size.
		indent := strings.Repeat("  ", len(parts)-1)
		size := humanSize(e.Size)
		sb.WriteString(fmt.Sprintf("%s%s  (%s)\n", indent, parts[len(parts)-1], size))
		prevParts = parts
	}

	return sb.String(), nil
}

// ReadFiles reads the content of the given relative paths under root.
// Files over maxFileSizeBytes get a truncation notice instead of full content.
// Secret files are silently skipped — Groq cannot request them.
// Returns a formatted string ready to append to a Groq prompt.
func ReadFiles(root string, relPaths []string) string {
	var sb strings.Builder
	for _, rel := range relPaths {
		rel = filepath.Clean(rel)
		// Safety: never escape the project root.
		if strings.HasPrefix(rel, "..") {
			continue
		}
		// Hard block: never read secrets even if Groq explicitly asks for them.
		if isSecret(filepath.Base(rel)) {
			sb.WriteString(fmt.Sprintf("\n\n--- %s ---\n[blocked: secret/credential file]\n", rel))
			continue
		}
		abs := filepath.Join(root, rel)
		data, err := os.ReadFile(abs)
		if err != nil {
			sb.WriteString(fmt.Sprintf("\n\n--- %s ---\n[error reading file: %v]\n", rel, err))
			continue
		}
		if len(data) > maxFileSizeBytes {
			sb.WriteString(fmt.Sprintf(
				"\n\n--- %s --- (%s, truncated to first 32 KB)\n```\n%s\n```",
				rel, humanSize(int64(len(data))), string(data[:maxFileSizeBytes]),
			))
		} else {
			ext := strings.TrimPrefix(filepath.Ext(rel), ".")
			sb.WriteString(fmt.Sprintf("\n\n--- %s ---\n```%s\n%s\n```", rel, ext, string(data)))
		}
	}
	return sb.String()
}

// ParseFileList extracts a list of relative file paths from a Groq response.
// Groq is prompted to return one path per line inside a <files> block.
// Example response:
//
//	<files>
//	internal/handler/handler.go
//	internal/rpc/params.go
//	</files>
func ParseFileList(response string) []string {
	start := strings.Index(response, "<files>")
	end := strings.Index(response, "</files>")
	if start == -1 || end == -1 || end <= start {
		// Fallback: treat every line that looks like a path as a candidate.
		return looksLikePaths(response)
	}
	block := response[start+len("<files>") : end]
	return splitLines(block)
}

func looksLikePaths(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// A path-like line has a dot or a slash but no spaces.
		if (strings.Contains(line, "/") || strings.Contains(line, ".")) && !strings.Contains(line, " ") {
			out = append(out, line)
		}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func humanSize(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d B", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
	}
}

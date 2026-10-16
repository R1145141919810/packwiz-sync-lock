package core

import (
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/viper"
)

// Sync lock modes. The mode decides what happens when a file outside of the locked
// folders is found.
const (
	// SyncModeOff disables the lock completely; every path is allowed.
	SyncModeOff = "off"
	// SyncModeWarn reports files outside of the locked folders but still syncs them.
	SyncModeWarn = "warn"
	// SyncModeStrip (the default) removes files outside of the locked folders from the index.
	SyncModeStrip = "strip"
	// SyncModeDeny behaves like strip, but additionally makes the command exit with a
	// non-zero status so that CI/builds fail loudly.
	SyncModeDeny = "deny"
)

// SyncLock is the [sync] table of pack.toml.
//
// It restricts the folders a pack is allowed to sync. Because packwiz-installer derives
// the list of files it downloads exclusively from index.toml, removing out-of-scope files
// from the index is what stops the installer from ever writing them to a client.
type SyncLock struct {
	// AllowedPaths lists the folders (relative to pack.toml, using forward slashes) that may
	// be synced. A single "*" (or ".") allows every path that stays inside the pack folder.
	AllowedPaths []string `toml:"allowed-paths"`
	// Mode is one of off, warn, strip or deny. An empty value means strip.
	Mode string `toml:"mode,omitempty"`
	// BlockEscapes rejects paths that try to leave the pack folder (absolute paths, "..",
	// Windows volume letters). It defaults to true; only disable it if you really know why.
	BlockEscapes *bool `toml:"block-escapes,omitempty"`
	// Note is an optional free-form comment, shown by "packwiz lock show".
	Note string `toml:"note,omitempty"`
}

// SyncViolation records a single path that the lock rejected.
type SyncViolation struct {
	// Path is the pack-relative path of the file that was rejected
	Path string `json:"path"`
	// Dest is the pack-relative destination a metadata file would have installed, if it
	// differs from Path
	Dest string `json:"dest,omitempty"`
	// Reason explains why the path was rejected
	Reason string `json:"reason"`
}

// SyncPathError is returned for paths that can never be used safely: empty paths, paths
// containing a NUL byte, absolute paths, Windows volume letters and paths that escape the
// pack folder.
type SyncPathError struct {
	Path   string
	Reason string
}

func (e *SyncPathError) Error() string {
	return fmt.Sprintf("unsafe path %q: %s", e.Path, e.Reason)
}

// OutsideLockError is returned for paths that are safe but outside of the locked folders.
type OutsideLockError struct {
	Path    string
	Allowed []string
}

func (e *OutsideLockError) Error() string {
	return fmt.Sprintf("%q is outside of the locked folders (%s)", e.Path, strings.Join(e.Allowed, ", "))
}

// CaseInsensitivePathLock is true on platforms where paths are (by default) case-insensitive.
// The lock follows the platform here: on Windows "MODS/x.jar" and "mods/x.jar" are the same
// file, so they must be treated the same way or the lock could be bypassed with a capital letter.
var CaseInsensitivePathLock = caseInsensitiveFS()

func caseInsensitiveFS() bool {
	switch runtime.GOOS {
	case "windows", "darwin":
		// Both platforms use case-insensitive filesystems by default, so "MODS/x.jar"
		// and "mods/x.jar" refer to the same file and must be judged identically.
		return true
	default:
		return false
	}
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// NormalizeRelPath converts a pack-relative path into its canonical forward-slash form.
//
// It rejects anything that could leave the pack folder, which is exactly the set of tricks
// that could otherwise be used to make packwiz-installer write outside of the pack.
func NormalizeRelPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", &SyncPathError{Path: p, Reason: "path contains a NUL byte"}
	}
	if strings.TrimSpace(p) == "" {
		return "", &SyncPathError{Path: p, Reason: "path is empty"}
	}
	// Treat Windows separators as separators everywhere, like the installer does
	p = strings.ReplaceAll(p, "\\", "/")
	// The installer normalises URL-encoded dots before validating; do the same
	p = strings.ReplaceAll(p, "%2e", ".")
	p = strings.ReplaceAll(p, "%2E", ".")
	lower := strings.ToLower(p)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return "", &SyncPathError{Path: p, Reason: "path contains a URL-encoded path separator"}
	}
	if strings.HasPrefix(p, "/") {
		return "", &SyncPathError{Path: p, Reason: "absolute paths are not allowed"}
	}
	if len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0]) {
		return "", &SyncPathError{Path: p, Reason: "Windows volume letters are not allowed"}
	}
	cleaned := path.Clean(p)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", &SyncPathError{Path: p, Reason: "path escapes the pack folder"}
	}
	if cleaned == "/" || cleaned == "." {
		return ".", nil
	}
	// packwiz-installer refuses a component like "C:" because it can climb out of the pack
	// folder on Windows. Refuse it here too, so that both sides agree on what is safe.
	for _, component := range strings.Split(cleaned, "/") {
		if len(component) == 2 && component[1] == ':' && isASCIILetter(component[0]) {
			return "", &SyncPathError{Path: p, Reason: "Windows volume letters are not allowed"}
		}
	}
	return cleaned, nil
}

// ModeValue returns the effective mode of the lock, defaulting to strip.
func (l SyncLock) ModeValue() string {
	switch strings.ToLower(strings.TrimSpace(l.Mode)) {
	case SyncModeOff:
		return SyncModeOff
	case SyncModeWarn:
		return SyncModeWarn
	case SyncModeDeny:
		return SyncModeDeny
	default:
		return SyncModeStrip
	}
}

// IsActive reports whether the lock restricts anything at all.
func (l SyncLock) IsActive() bool {
	return len(l.AllowedPaths) > 0 && l.ModeValue() != SyncModeOff
}

// BlocksEscapes reports whether the lock refuses paths that leave the pack folder.
func (l SyncLock) BlocksEscapes() bool {
	return l.BlockEscapes == nil || *l.BlockEscapes
}

// AllowsAll reports whether every path inside of the pack folder is allowed.
func (l SyncLock) AllowsAll() bool {
	for _, p := range l.AllowedPaths {
		if p == "*" || p == "." {
			return true
		}
	}
	return false
}

// Normalize validates the lock as read from pack.toml, cleaning up the allowed paths.
func (l SyncLock) Normalize() (SyncLock, error) {
	out := l
	rawMode := strings.ToLower(strings.TrimSpace(l.Mode))
	switch rawMode {
	case "", SyncModeStrip:
		out.Mode = SyncModeStrip
	case SyncModeOff, SyncModeWarn, SyncModeDeny:
		out.Mode = rawMode
	default:
		return SyncLock{}, fmt.Errorf("invalid sync mode %q (expected %q, %q, %q or %q)",
			l.Mode, SyncModeOff, SyncModeWarn, SyncModeStrip, SyncModeDeny)
	}

	var cleaned []string
	seen := make(map[string]bool)
	for _, raw := range l.AllowedPaths {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if raw == "*" {
			cleaned = append(cleaned, "*")
			continue
		}
		rel, err := NormalizeRelPath(raw)
		if err != nil {
			return SyncLock{}, fmt.Errorf("invalid allowed path in the [sync] table: %w", err)
		}
		key := rel
		if CaseInsensitivePathLock {
			key = strings.ToLower(rel)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, rel)
	}
	sort.Strings(cleaned)
	out.AllowedPaths = cleaned
	return out, nil
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// Allows reports whether an already canonical pack-relative path is inside the locked folders.
func (l SyncLock) Allows(rel string) bool {
	if l.AllowsAll() {
		return true
	}
	rel = strings.TrimSuffix(strings.ReplaceAll(rel, "\\", "/"), "/")
	for _, allowed := range l.AllowedPaths {
		a := strings.TrimSuffix(allowed, "/")
		if a == "*" || a == "." {
			return true
		}
		if rel == a || strings.HasPrefix(rel, a+"/") {
			return true
		}
		if CaseInsensitivePathLock && (strings.EqualFold(rel, a) || hasPrefixFold(rel, a+"/")) {
			return true
		}
	}
	return false
}

// CouldContain reports whether a folder may (directly or indirectly) contain allowed files.
// It is used to prune whole directory trees while walking a pack.
func (l SyncLock) CouldContain(dirRel string) bool {
	if l.AllowsAll() {
		return true
	}
	dirRel = strings.TrimSuffix(strings.ReplaceAll(dirRel, "\\", "/"), "/")
	if dirRel == "" || dirRel == "." || dirRel == "/" {
		return true
	}
	for _, allowed := range l.AllowedPaths {
		a := strings.TrimSuffix(allowed, "/")
		if a == "*" || a == "." {
			return true
		}
		if a == dirRel || strings.HasPrefix(a, dirRel+"/") {
			return true
		}
		if CaseInsensitivePathLock && (strings.EqualFold(a, dirRel) || hasPrefixFold(a, dirRel+"/")) {
			return true
		}
	}
	return false
}

// Check validates a single pack-relative path against the lock. It returns nil when the
// path may be synced.
func (l SyncLock) Check(rel string) error {
	if !l.IsActive() {
		return nil
	}
	cleaned, err := NormalizeRelPath(rel)
	if err != nil {
		var pathErr *SyncPathError
		if errors.As(err, &pathErr) && !l.BlocksEscapes() && l.AllowsAll() {
			return nil
		}
		return err
	}
	if !l.Allows(cleaned) {
		return &OutsideLockError{Path: cleaned, Allowed: l.AllowedPaths}
	}
	return nil
}

// CheckMetaDest resolves the destination that a metadata (.pw.toml) file installs into and
// validates it against the lock.
//
// metaRelPath is the pack-relative path of the metadata file itself and filename is the
// "filename" key it declares. Both the metadata file and its destination must be inside the
// locked folders, otherwise a metadata file could be used to write anywhere in the instance.
func (l SyncLock) CheckMetaDest(metaRelPath string, filename string) (string, error) {
	metaRelPath = strings.ReplaceAll(metaRelPath, "\\", "/")
	if strings.TrimSpace(filename) == "" {
		return "", fmt.Errorf("metadata file %q does not declare a filename, so its destination cannot be verified", metaRelPath)
	}
	// packwiz-installer resolves the filename the same way: a filename that starts with a
	// separator counts from the pack root, anything else counts from the folder holding the
	// metadata file. Mirroring it here is what stops "/pwned.jar" from slipping through.
	name := strings.ReplaceAll(filename, "\\", "/")
	dest := name
	if strings.HasPrefix(name, "/") {
		dest = strings.TrimLeft(name, "/")
	} else {
		dir := path.Dir(path.Clean(metaRelPath))
		if dir != "." && dir != "/" {
			dest = dir + "/" + name
		}
	}
	cleaned, err := NormalizeRelPath(dest)
	if err != nil {
		return dest, err
	}
	if l.IsActive() && !l.Allows(cleaned) {
		return cleaned, &OutsideLockError{Path: cleaned, Allowed: l.AllowedPaths}
	}
	return cleaned, nil
}

// SyncLockDescription returns a human readable one line summary of the lock.
func (l SyncLock) SyncLockDescription() string {
	if !l.IsActive() {
		return "no sync lock configured (every file in the pack may be synced)"
	}
	folders := strings.Join(l.AllowedPaths, ", ")
	if l.AllowsAll() {
		folders = "every folder inside the pack"
	}
	return fmt.Sprintf("only %s may be synced (mode: %s)", folders, l.ModeValue())
}

// LoadSyncLock reads the [sync] table from the pack file currently selected by --pack-file.
// A pack without a [sync] table returns an inactive lock and no error.
func LoadSyncLock() (SyncLock, error) {
	packFile := viper.GetString("pack-file")
	if strings.TrimSpace(packFile) == "" {
		packFile = "pack.toml"
	}

	var wrapper struct {
		Sync SyncLock `toml:"sync"`
	}
	md, err := toml.DecodeFile(packFile, &wrapper)
	if err != nil {
		if os.IsNotExist(err) {
			return SyncLock{}, nil
		}
		return SyncLock{}, err
	}
	if !md.IsDefined("sync") {
		return SyncLock{}, nil
	}

	var unknown []string
	for _, key := range md.Undecoded() {
		if len(key) > 0 && key[0] == "sync" {
			unknown = append(unknown, strings.Join(key, "."))
		}
	}
	if len(unknown) > 0 {
		return SyncLock{}, fmt.Errorf("unknown key(s) in the [sync] table of %s: %s (known keys: allowed-paths, mode, block-escapes, note)",
			packFile, strings.Join(unknown, ", "))
	}

	lock, err := wrapper.Sync.Normalize()
	if err != nil {
		return SyncLock{}, fmt.Errorf("%s: %w", packFile, err)
	}
	if len(lock.AllowedPaths) == 0 && lock.ModeValue() != SyncModeOff {
		return SyncLock{}, fmt.Errorf("%s: the [sync] table does not allow any folder; add allowed-paths or set mode = %q", packFile, SyncModeOff)
	}
	return lock, nil
}

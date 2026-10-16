package core

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// PackLocation abstracts where a pack lives: a local directory or an HTTP(S) URL.
//
// Both "packwiz sync" (the client side) and "packwiz lock check --preflight" (the authoring
// side) use it, so both always look at the pack in exactly the same way.
type PackLocation interface {
	// Describe returns a human readable description of where the pack lives
	Describe() string
	// Resolve returns a displayable location for a pack-relative path
	Resolve(relPath string) string
	// ReadAll reads a whole pack-relative file
	ReadAll(relPath string) ([]byte, error)
	// Open streams a pack-relative file
	Open(relPath string) (io.ReadCloser, error)
	// LocalPath returns the OS path of a pack-relative file, when the pack is local
	LocalPath(relPath string) (string, bool)
	// RawURL returns the URL of a pack-relative file, when the pack is served over HTTP
	RawURL(relPath string) (string, bool)
}

// DirPackLocation is a pack stored in a local directory.
type DirPackLocation struct {
	Root string
}

// NewDirPackLocation returns the location of the pack that packFile belongs to.
func NewDirPackLocation(packFile string) DirPackLocation {
	root := filepath.Dir(packFile)
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	return DirPackLocation{Root: root}
}

func (d DirPackLocation) Describe() string { return "local pack in " + d.Root }

func (d DirPackLocation) Resolve(relPath string) string {
	return filepath.Join(d.Root, filepath.FromSlash(relPath))
}

func (d DirPackLocation) LocalPath(relPath string) (string, bool) { return d.Resolve(relPath), true }

func (d DirPackLocation) RawURL(string) (string, bool) { return "", false }

func (d DirPackLocation) ReadAll(relPath string) ([]byte, error) {
	return os.ReadFile(d.Resolve(relPath))
}

func (d DirPackLocation) Open(relPath string) (io.ReadCloser, error) {
	return os.Open(d.Resolve(relPath))
}

// URLPackLocation is a pack served over HTTP(S).
type URLPackLocation struct {
	Base *url.URL
}

// NewURLPackLocation parses the URL a pack.toml is served from.
func NewURLPackLocation(rawURL string) (URLPackLocation, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return URLPackLocation{}, fmt.Errorf("invalid pack URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return URLPackLocation{}, fmt.Errorf("unsupported pack URL scheme %q (only http and https are supported)", parsed.Scheme)
	}
	clean := *parsed
	clean.RawQuery = ""
	clean.Fragment = ""
	return URLPackLocation{Base: &clean}, nil
}

func (u URLPackLocation) urlFor(relPath string) string {
	relPath = strings.TrimPrefix(strings.ReplaceAll(relPath, "\\", "/"), "/")
	clean := *u.Base
	return clean.ResolveReference(&url.URL{Path: relPath}).String()
}

func (u URLPackLocation) Describe() string { return u.Base.String() }

func (u URLPackLocation) Resolve(relPath string) string { return u.urlFor(relPath) }

func (u URLPackLocation) LocalPath(string) (string, bool) { return "", false }

func (u URLPackLocation) RawURL(relPath string) (string, bool) { return u.urlFor(relPath), true }

func (u URLPackLocation) ReadAll(relPath string) ([]byte, error) {
	resp, err := GetWithUA(u.urlFor(relPath), "text/plain, application/toml, */*")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (u URLPackLocation) Open(relPath string) (io.ReadCloser, error) {
	return OpenURL(u.urlFor(relPath))
}

// OpenURL fetches a file from an absolute http(s) URL.
func OpenURL(rawURL string) (io.ReadCloser, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid download URL %q: %w", rawURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported download URL scheme %q", parsed.Scheme)
	}
	resp, err := GetWithUA(rawURL, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// HashReaderWithFormat hashes everything read from r, returning the hash and the byte count.
func HashReaderWithFormat(format string, r io.Reader) (string, int64, error) {
	if strings.TrimSpace(format) == "" {
		format = "sha256"
	}
	hasher, err := GetHashImpl(format)
	if err != nil {
		return "", 0, err
	}
	written, err := io.Copy(hasher, r)
	if err != nil {
		return "", written, err
	}
	return hasher.HashToString(hasher.Sum(nil)), written, nil
}

// FileMatchesHash reports whether the file at path already has the expected hash.
func FileMatchesHash(path string, format string, expected string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	actual, _, err := HashReaderWithFormat(format, file)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(actual, expected), nil
}

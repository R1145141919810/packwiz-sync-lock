package core

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	gitignore "github.com/sabhiram/go-gitignore"
	"github.com/spf13/viper"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
)

// Index is a representation of the index.toml file for referencing all the files in a pack.
type Index struct {
	HashFormat string
	Files      IndexFiles
	indexFile  string
	packRoot   string
	// Lock is the sync lock that was in force the last time this index was refreshed,
	// or nil when the pack does not lock any folders.
	Lock *SyncLock
	// LockViolations records everything the sync lock rejected during the last refresh.
	LockViolations []SyncViolation
}

// indexTomlRepresentation is the TOML representation of Index (Files must be converted)
type indexTomlRepresentation struct {
	HashFormat string                       `toml:"hash-format"`
	Files      indexFilesTomlRepresentation `toml:"files"`
}

// LoadIndex attempts to load the index file from a path
func LoadIndex(indexFile string) (Index, error) {
	// Decode as indexTomlRepresentation then convert to Index
	var rep indexTomlRepresentation
	if _, err := toml.DecodeFile(indexFile, &rep); err != nil {
		return Index{}, err
	}
	if len(rep.HashFormat) == 0 {
		rep.HashFormat = "sha256"
	}
	index := Index{
		HashFormat: rep.HashFormat,
		Files:      rep.Files.toMemoryRep(),
		indexFile:  indexFile,
		packRoot:   filepath.Dir(indexFile),
	}
	return index, nil
}

// LoadIndexFromBytes parses an index that was fetched from somewhere instead of read from
// disk. It is used for remote packs, where there is no local index file to point at.
func LoadIndexFromBytes(data []byte, name string) (Index, error) {
	var rep indexTomlRepresentation
	if _, err := toml.Decode(string(data), &rep); err != nil {
		return Index{}, err
	}
	if len(rep.HashFormat) == 0 {
		rep.HashFormat = "sha256"
	}
	return Index{
		HashFormat: rep.HashFormat,
		Files:      rep.Files.toMemoryRep(),
		indexFile:  name,
		packRoot:   filepath.Dir(name),
	}, nil
}

// RemoveFile removes a file from the index, given a file path
func (in *Index) RemoveFile(path string) error {
	relPath, err := in.RelIndexPath(path)
	if err != nil {
		return err
	}
	delete(in.Files, relPath)
	return nil
}

func (in *Index) updateFileHashGiven(path, format, hash string, markAsMetaFile bool) error {
	// Remove format if equal to index hash format
	if in.HashFormat == format {
		format = ""
	}

	// Find in index
	relPath, err := in.RelIndexPath(path)
	if err != nil {
		return err
	}
	in.Files.updateFileEntry(relPath, format, hash, markAsMetaFile)
	return nil
}

// updateFile calculates the hash for a given path and updates it in the index
func (in *Index) updateFile(path string) error {
	var hashString string
	if viper.GetBool("no-internal-hashes") {
		hashString = ""
	} else {
		f, err := os.Open(path)
		if err != nil {
			return err
		}

		// Hash usage strategy (may change):
		// Just use SHA256, overwrite existing hash regardless of what it is
		// May update later to continue using the same hash that was already being used
		h, err := GetHashImpl("sha256")
		if err != nil {
			_ = f.Close()
			return err
		}
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return err
		}
		err = f.Close()
		if err != nil {
			return err
		}
		hashString = h.HashToString(h.Sum(nil))
	}

	markAsMetaFile := false
	// If the file has an extension of pw.toml, set markAsMetaFile to true
	if strings.HasSuffix(filepath.Base(path), MetaExtension) {
		markAsMetaFile = true
	}

	return in.updateFileHashGiven(path, "sha256", hashString, markAsMetaFile)
}

// IndexFilePath returns the OS path of the index file that was loaded.
func (in Index) IndexFilePath() string { return in.indexFile }

// ResolveIndexPath turns a path from the index into a file path on disk
func (in Index) ResolveIndexPath(p string) string {
	return filepath.Join(in.packRoot, filepath.FromSlash(p))
}

// RelIndexPath turns a file path on disk into a path from the index
func (in Index) RelIndexPath(p string) (string, error) {
	rel, err := filepath.Rel(in.packRoot, p)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

var ignoreDefaults = []string{
	// Defaults (can be overridden with a negating pattern preceded with !)

	// Exclude Git metadata
	".git/**",
	".gitattributes",
	".gitignore",

	// Exclude macOS metadata
	".DS_Store",

	// Exclude exported CurseForge zip files
	"/*.zip",

	// Exclude exported Modrinth packs
	"*.mrpack",

	// Exclude packwiz binaries, if the user puts them in their pack folder
	"packwiz.exe",
	"packwiz", // Note: also excludes packwiz/ as a directory - you can negate this pattern if you want a directory called packwiz
}

func readGitignore(path string) (*gitignore.GitIgnore, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		// TODO: check for read errors (and present them)
		return gitignore.CompileIgnoreLines(ignoreDefaults...), false
	}

	s := strings.Split(string(data), "\n")
	var lines []string
	lines = append(lines, ignoreDefaults...)
	lines = append(lines, s...)
	return gitignore.CompileIgnoreLines(lines...), true
}

// Refresh updates the hashes of all the files in the index, and adds new files to the index
func (in *Index) Refresh() error {
	// TODO: If needed, multithreaded hashing
	// for i := 0; i < runtime.NumCPU(); i++ {}

	// Load the sync lock (if the pack declares one). Files that end up outside of the locked
	// folders are kept out of the index, and the index is the only thing packwiz-installer
	// downloads from - so this is what makes the installer refuse out-of-scope files.
	lock, err := LoadSyncLock()
	if err != nil {
		return err
	}
	in.LockViolations = nil
	if lock.IsActive() {
		in.Lock = &lock
	} else {
		in.Lock = nil
	}

	// Is case-sensitivity a problem?
	pathPF, _ := filepath.Abs(viper.GetString("pack-file"))
	pathIndex, _ := filepath.Abs(in.indexFile)

	pathIgnore, _ := filepath.Abs(filepath.Join(in.packRoot, ".packwizignore"))
	ignore, ignoreExists := readGitignore(pathIgnore)

	var fileList []string
	err = filepath.WalkDir(in.packRoot, func(path string, info os.DirEntry, err error) error {
		if err != nil {
			// TODO: Handle errors on individual files properly
			return err
		}

		// Never ignore pack root itself (gitignore doesn't allow ignoring the root)
		if path == in.packRoot {
			return nil
		}

		if info.IsDir() {
			// Don't traverse ignored directories (consistent with Git handling of ignored dirs)
			if ignore.MatchesPath(path) {
				return fs.SkipDir
			}
			// Prune whole folders that cannot contain any allowed file
			if in.Lock != nil {
				if relPath, relErr := in.RelIndexPath(path); relErr == nil && !in.Lock.CouldContain(relPath) {
					if in.lockReject(SyncViolation{
						Path:   relPath + "/",
						Reason: "folder is outside of the locked folders",
					}) {
						return fs.SkipDir
					}
				}
			}
			// Don't add directories to the file list
			return nil
		}
		// Exit if the files are the same as the pack/index files
		absPath, _ := filepath.Abs(path)
		if absPath == pathPF || absPath == pathIndex {
			return nil
		}
		if ignoreExists {
			if absPath == pathIgnore {
				return nil
			}
		}
		if ignore.MatchesPath(path) {
			return nil
		}

		// Enforce the sync lock before the file is allowed into the index
		if in.Lock != nil {
			relPath, relErr := in.RelIndexPath(path)
			if relErr != nil {
				relPath = path
			}
			if lockErr := in.Lock.Check(relPath); lockErr != nil {
				if in.lockReject(SyncViolation{Path: relPath, Reason: lockErr.Error()}) {
					return nil
				}
			}
			if strings.HasSuffix(filepath.Base(path), MetaExtension) {
				dest, destErr := in.lockCheckMetafile(relPath, path)
				if destErr != nil {
					violation := SyncViolation{Path: relPath, Reason: destErr.Error()}
					if dest != "" && dest != relPath {
						violation.Dest = dest
					}
					if in.lockReject(violation) {
						return nil
					}
				}
			}
		}

		fileList = append(fileList, path)
		return nil
	})
	if err != nil {
		return err
	}

	progressContainer := mpb.New()
	progress := progressContainer.AddBar(int64(len(fileList)),
		mpb.PrependDecorators(
			// simple name decorator
			decor.Name("Refreshing index..."),
			// decor.DSyncWidth bit enables column width synchronization
			decor.Percentage(decor.WCSyncSpace),
		),
		mpb.AppendDecorators(
			// replace ETA decorator with "done" message, OnComplete event
			decor.OnComplete(
				// ETA decorator with ewma age of 60
				decor.EwmaETA(decor.ET_STYLE_GO, 60), "done",
			),
		),
	)

	for _, v := range fileList {
		start := time.Now()

		err := in.updateFile(v)
		if err != nil {
			return err
		}

		progress.Increment(time.Since(start))
	}
	// Close bar
	progress.SetTotal(int64(len(fileList)), true) // If len = 0, we have to manually set complete to true
	progressContainer.Wait()

	// Check all the files exist, remove them if they don't
	for p, file := range in.Files {
		if !file.markedFound() {
			delete(in.Files, p)
		}
	}

	// Final safety net: re-validate every entry that is going into the index, including the
	// aliases, which packwiz-installer uses as the destination path of a file.
	in.pruneLockViolations()
	in.reportLockViolations()

	return nil
}

// lockReject records a sync lock violation and reports whether the file has to be kept out of
// the index. In "warn" mode the file is still indexed, but is always reported.
func (in *Index) lockReject(v SyncViolation) bool {
	in.LockViolations = append(in.LockViolations, v)
	return in.Lock != nil && in.Lock.ModeValue() != SyncModeWarn
}

// lockCheckMetafile verifies that a metadata file, and the file it installs, are both inside
// the locked folders. This is the check that stops a metadata file declaring a filename such
// as "../../options.txt" from writing outside of the locked folders.
func readMetaFilenameLocal(absPath string) (string, error) {
	var meta struct {
		Filename string `toml:"filename"`
	}
	if _, err := toml.DecodeFile(absPath, &meta); err != nil {
		return "", fmt.Errorf("could not read metadata file: %w", err)
	}
	return meta.Filename, nil
}

func (in *Index) lockCheckMetafile(relPath string, absPath string) (string, error) {
	filename, err := readMetaFilenameLocal(absPath)
	if err != nil {
		return "", err
	}
	return in.Lock.CheckMetaDest(relPath, filename)
}

// prunedCopy returns a copy of the index with every entry that the lock rejects removed,
// together with the list of what was removed. It never modifies the receiver, which makes
// it safe to use on the write path as well as during a refresh.
func (in Index) prunedCopy(lock SyncLock) (Index, []SyncViolation) {
	out := in
	out.Files = make(IndexFiles, len(in.Files))
	violations := []SyncViolation{}
	for p, holder := range in.Files {
		if err := lock.Check(string(p)); err != nil {
			violations = append(violations, SyncViolation{Path: string(p), Reason: err.Error()})
			continue
		}
		aliasReason := ""
		for _, alias := range holder.getAliases() {
			if err := lock.Check(alias); err != nil {
				aliasReason = fmt.Sprintf("alias %q: %s", alias, err)
				break
			}
		}
		if aliasReason != "" {
			violations = append(violations, SyncViolation{Path: string(p), Reason: aliasReason})
			continue
		}
		if holder.IsMetaFile() {
			filename, err := readMetaFilenameLocal(in.ResolveIndexPath(string(p)))
			if err != nil {
				violations = append(violations, SyncViolation{Path: string(p), Reason: err.Error()})
				continue
			}
			dest, err := lock.CheckMetaDest(string(p), filename)
			if err != nil {
				violations = append(violations, SyncViolation{Path: string(p), Dest: dest, Reason: err.Error()})
				continue
			}
		}
		out.Files[p] = holder
	}
	return out, violations
}

// pruneLockViolations double checks every entry that is still in the index, including aliases.
func (in *Index) pruneLockViolations() {
	if in.Lock == nil || in.Lock.ModeValue() == SyncModeWarn {
		return
	}
	pruned, violations := in.prunedCopy(*in.Lock)
	in.Files = pruned.Files
	in.LockViolations = append(in.LockViolations, violations...)
}

// reportLockViolations prints a summary of everything the sync lock rejected.
func (in *Index) reportLockViolations() {
	if in.Lock == nil || len(in.LockViolations) == 0 {
		return
	}
	lock := *in.Lock
	if lock.ModeValue() == SyncModeWarn {
		fmt.Printf("Warning: %d path(s) are outside of the locked folders but will still be synced (mode = %q)\n",
			len(in.LockViolations), lock.ModeValue())
	} else {
		fmt.Printf("Sync lock: %s\n", lock.SyncLockDescription())
		fmt.Printf("Excluded %d path(s) from the index:\n", len(in.LockViolations))
	}
	PrintSyncViolations(in.LockViolations, 25)
}

// HasStrictLockFailure reports whether the sync lock rejected something while running in
// "deny" mode, in which case the calling command should exit with a non-zero status.
func (in Index) HasStrictLockFailure() bool {
	return in.Lock != nil && in.Lock.ModeValue() == SyncModeDeny && len(in.LockViolations) > 0
}

// CheckLock validates the whole index against a lock and returns every violation it finds.
// Unlike Refresh it never modifies the index, which makes it usable as a read-only gate
// (for example on a client that is about to hand the pack to packwiz-installer).
func (in Index) CheckLock(lock SyncLock) []SyncViolation {
	violations := []SyncViolation{}
	if !lock.IsActive() {
		return violations
	}
	paths := make([]string, 0, len(in.Files))
	for p := range in.Files {
		paths = append(paths, string(p))
	}
	sort.Strings(paths)

	for _, p := range paths {
		file := in.Files[p]
		if err := lock.Check(p); err != nil {
			violations = append(violations, SyncViolation{Path: p, Reason: err.Error()})
			continue
		}
		for _, alias := range file.getAliases() {
			if err := lock.Check(alias); err != nil {
				violations = append(violations, SyncViolation{
					Path:   p,
					Reason: fmt.Sprintf("alias %q: %s", alias, err),
				})
			}
		}
		if !file.IsMetaFile() {
			continue
		}
		var meta struct {
			Filename string `toml:"filename"`
		}
		if _, err := toml.DecodeFile(in.ResolveIndexPath(p), &meta); err != nil {
			violations = append(violations, SyncViolation{
				Path:   p,
				Reason: fmt.Sprintf("could not read metadata file: %s", err),
			})
			continue
		}
		dest, err := lock.CheckMetaDest(p, meta.Filename)
		if err != nil {
			violations = append(violations, SyncViolation{Path: p, Dest: dest, Reason: err.Error()})
		}
	}
	return violations
}

// enforceLockOnWrite makes sure that an index which reaches the disk never points outside of
// the locked folders.
//
// Refresh already enforces the lock, but a number of commands (modrinth add, url add, update,
// pin, remove, ...) write the index straight back out without refreshing first. Enforcing here
// as well means that none of them can publish an out-of-scope index by accident - which is
// exactly how files outside of the lock used to end up in front of packwiz-installer.
func (in Index) enforceLockOnWrite() Index {
	lock, err := LoadSyncLock()
	if err != nil || !lock.IsActive() || lock.ModeValue() == SyncModeWarn {
		return in
	}
	enforced, violations := in.prunedCopy(lock)
	if len(violations) == 0 {
		return in
	}
	enforced.Lock = &lock
	enforced.LockViolations = append(append([]SyncViolation{}, in.LockViolations...), violations...)

	fmt.Printf("Sync lock: %s\n", lock.SyncLockDescription())
	if len(in.LockViolations) > 0 {
		fmt.Printf("Excluded %d more path(s) while writing the index:\n", len(violations))
	} else {
		fmt.Printf("Excluded %d path(s) from the index:\n", len(violations))
	}
	PrintSyncViolations(violations, 15)
	return enforced
}

// PrintSyncViolations prints a capped list of lock violations.
func PrintSyncViolations(violations []SyncViolation, maxPrinted int) {
	for i, v := range violations {
		if i >= maxPrinted {
			fmt.Printf("  ... and %d more (run 'packwiz lock show' for the configuration)\n", len(violations)-maxPrinted)
			break
		}
		label := v.Path
		if v.Dest != "" {
			label = fmt.Sprintf("%s -> %s", v.Path, v.Dest)
		}
		fmt.Printf("  - %s (%s)\n", label, v.Reason)
	}
}

// Write saves the index file
func (in Index) Write() error {
	in = in.enforceLockOnWrite()

	// Convert to indexTomlRepresentation
	rep := indexTomlRepresentation{
		HashFormat: in.HashFormat,
		Files:      in.Files.toTomlRep(),
	}

	// TODO: calculate and provide hash while writing?
	f, err := os.Create(in.indexFile)
	if err != nil {
		return err
	}

	enc := toml.NewEncoder(f)
	// Disable indentation
	enc.Indent = ""
	err = enc.Encode(rep)
	if err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// RefreshFileWithHash updates a file in the index, given a file hash and whether it should be marked as metafile or not
func (in *Index) RefreshFileWithHash(path, format, hash string, markAsMetaFile bool) error {
	if viper.GetBool("no-internal-hashes") {
		hash = ""
	}
	return in.updateFileHashGiven(path, format, hash, markAsMetaFile)
}

// FindMod finds a mod in the index and returns its path and whether it has been found
func (in Index) FindMod(modName string) (string, bool) {
	for p, v := range in.Files {
		if v.IsMetaFile() {
			_, fileName := path.Split(p)
			fileTrimmed := strings.TrimSuffix(strings.TrimSuffix(fileName, MetaExtension), MetaExtensionOld)
			if fileTrimmed == modName {
				return in.ResolveIndexPath(p), true
			}
		}
	}
	return "", false
}

// getAllMods finds paths to every metadata file (Mod) in the index
func (in Index) getAllMods() []string {
	var list []string
	for p, v := range in.Files {
		if v.IsMetaFile() {
			list = append(list, in.ResolveIndexPath(p))
		}
	}
	return list
}

// LoadAllMods reads all metadata files into Mod structs
func (in Index) LoadAllMods() ([]*Mod, error) {
	modPaths := in.getAllMods()
	mods := make([]*Mod, len(modPaths))
	for i, v := range modPaths {
		modData, err := LoadMod(v)
		if err != nil {
			return nil, fmt.Errorf("failed to read metadata file %s: %w", v, err)
		}
		mods[i] = &modData
	}
	return mods, nil
}

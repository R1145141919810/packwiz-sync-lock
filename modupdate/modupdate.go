// Package modupdate adds "packwiz mod update": it asks Modrinth/CurseForge for the newest
// full release of each mod, replaces the file on disk, and rewrites the metadata, the index
// and a lock file recording what was chosen and why.
package modupdate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/packwiz/packwiz/cfapi"
	"github.com/packwiz/packwiz/cmd"
	"github.com/packwiz/packwiz/core"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// modLockFileName records which version every mod was updated to, and why.
const modLockFileName = "packwiz-mod-lock.toml"

var (
	modUpdateAll             bool
	modUpdateDryRun          bool
	modUpdateYes             bool
	modUpdateMetadata        bool
	modUpdateNoBackup        bool
	modUpdateBackupDir       string
	modUpdateTimeout         int
	modUpdatePreferVersion   bool
	modUpdateIncludeDisabled bool
	modUpdateNoLock          bool
	modUpdateNoLockCheck     bool
	modUpdateForce           bool
)

// errVerification marks a download that did not match the hash the host published. That is a
// hard stop: continuing would mean trusting a host that just served us the wrong bytes.
var errVerification = errors.New("verification failed")

var modUpdateCmd = &cobra.Command{
	Use:     "update [names...]",
	Aliases: []string{"upgrade", "pull"},
	Short:   "Update mods to their newest full release and replace the files on disk",
	Long: "Check every named mod (or all of them with --all) for a newer version on the host it\n" +
		"came from, download it, replace the file in the pack, and rewrite the metadata and\n" +
		"index.toml so clients pick the new version up.\n\n" +
		"This goes one step further than 'packwiz update', which only rewrites metadata: the new\n" +
		"file really is downloaded and put in place, which is what a server needs.\n\n" +
		"What counts as an update:\n" +
		"  - only versions the author marked as a full release are considered (no betas, alphas\n" +
		"    or pre-releases), unless the project has no release for this pack at all;\n" +
		"  - a version that would move the mod backwards is held back;\n" +
		"  - files are skipped when the metadata marks them as disabled (.jar.dis / .disabled).\n\n" +
		"Safety:\n" +
		"  - the new file is downloaded to a temporary file and verified against the hash the\n" +
		"    host published (sha512, sha256 or sha1) before anything is replaced;\n" +
		"  - a verification failure stops the run instead of being papered over;\n" +
		"  - the file it replaces is moved to .packwiz-backup/<timestamp>/ instead of deleted;\n" +
		"  - any other failure restores both the old file and the old metadata;\n" +
		"  - nothing outside of the [sync] lock is ever written;\n" +
		"  - --dry-run prints the plan and changes nothing.\n\n" +
		"The chosen version and the reason are written to packwiz-mod-lock.toml; the next run\n" +
		"checks the files against it and reports (and skips) anything that drifted.\n\n" +
		"Nothing else is touched: the Minecraft version, the loader and the rest of the pack stay\n" +
		"as they are, and dependencies are not added automatically.",
	Args: cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, args []string) {
		runModUpdate(args)
	},
}

// pendingUpdate is one mod that has a newer version available.
type pendingUpdate struct {
	Mod     *core.Mod
	Updater string
	Check   core.UpdateCheck
}

// backupEntry remembers where a replaced file was moved to.
type backupEntry struct {
	original string
	backup   string
}

type applyOptions struct {
	MetadataOnly bool
	NoBackup     bool
	BackupRoot   string
	PackRoot     string
	Timeout      time.Duration
}

// modLockEntry is what packwiz-mod-lock.toml records for one mod.
type modLockEntry struct {
	Source      string "toml:\"source\""
	VersionID   string "toml:\"version-id,omitempty\""
	Version     string "toml:\"version,omitempty\""
	ReleaseType string "toml:\"release-type,omitempty\""
	FileName    string "toml:\"filename\""
	HashFormat  string "toml:\"hash-format\""
	Hash        string "toml:\"hash\""
	SelectedBy  string "toml:\"selected-by\""
	Updated     string "toml:\"updated\""
}

type modLock struct {
	Version int                     "toml:\"version\""
	Mods    map[string]modLockEntry "toml:\"mods\""
}

// checkSummary is everything checkUpdates found.
type checkSummary struct {
	Pending     []pendingUpdate
	UpToDate    int
	Pinned      int
	Skipped     int
	Disabled    int
	HeldBack    []string
	CheckErrors []string
}

func runModUpdate(args []string) {
	if modUpdateAll && len(args) > 0 {
		fmt.Println("--all cannot be combined with explicit mod names.")
		os.Exit(1)
	}
	if !modUpdateAll && len(args) == 0 {
		fmt.Println("Name at least one mod, or pass --all to update every mod with a supported source.")
		fmt.Println("Tip: 'packwiz mod update <name> --dry-run' shows what would change.")
		os.Exit(1)
	}
	if modUpdatePreferVersion {
		viper.Set("mod-update.prefer-version-number", true)
	}

	fmt.Println("Loading modpack...")
	pack, err := core.LoadPack()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	index, err := pack.LoadIndex()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	lock, err := core.LoadSyncLock()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	packFile := viper.GetString("pack-file")
	if strings.TrimSpace(packFile) == "" {
		packFile = "pack.toml"
	}
	packRoot := filepath.Dir(packFile)

	mods, err := selectMods(&index, args)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if len(mods) == 0 {
		fmt.Println("No matching metadata files were found in the pack.")
		os.Exit(1)
	}

	// Compare the files on disk with the version lock before planning anything, so a file that
	// was swapped by hand outside of this tool cannot silently be carried along.
	lockFile, lockErr := loadModLock(packRoot)
	if lockErr != nil {
		fmt.Println("Warning: could not read " + modLockFileName + ": " + lockErr.Error())
	}
	skipDrifted := make(map[string]bool)
	if !modUpdateNoLockCheck && len(lockFile.Mods) > 0 {
		drifted, checked := checkModLock(&index, lockFile)
		if len(drifted) > 0 {
			fmt.Printf("Lock check: %d mod(s) no longer match %s:\n", len(drifted), modLockFileName)
			for _, entry := range drifted {
				fmt.Println("  - " + entry)
			}
			if modUpdateForce {
				fmt.Println("  --force given, so they will be updated anyway.")
			} else {
				fmt.Println("  These are skipped; pass --force to update them anyway.")
				for _, entry := range drifted {
					skipDrifted[lockEntryName(entry)] = true
				}
			}
		} else if checked > 0 {
			fmt.Printf("Lock check: %d mod(s) still match %s\n", checked, modLockFileName)
		}
	}

	if len(skipDrifted) > 0 {
		kept := mods[:0]
		for _, mod := range mods {
			if skipDrifted[lockKey(mod)] {
				continue
			}
			kept = append(kept, mod)
		}
		mods = kept
		if len(mods) == 0 {
			fmt.Println("Every selected mod was skipped by the lock check; nothing to do.")
			return
		}
	}

	fmt.Printf("Checking %d mod(s) for updates...\n", len(mods))
	summary := checkUpdates(mods, pack)

	for _, message := range summary.CheckErrors {
		fmt.Println("  " + message)
	}
	if len(summary.Pending) == 0 {
		fmt.Println()
		fmt.Printf("Nothing to update: %d already up to date, %d pinned, %d disabled, %d held back.\n",
			summary.UpToDate, summary.Pinned, summary.Disabled, len(summary.HeldBack))
		if len(summary.CheckErrors) > 0 {
			fmt.Printf("%d check(s) failed; see above.\n", len(summary.CheckErrors))
			os.Exit(1)
		}
		return
	}

	sort.Slice(summary.Pending, func(i, j int) bool {
		return summary.Pending[i].Mod.Name < summary.Pending[j].Mod.Name
	})
	fmt.Println()
	fmt.Println("Update plan (full releases only):")
	for _, item := range summary.Pending {
		fmt.Printf("  %-32s %s\n", item.Mod.Name, item.Check.UpdateString)
	}
	if len(summary.HeldBack) > 0 {
		fmt.Println()
		fmt.Printf("Held back: %d\n", len(summary.HeldBack))
		for _, message := range summary.HeldBack {
			fmt.Println("  - " + message)
		}
	}
	if summary.Disabled > 0 {
		fmt.Println()
		fmt.Printf("Skipped because the file is disabled: %d (use --include-disabled to update them)\n", summary.Disabled)
	}
	fmt.Println()
	fmt.Printf("Up to date: %d, pinned: %d, skipped: %d\n", summary.UpToDate, summary.Pinned, summary.Skipped)

	if modUpdateDryRun {
		fmt.Println()
		fmt.Println("Dry run: nothing was downloaded or written.")
		return
	}

	if !modUpdateYes {
		fmt.Println()
		fmt.Println("The files above will be replaced in place, and the old ones kept in a backup folder.")
		if !confirm("Apply these updates? [y/N]: ") {
			fmt.Println("Cancelled; nothing was written.")
			return
		}
	}

	backupRoot := filepath.Join(packRoot, modUpdateBackupDir, time.Now().Format("20060102-150405"))
	if modUpdateNoBackup {
		backupRoot = ""
	}
	opts := applyOptions{
		MetadataOnly: modUpdateMetadata,
		NoBackup:     modUpdateNoBackup,
		BackupRoot:   backupRoot,
		PackRoot:     packRoot,
		Timeout:      time.Duration(modUpdateTimeout) * time.Second,
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if !opts.MetadataOnly && backupRoot != "" {
		// Keep the pack manifest next to the replaced jars, so a manual rollback has everything.
		for _, name := range []string{packFile, index.IndexFilePath()} {
			if err := copyToBackup(name, opts); err != nil {
				fmt.Printf("Warning: could not back up %s: %s\n", name, err)
			}
		}
	}

	fmt.Println()
	updated, failures := 0, 0
	lockEntries := make(map[string]modLockEntry)

	persist := func() {
		if updated == 0 {
			return
		}
		if err := index.Write(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if err := pack.UpdateIndexHash(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if err := pack.Write(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if modUpdateNoLock || len(lockEntries) == 0 {
			return
		}
		if lockFile.Mods == nil {
			lockFile.Mods = make(map[string]modLockEntry)
		}
		for name, entry := range lockEntries {
			lockFile.Mods[name] = entry
		}
		lockFile.Version = 1
		if err := saveModLock(packRoot, lockFile); err != nil {
			fmt.Println("Warning: could not write " + modLockFileName + ": " + err.Error())
		}
	}

	for _, item := range summary.Pending {
		message, entry, applyErr := applyUpdate(item, &index, lock, opts)
		if applyErr != nil {
			failures++
			fmt.Printf("  FAILED %s: %s\n", item.Mod.Name, applyErr)
			if errors.Is(applyErr, errVerification) {
				persist()
				fmt.Println()
				fmt.Println("Verification failed, so the run stops here rather than carrying on with a host")
				fmt.Println("that did not serve what it promised. Everything above was verified and is kept.")
				os.Exit(1)
			}
			continue
		}
		updated++
		if entry != nil {
			lockEntries[lockKey(item.Mod)] = *entry
		}
		fmt.Printf("  %s: %s\n", item.Mod.Name, message)
	}
	persist()

	fmt.Println()
	fmt.Printf("Updated %d mod(s), %d failed.\n", updated, failures)
	if failures == 0 && len(summary.CheckErrors) > 0 {
		fmt.Printf("%d check(s) failed; see above.\n", len(summary.CheckErrors))
	}
	if updated > 0 {
		if opts.MetadataOnly {
			fmt.Println("Only metadata was written (--metadata-only). Run 'packwiz refresh' to reconcile")
			fmt.Println("index.toml with the files on disk.")
		} else {
			if !modUpdateNoBackup {
				fmt.Println("Replaced files were moved to " + backupRoot)
			}
			if !modUpdateNoLock {
				fmt.Println("Chosen versions were recorded in " + modLockFileName + ".")
			}
			fmt.Println("Restart the server (or Minecraft) so it loads the new jars.")
		}
	}
	if failures > 0 {
		os.Exit(1)
	}
}

// selectMods resolves the requested names, or loads every metadata file with --all.
func selectMods(index *core.Index, names []string) ([]*core.Mod, error) {
	if modUpdateAll {
		fmt.Println("Reading metadata files...")
		return index.LoadAllMods()
	}
	mods := make([]*core.Mod, 0, len(names))
	for _, name := range names {
		modPath, ok := index.FindMod(name)
		if !ok {
			return nil, fmt.Errorf("can't find %q in the pack; run 'packwiz refresh' first, and use the name of the .pw.toml file (usually the project slug)", name)
		}
		mod, err := core.LoadMod(modPath)
		if err != nil {
			return nil, fmt.Errorf("could not read %s: %w", modPath, err)
		}
		mods = append(mods, &mod)
	}
	return mods, nil
}

// checkUpdates asks each mod's host for the newest version.
func checkUpdates(mods []*core.Mod, pack core.Pack) checkSummary {
	summary := checkSummary{}
	groups := make(map[string][]*core.Mod)
	order := make([]string, 0)
	for _, mod := range mods {
		if isDisabledFile(mod.FileName) && !modUpdateIncludeDisabled {
			summary.Disabled++
			continue
		}
		updaterName := ""
		for name := range mod.Update {
			if _, ok := core.Updaters[name]; ok {
				updaterName = name
				break
			}
		}
		if updaterName == "" {
			summary.Skipped++
			summary.CheckErrors = append(summary.CheckErrors, fmt.Sprintf("skipped %s: no supported update source", displayName(mod)))
			continue
		}
		if _, seen := groups[updaterName]; !seen {
			order = append(order, updaterName)
		}
		groups[updaterName] = append(groups[updaterName], mod)
	}
	sort.Strings(order)

	for _, name := range order {
		group := groups[name]
		checks, err := core.Updaters[name].CheckUpdate(group, pack)
		if err != nil {
			summary.CheckErrors = append(summary.CheckErrors, fmt.Sprintf("could not check %s mods: %s", name, err))
			continue
		}
		for i, check := range checks {
			mod := group[i]
			switch {
			case check.Error != nil:
				summary.CheckErrors = append(summary.CheckErrors, fmt.Sprintf("%s: %s", displayName(mod), check.Error))
			case mod.Pin:
				summary.Pinned++
			case check.SkippedReason != "":
				summary.HeldBack = append(summary.HeldBack, fmt.Sprintf("%s: %s", displayName(mod), check.SkippedReason))
			case !check.UpdateAvailable:
				summary.UpToDate++
			default:
				summary.Pending = append(summary.Pending, pendingUpdate{Mod: mod, Updater: name, Check: check})
			}
		}
	}
	return summary
}

// applyUpdate carries out one update: download, replace, rewrite metadata, index and lock.
func applyUpdate(item pendingUpdate, index *core.Index, lock core.SyncLock, opts applyOptions) (string, *modLockEntry, error) {
	mod := item.Mod
	metaPath := mod.GetFilePath()
	oldDest := mod.GetDestFilePath()

	metaSnapshot, snapshotErr := os.ReadFile(metaPath)

	if err := core.Updaters[item.Updater].DoUpdate([]*core.Mod{mod}, []interface{}{item.Check.CachedState}); err != nil {
		return "", nil, err
	}
	newDest := mod.GetDestFilePath()

	if lock.IsActive() {
		metaRel, relErr := index.RelIndexPath(metaPath)
		if relErr != nil {
			return "", nil, relErr
		}
		if err := lock.Check(metaRel); err != nil {
			return "", nil, err
		}
		if _, err := lock.CheckMetaDest(metaRel, mod.FileName); err != nil {
			return "", nil, err
		}
	}

	var backups []backupEntry
	placedPath := ""
	restore := func() {
		// Remove the file this run put in place first, otherwise a failed update would leave
		// two versions of the same mod in the mods folder.
		if placedPath != "" {
			_ = os.Remove(placedPath)
			placedPath = ""
		}
		for _, entry := range backups {
			if entry.backup == "" {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(entry.original), 0755)
			_ = os.Rename(entry.backup, entry.original)
		}
		backups = nil
		if snapshotErr == nil {
			_ = os.WriteFile(metaPath, metaSnapshot, 0644)
		}
	}

	if !opts.MetadataOnly {
		replacedBackups, replacedPath, err := replaceFile(mod, oldDest, newDest, opts)
		backups = replacedBackups
		placedPath = replacedPath
		if err != nil {
			restore()
			return "", nil, err
		}
	}

	format, hash, err := mod.Write()
	if err != nil {
		restore()
		return "", nil, err
	}
	if err := index.RefreshFileWithHash(metaPath, format, hash, true); err != nil {
		restore()
		return "", nil, err
	}

	if !opts.MetadataOnly {
		if err := index.UpdateFileEntry(newDest); err != nil {
			restore()
			return "", nil, err
		}
		if oldDest != newDest {
			_ = index.RemoveFile(oldDest)
		}
	}

	message := "metadata updated"
	var lockEntry *modLockEntry
	if !opts.MetadataOnly {
		message = fmt.Sprintf("%s -> %s", filepath.Base(oldDest), filepath.Base(newDest))
		if oldDest == newDest {
			message = "replaced " + filepath.Base(newDest)
		}
		entry := buildLockEntry(mod, item)
		lockEntry = &entry
	}
	return message, lockEntry, nil
}

// buildLockEntry records what was chosen and why.
func buildLockEntry(mod *core.Mod, item pendingUpdate) modLockEntry {
	entry := modLockEntry{
		Source:      item.Updater,
		Version:     item.Check.NewVersion,
		ReleaseType: item.Check.NewVersionType,
		FileName:    mod.FileName,
		HashFormat:  mod.Download.HashFormat,
		Hash:        mod.Download.Hash,
		Updated:     time.Now().UTC().Format(time.RFC3339),
	}
	if entry.ReleaseType == "" {
		entry.ReleaseType = "release"
	}
	if modUpdatePreferVersion {
		entry.SelectedBy = "highest version number"
	} else {
		entry.SelectedBy = "newest full release"
	}
	if id, ok := stringUpdateField(mod, item.Updater, "version"); ok {
		entry.VersionID = id
	} else if fileID, ok := numericUpdateField(mod, item.Updater, "file-id"); ok {
		entry.VersionID = fmt.Sprintf("%d", fileID)
	}
	return entry
}

// replaceFile downloads the new version into place, keeping a backup of what it replaces.
// It returns the backups it made and the path of the file it put in place, so the caller can
// undo everything if a later step fails.
func replaceFile(mod *core.Mod, oldDest, newDest string, opts applyOptions) ([]backupEntry, string, error) {
	source, description, err := openModSource(mod, opts.Timeout)
	if err != nil {
		return nil, "", err
	}
	defer source.Close()

	if err := os.MkdirAll(filepath.Dir(newDest), 0755); err != nil {
		return nil, "", err
	}
	tempFile, err := os.CreateTemp(filepath.Dir(newDest), ".packwiz-update-*.tmp")
	if err != nil {
		return nil, "", err
	}
	tempPath := tempFile.Name()
	discard := func() {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
	}

	format := mod.Download.HashFormat
	if strings.TrimSpace(format) == "" {
		format = "sha256"
	}
	hasher, err := core.GetHashImpl(format)
	if err != nil {
		discard()
		return nil, "", err
	}
	if _, err := io.Copy(io.MultiWriter(tempFile, hasher), source); err != nil {
		discard()
		return nil, "", fmt.Errorf("failed while downloading %s: %w", description, err)
	}
	actual := hasher.HashToString(hasher.Sum(nil))
	if mod.Download.Hash != "" && !strings.EqualFold(actual, mod.Download.Hash) {
		discard()
		return nil, "", fmt.Errorf("%w: %s served a file that hashes to %s %s, but the pack expects %s",
			errVerification, description, format, actual, mod.Download.Hash)
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return nil, "", err
	}

	backups := make([]backupEntry, 0, 2)
	backup := func(path string) error {
		if _, statErr := os.Stat(path); statErr != nil {
			return nil
		}
		entry, moveErr := moveToBackup(path, opts)
		if moveErr != nil {
			return moveErr
		}
		backups = append(backups, entry)
		return nil
	}

	if err := backup(oldDest); err != nil {
		_ = os.Remove(tempPath)
		return nil, "", err
	}
	if oldDest != newDest {
		if err := backup(newDest); err != nil {
			_ = os.Remove(tempPath)
			return backups, "", err
		}
	}
	if err := os.Rename(tempPath, newDest); err != nil {
		_ = os.Remove(tempPath)
		return backups, "", err
	}
	return backups, newDest, nil
}

// moveToBackup moves a replaced file into the backup folder.
func moveToBackup(path string, opts applyOptions) (backupEntry, error) {
	if opts.NoBackup || opts.BackupRoot == "" {
		if err := os.Remove(path); err != nil {
			return backupEntry{}, err
		}
		return backupEntry{original: path}, nil
	}
	dest, err := backupPathFor(path, opts)
	if err != nil {
		return backupEntry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return backupEntry{}, err
	}
	if err := os.Rename(path, dest); err != nil {
		return backupEntry{}, err
	}
	return backupEntry{original: path, backup: dest}, nil
}

// copyToBackup copies a file into the backup folder without moving it.
func copyToBackup(path string, opts applyOptions) error {
	if opts.NoBackup || opts.BackupRoot == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	dest, err := backupPathFor(path, opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return err
	}
	return target.Close()
}

func backupPathFor(path string, opts applyOptions) (string, error) {
	rel, err := filepath.Rel(opts.PackRoot, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(path)
	}
	return filepath.Join(opts.BackupRoot, rel), nil
}

// openModSource opens the download for the version the metadata now points at.
func openModSource(mod *core.Mod, timeout time.Duration) (io.ReadCloser, string, error) {
	mode := strings.TrimSpace(mod.Download.Mode)
	switch mode {
	case "", core.ModeURL:
		if strings.TrimSpace(mod.Download.URL) == "" {
			return nil, "", errors.New("the metadata file has no download URL")
		}
		source, err := core.OpenURLWithTimeout(mod.Download.URL, timeout)
		return source, mod.Download.URL, err
	case core.ModeCF:
		fileID, ok := numericUpdateField(mod, "curseforge", "file-id")
		if !ok {
			return nil, "", errors.New("the metadata file has no CurseForge file-id")
		}
		files, err := cfapi.ResolveFiles([]int{int(fileID)}, timeout)
		if err != nil {
			return nil, "", err
		}
		file, ok := files[int(fileID)]
		if !ok {
			return nil, "", fmt.Errorf("the CurseForge API did not return file %d", fileID)
		}
		if file.DownloadURL == nil || strings.TrimSpace(*file.DownloadURL) == "" {
			return nil, "", fmt.Errorf("CurseForge file %d has no download URL (the author disabled third party downloads)", fileID)
		}
		source, err := core.OpenURLWithTimeout(*file.DownloadURL, timeout)
		return source, *file.DownloadURL, err
	default:
		return nil, "", fmt.Errorf("download mode %q is not supported by this command", mode)
	}
}

// loadModLock reads packwiz-mod-lock.toml; a missing file is not an error.
func loadModLock(packRoot string) (modLock, error) {
	lock := modLock{Version: 1, Mods: make(map[string]modLockEntry)}
	path := filepath.Join(packRoot, modLockFileName)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return lock, nil
		}
		return lock, err
	}
	if _, err := toml.DecodeFile(path, &lock); err != nil {
		return lock, err
	}
	if lock.Mods == nil {
		lock.Mods = make(map[string]modLockEntry)
	}
	return lock, nil
}

// saveModLock writes packwiz-mod-lock.toml in a stable order.
func saveModLock(packRoot string, lock modLock) error {
	var builder strings.Builder
	builder.WriteString("# Written by 'packwiz mod update'. Records the version each mod was updated to and why,\n")
	builder.WriteString("# so a bad pick can be traced and rolled back. Not synced to clients. Do not edit.\n\n")
	builder.WriteString("version = 1\n")

	names := make([]string, 0, len(lock.Mods))
	for name := range lock.Mods {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		entry := lock.Mods[name]
		builder.WriteString("\n[mods." + quoteTomlKey(name) + "]\n")
		writeLockLine(&builder, "source", entry.Source)
		writeLockLine(&builder, "version-id", entry.VersionID)
		writeLockLine(&builder, "version", entry.Version)
		writeLockLine(&builder, "release-type", entry.ReleaseType)
		writeLockLine(&builder, "filename", entry.FileName)
		writeLockLine(&builder, "hash-format", entry.HashFormat)
		writeLockLine(&builder, "hash", entry.Hash)
		writeLockLine(&builder, "selected-by", entry.SelectedBy)
		writeLockLine(&builder, "updated", entry.Updated)
	}
	return os.WriteFile(filepath.Join(packRoot, modLockFileName), []byte(builder.String()), 0644)
}

func writeLockLine(builder *strings.Builder, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	builder.WriteString(key + " = " + quoteTomlKey(value) + "\n")
}

// quoteTomlKey turns a string into a safely quoted TOML key or value.
func quoteTomlKey(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			builder.WriteString("\\\"")
		case '\\':
			builder.WriteString("\\\\")
		case '\n':
			builder.WriteString("\\n")
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// checkModLock compares the files on disk with what the lock file recorded.
func checkModLock(index *core.Index, lock modLock) (drifted []string, checked int) {
	for name, entry := range lock.Mods {
		metaPath, ok := index.FindMod(name)
		if !ok || entry.FileName == "" || entry.Hash == "" {
			continue
		}
		dest := filepath.Join(filepath.Dir(metaPath), filepath.FromSlash(entry.FileName))
		if _, err := os.Stat(dest); err != nil {
			drifted = append(drifted, fmt.Sprintf("%s (%s is gone)", name, entry.FileName))
			continue
		}
		checked++
		matches, err := core.FileMatchesHash(dest, entry.HashFormat, entry.Hash)
		if err != nil || !matches {
			drifted = append(drifted, fmt.Sprintf("%s (changed since %s)", name, entry.Updated))
		}
	}
	sort.Strings(drifted)
	return drifted, checked
}

// lockEntryName pulls the mod name back out of a "name (reason)" drift message.
func lockEntryName(message string) string {
	if index := strings.Index(message, " ("); index > 0 {
		return message[:index]
	}
	return message
}

// lockKey is the name a mod is stored under in the lock file, and the name you pass on the
// command line.
func lockKey(mod *core.Mod) string {
	return strings.TrimSuffix(filepath.Base(mod.GetFilePath()), core.MetaExtension)
}

// isDisabledFile reports whether a metadata file points at a file the user disabled.
func isDisabledFile(fileName string) bool {
	lower := strings.ToLower(strings.TrimSpace(fileName))
	return strings.HasSuffix(lower, ".dis") || strings.HasSuffix(lower, ".disabled")
}

// numericUpdateField reads a numeric value out of a metadata file's [update.*] table.
func numericUpdateField(mod *core.Mod, updater, key string) (int64, bool) {
	table, ok := mod.Update[updater]
	if !ok {
		return 0, false
	}
	value, ok := table[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint32:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	case float64:
		return int64(typed), true
	}
	return 0, false
}

// stringUpdateField reads a string value out of a metadata file's [update.*] table.
func stringUpdateField(mod *core.Mod, updater, key string) (string, bool) {
	table, ok := mod.Update[updater]
	if !ok {
		return "", false
	}
	value := ""
	switch typed := table[key].(type) {
	case string:
		value = typed
	case *string:
		if typed != nil {
			value = *typed
		}
	default:
		return "", false
	}
	if strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func displayName(mod *core.Mod) string {
	if strings.TrimSpace(mod.Name) != "" {
		return mod.Name
	}
	return filepath.Base(mod.GetFilePath())
}

// confirm asks a yes/no question and treats an empty answer as "no".
func confirm(prompt string) bool {
	fmt.Print(prompt)
	var answer string
	_, _ = fmt.Scanln(&answer)
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

func init() {
	cmd.ModCmd.AddCommand(modUpdateCmd)

	modUpdateCmd.Flags().BoolVarP(&modUpdateAll, "all", "a", false, "Update every mod that has a supported source")
	modUpdateCmd.Flags().BoolVarP(&modUpdateDryRun, "dry-run", "n", false, "Only show the update plan, change nothing")
	modUpdateCmd.Flags().BoolVarP(&modUpdateYes, "yes", "y", false, "Do not ask for confirmation")
	modUpdateCmd.Flags().BoolVar(&modUpdateMetadata, "metadata-only", false, "Only rewrite metadata; do not download or replace files")
	modUpdateCmd.Flags().BoolVar(&modUpdateNoBackup, "no-backup", false, "Delete replaced files instead of keeping a backup")
	modUpdateCmd.Flags().StringVar(&modUpdateBackupDir, "backup-dir", ".packwiz-backup", "Where replaced files are kept, relative to the pack")
	modUpdateCmd.Flags().IntVar(&modUpdateTimeout, "timeout", 60, "How long a single download may take, in seconds")
	modUpdateCmd.Flags().BoolVar(&modUpdatePreferVersion, "prefer-version-number", false, "Pick the highest version number instead of the newest release (off by default)")
	modUpdateCmd.Flags().BoolVar(&modUpdateIncludeDisabled, "include-disabled", false, "Also update mods whose file is disabled (.jar.dis / .disabled)")
	modUpdateCmd.Flags().BoolVar(&modUpdateNoLock, "no-lock", false, "Do not write "+modLockFileName)
	modUpdateCmd.Flags().BoolVar(&modUpdateNoLockCheck, "no-lock-check", false, "Do not compare the files on disk with "+modLockFileName)
	modUpdateCmd.Flags().BoolVar(&modUpdateForce, "force", false, "Update even when the lock check reports that a file drifted")
}

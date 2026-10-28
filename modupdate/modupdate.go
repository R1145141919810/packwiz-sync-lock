// Package modupdate adds "packwiz mod update": it asks Modrinth/CurseForge for the newest
// version of each mod and replaces the file on disk, then rewrites the metadata and the index.
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

	"github.com/packwiz/packwiz/cfapi"
	"github.com/packwiz/packwiz/cmd"
	"github.com/packwiz/packwiz/core"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	modUpdateAll       bool
	modUpdateDryRun    bool
	modUpdateYes       bool
	modUpdateMetadata  bool
	modUpdateNoBackup  bool
	modUpdateBackupDir string
	modUpdateTimeout   int
)

var modUpdateCmd = &cobra.Command{
	Use:     "update [names...]",
	Aliases: []string{"upgrade", "pull"},
	Short:   "Update mods from Modrinth/CurseForge and replace the files on disk",
	Long: "Check every named mod (or all of them with --all) for a newer version on the host it\n" +
		"came from, download it, replace the file in the pack, and rewrite the metadata and\n" +
		"index.toml so clients pick the new version up.\n\n" +
		"This goes one step further than 'packwiz update', which only rewrites metadata: the new\n" +
		"file really is downloaded and put in place, which is what a server needs.\n\n" +
		"Safety:\n" +
		"  - the new file is downloaded to a temporary file and hash checked first;\n" +
		"  - the file it replaces is moved to .packwiz-backup/<timestamp>/ instead of deleted;\n" +
		"  - if anything fails, the old file and the old metadata are restored;\n" +
		"  - nothing outside of the [sync] lock is ever written;\n" +
		"  - --dry-run prints the plan and changes nothing.\n\n" +
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

	fmt.Printf("Checking %d mod(s) for updates...\n", len(mods))
	pending, upToDate, pinned, skipped, checkErrors := checkUpdates(mods, pack)

	for _, message := range checkErrors {
		fmt.Println("  " + message)
	}
	if len(pending) == 0 {
		fmt.Println()
		fmt.Printf("Nothing to update: %d already up to date, %d pinned, %d skipped.\n", upToDate, pinned, skipped)
		if len(checkErrors) > 0 {
			fmt.Printf("%d check(s) failed; see above.\n", len(checkErrors))
			os.Exit(1)
		}
		return
	}

	sort.Slice(pending, func(i, j int) bool { return pending[i].Mod.Name < pending[j].Mod.Name })
	fmt.Println()
	fmt.Println("Update plan:")
	for _, item := range pending {
		fmt.Printf("  %-32s %s\n", item.Mod.Name, item.Check.UpdateString)
	}
	fmt.Println()
	fmt.Printf("Up to date: %d, pinned: %d, skipped: %d\n", upToDate, pinned, skipped)

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

	fmt.Println()
	updated, failures := 0, 0
	for _, item := range pending {
		result, applyErr := applyUpdate(item, &index, lock, opts)
		if applyErr != nil {
			failures++
			fmt.Printf("  FAILED %s: %s\n", item.Mod.Name, applyErr)
			continue
		}
		updated++
		fmt.Printf("  %s: %s\n", item.Mod.Name, result)
	}

	if updated > 0 {
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
	}

	fmt.Println()
	fmt.Printf("Updated %d mod(s), %d failed.\n", updated, failures)
	if failures == 0 && len(checkErrors) > 0 {
		fmt.Printf("%d check(s) failed; see above.\n", len(checkErrors))
	}
	if updated > 0 {
		if opts.MetadataOnly {
			fmt.Println("Only metadata was written (--metadata-only). Run 'packwiz refresh' to reconcile")
			fmt.Println("index.toml with the files on disk.")
		} else if !modUpdateNoBackup {
			fmt.Println("Replaced files were moved to " + backupRoot)
			fmt.Println("Restart the server (or Minecraft) so it loads the new jars.")
		} else {
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
func checkUpdates(mods []*core.Mod, pack core.Pack) (pending []pendingUpdate, upToDate, pinned, skipped int, checkErrors []string) {
	groups := make(map[string][]*core.Mod)
	order := make([]string, 0)
	for _, mod := range mods {
		updaterName := ""
		for name := range mod.Update {
			if _, ok := core.Updaters[name]; ok {
				updaterName = name
				break
			}
		}
		if updaterName == "" {
			skipped++
			checkErrors = append(checkErrors, fmt.Sprintf("skipped %s: no supported update source", displayName(mod)))
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
			checkErrors = append(checkErrors, fmt.Sprintf("could not check %s mods: %s", name, err))
			continue
		}
		for i, check := range checks {
			mod := group[i]
			switch {
			case check.Error != nil:
				checkErrors = append(checkErrors, fmt.Sprintf("%s: %s", displayName(mod), check.Error))
			case mod.Pin:
				pinned++
			case !check.UpdateAvailable:
				upToDate++
			default:
				pending = append(pending, pendingUpdate{Mod: mod, Updater: name, Check: check})
			}
		}
	}
	return pending, upToDate, pinned, skipped, checkErrors
}

// applyUpdate carries out one update: download, replace, rewrite metadata and index.
func applyUpdate(item pendingUpdate, index *core.Index, lock core.SyncLock, opts applyOptions) (string, error) {
	mod := item.Mod
	metaPath := mod.GetFilePath()
	oldDest := mod.GetDestFilePath()

	metaSnapshot, snapshotErr := os.ReadFile(metaPath)

	if err := core.Updaters[item.Updater].DoUpdate([]*core.Mod{mod}, []interface{}{item.Check.CachedState}); err != nil {
		return "", err
	}
	newDest := mod.GetDestFilePath()

	if lock.IsActive() {
		metaRel, relErr := index.RelIndexPath(metaPath)
		if relErr != nil {
			return "", relErr
		}
		if err := lock.Check(metaRel); err != nil {
			return "", err
		}
		if _, err := lock.CheckMetaDest(metaRel, mod.FileName); err != nil {
			return "", err
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
			return "", err
		}
	}

	format, hash, err := mod.Write()
	if err != nil {
		restore()
		return "", err
	}
	if err := index.RefreshFileWithHash(metaPath, format, hash, true); err != nil {
		restore()
		return "", err
	}

	if !opts.MetadataOnly {
		if err := index.UpdateFileEntry(newDest); err != nil {
			restore()
			return "", err
		}
		if oldDest != newDest {
			_ = index.RemoveFile(oldDest)
		}
	}

	message := "metadata updated"
	if !opts.MetadataOnly {
		message = fmt.Sprintf("%s -> %s", filepath.Base(oldDest), filepath.Base(newDest))
		if oldDest == newDest {
			message = "replaced " + filepath.Base(newDest)
		}
	}
	return message, nil
}

// replaceFile downloads the new version into place, keeping a backup of what it replaces.
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
		return nil, "", fmt.Errorf("hash invalid: %s served a file that hashes to %s %s, but the pack expects %s",
			description, format, actual, mod.Download.Hash)
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
	rel, err := filepath.Rel(opts.PackRoot, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	dest := filepath.Join(opts.BackupRoot, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return backupEntry{}, err
	}
	if err := os.Rename(path, dest); err != nil {
		return backupEntry{}, err
	}
	return backupEntry{original: path, backup: dest}, nil
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
}

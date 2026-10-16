package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/packwiz/packwiz/core"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// lockCmd represents the lock command
var lockCmd = &cobra.Command{
	Use:   "lock",
	Short: "Lock the pack to a set of folders so that nothing outside of them can be synced",
	Long: "Lock the pack to a set of folders.\n\n" +
		"The lock is stored in the [sync] table of pack.toml, so it travels with the pack:\n\n" +
		"\t[sync]\n" +
		"\t# only the mods folder may be synced\n" +
		"\tallowed-paths = [\"mods\"]\n" +
		"\t# strip (default) removes out-of-scope files from the index, deny additionally\n" +
		"\t# fails the command, warn only reports them\n" +
		"\tmode = \"strip\"\n" +
		"\t# refuse paths that try to leave the pack folder (\"..\", absolute paths, C:)\n" +
		"\tblock-escapes = true\n\n" +
		"packwiz-installer builds the list of files it downloads from index.toml alone, so keeping\n" +
		"out-of-scope files out of index.toml is what makes the installer refuse to download them.",
}

var lockInitMode string
var lockInitNote string
var lockInitForce bool

var lockInitCmd = &cobra.Command{
	Use:   "init [folders...]",
	Short: "Write the [sync] table of pack.toml, locking the pack to the given folders (default: mods)",
	Args:  cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Loading modpack...")
		pack, err := core.LoadPack()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if pack.Sync != nil && !lockInitForce {
			fmt.Printf("The pack already locks %s; use --force to overwrite the lock.\n",
				strings.Join(pack.Sync.AllowedPaths, ", "))
			os.Exit(1)
		}

		folders := args
		if len(folders) == 0 {
			folders = []string{"mods"}
		}
		lock, err := core.SyncLock{
			AllowedPaths: folders,
			Mode:         lockInitMode,
			Note:         lockInitNote,
		}.Normalize()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		pack.Sync = &lock
		if err := pack.Write(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf("Sync lock saved: %s\n", lock.SyncLockDescription())

		fmt.Println("Refreshing the index to match the lock...")
		index, err := pack.LoadIndex()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if err := index.Refresh(); err != nil {
			fmt.Println(err)
			os.Exit(1)
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
		if index.HasStrictLockFailure() {
			fmt.Println("The sync lock is in deny mode and rejected the paths listed above; failing the build.")
			os.Exit(1)
		}
	},
}

var lockShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the sync lock configured in pack.toml",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		packFile := viper.GetString("pack-file")
		if strings.TrimSpace(packFile) == "" {
			packFile = "pack.toml"
		}
		lock, err := core.LoadSyncLock()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("Pack file: " + packFile)
		fmt.Println("Sync lock: " + lock.SyncLockDescription())
		if lock.Note != "" {
			fmt.Println("Note: " + lock.Note)
		}
		if lock.IsActive() {
			fmt.Println("Allowed folders:")
			for _, folder := range lock.AllowedPaths {
				fmt.Println("  - " + folder)
			}
			fmt.Println("Escape protection: " + enabledWord(lock.BlocksEscapes()))
			fmt.Println()
			fmt.Println("Files outside of these folders are removed from index.toml, and index.toml is")
			fmt.Println("the only file list packwiz-installer uses, so it never downloads them.")
		} else {
			fmt.Println()
			fmt.Println("This pack does not lock any folder, so every file in the pack may be synced.")
			fmt.Println("Run 'packwiz lock init mods' to lock the pack down to the mods folder.")
		}
		fmt.Println()
		fmt.Println("Run 'packwiz lock check' to verify that index.toml obeys the lock.")
	},
}

func enabledWord(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

var lockCheckURL string
var lockCheckJSON bool
var lockCheckQuiet bool

var lockCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Verify that an index only holds files inside the locked folders (exits non-zero on failure)",
	Long: "Verify that an index only holds files inside the locked folders.\n\n" +
		"Without --url the pack in the current directory is checked. With --url a pack.toml is\n" +
		"downloaded and checked remotely, which lets a client verify a pack before handing it to\n" +
		"packwiz-installer:\n\n" +
		"\tpackwiz lock check --url https://example.com/pack.toml || exit 1\n" +
		"\tjava -jar packwiz-installer-bootstrap.jar https://example.com/pack.toml\n\n" +
		"Every index entry, every alias and the destination declared by each metadata file is\n" +
		"checked, so a pack cannot smuggle in \"../../options.txt\" or an absolute path.",
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		if lockCheckURL != "" {
			runRemoteLockCheck(lockCheckURL)
		} else {
			runLocalLockCheck()
		}
	},
}

type lockCheckReport struct {
	OK         bool                 "json:\"ok\""
	Source     string               "json:\"source\""
	Checked    int                  "json:\"checked\""
	Lock       core.SyncLock        "json:\"lock\""
	Violations []core.SyncViolation "json:\"violations\""
}

func printLockCheckResult(lock core.SyncLock, source string, checked int, violations []core.SyncViolation) {
	if lockCheckJSON {
		payload := lockCheckReport{
			OK:         len(violations) == 0,
			Source:     source,
			Checked:    checked,
			Lock:       lock,
			Violations: violations,
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println(string(encoded))
	} else if !lockCheckQuiet {
		fmt.Println("Sync lock: " + lock.SyncLockDescription())
		fmt.Printf("Checked %d index entr%s of %s\n", checked, pluralY(checked), source)
		if len(violations) == 0 {
			fmt.Println("OK: everything in the index is inside the locked folders.")
		} else {
			fmt.Printf("FAILED: %d path(s) violate the lock:\n", len(violations))
			for _, v := range violations {
				label := v.Path
				if v.Dest != "" && v.Dest != v.Path {
					label = fmt.Sprintf("%s -> %s", v.Path, v.Dest)
				}
				fmt.Printf("  - %s (%s)\n", label, v.Reason)
			}
		}
	}

	if len(violations) > 0 {
		os.Exit(1)
	}
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func runLocalLockCheck() {
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
	violations := index.CheckLock(lock)
	source := viper.GetString("pack-file")
	if strings.TrimSpace(source) == "" {
		source = "pack.toml"
	}
	printLockCheckResult(lock, source, len(index.Files), violations)
}

type remotePackFile struct {
	Index struct {
		File string "toml:\"file\""
	} "toml:\"index\""
	Sync core.SyncLock "toml:\"sync\""
}

type remoteIndexFileEntry struct {
	File     string "toml:\"file\""
	Alias    string "toml:\"alias\""
	MetaFile bool   "toml:\"metafile\""
}

type remoteIndexFile struct {
	Files []remoteIndexFileEntry "toml:\"files\""
}

type remoteMetaFile struct {
	Filename string "toml:\"filename\""
}

func fetchPackTOML(u string, out interface{}) ([]byte, error) {
	resp, err := core.GetWithUA(u, "text/plain, application/toml, */*")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to fetch %s: HTTP %d", u, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", u, err)
	}
	if _, err := toml.Decode(string(raw), out); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", u, err)
	}
	return raw, nil
}

// resolvePackURL resolves a pack-relative path against the URL the pack.toml was fetched from.
func resolvePackURL(base *url.URL, rel string) (string, error) {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/")
	cleanBase := *base
	cleanBase.RawQuery = ""
	cleanBase.Fragment = ""
	ref := &url.URL{Path: rel}
	return cleanBase.ResolveReference(ref).String(), nil
}

func runRemoteLockCheck(packURL string) {
	base, err := url.Parse(packURL)
	if err != nil {
		fmt.Printf("Invalid pack URL: %s\n", err)
		os.Exit(1)
	}

	var packFile remotePackFile
	raw, err := fetchPackTOML(packURL, &packFile)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	var generic map[string]interface{}
	if _, err := toml.Decode(string(raw), &generic); err != nil {
		fmt.Printf("Failed to parse %s: %s\n", packURL, err)
		os.Exit(1)
	}
	lock, err := packFile.Sync.Normalize()
	if err != nil {
		fmt.Printf("%s: %s\n", packURL, err)
		os.Exit(1)
	}
	if _, hasSync := generic["sync"]; hasSync && len(lock.AllowedPaths) == 0 && lock.ModeValue() != core.SyncModeOff {
		fmt.Printf("%s: the [sync] table does not allow any folder; refusing to check an empty lock\n", packURL)
		os.Exit(1)
	}
	if !lock.IsActive() {
		if !lockCheckQuiet {
			fmt.Println("This pack does not declare a sync lock, so there is nothing to verify.")
		}
		printLockCheckResult(lock, packURL, 0, []core.SyncViolation{})
		return
	}

	indexFile := packFile.Index.File
	if strings.TrimSpace(indexFile) == "" {
		indexFile = "index.toml"
	}
	indexURL, err := resolvePackURL(base, indexFile)
	if err != nil {
		fmt.Printf("Failed to resolve the index URL: %s\n", err)
		os.Exit(1)
	}

	var index remoteIndexFile
	if _, err := fetchPackTOML(indexURL, &index); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	violations := []core.SyncViolation{}
	metafilesToCheck := make([]remoteIndexFileEntry, 0)

	for _, entry := range index.Files {
		if err := lock.Check(entry.File); err != nil {
			violations = append(violations, core.SyncViolation{Path: entry.File, Reason: err.Error()})
			continue
		}
		if entry.Alias != "" {
			if err := lock.Check(entry.Alias); err != nil {
				violations = append(violations, core.SyncViolation{
					Path:   entry.File,
					Reason: fmt.Sprintf("alias %q: %s", entry.Alias, err),
				})
			}
		}
		if entry.MetaFile {
			metafilesToCheck = append(metafilesToCheck, entry)
		}
	}

	// Fetch the metadata files concurrently; each one declares the real download destination
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, entry := range metafilesToCheck {
		wg.Add(1)
		go func(entry remoteIndexFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			metaURL, err := resolvePackURL(base, entry.File)
			if err != nil {
				mu.Lock()
				violations = append(violations, core.SyncViolation{Path: entry.File, Reason: err.Error()})
				mu.Unlock()
				return
			}
			var meta remoteMetaFile
			if _, err := fetchPackTOML(metaURL, &meta); err != nil {
				mu.Lock()
				violations = append(violations, core.SyncViolation{Path: entry.File, Reason: err.Error()})
				mu.Unlock()
				return
			}
			dest, err := lock.CheckMetaDest(entry.File, meta.Filename)
			if err != nil {
				mu.Lock()
				violations = append(violations, core.SyncViolation{Path: entry.File, Dest: dest, Reason: err.Error()})
				mu.Unlock()
			}
		}(entry)
	}
	wg.Wait()

	sort.Slice(violations, func(i, j int) bool { return violations[i].Path < violations[j].Path })
	printLockCheckResult(lock, indexURL, len(index.Files), violations)
}

func init() {
	rootCmd.AddCommand(lockCmd)
	lockCmd.AddCommand(lockInitCmd)
	lockCmd.AddCommand(lockShowCmd)
	lockCmd.AddCommand(lockCheckCmd)

	lockInitCmd.Flags().StringVar(&lockInitMode, "mode", "", "Lock mode: strip (default), deny, warn or off")
	lockInitCmd.Flags().StringVar(&lockInitNote, "note", "", "Optional note stored in the lock and shown by packwiz lock show")
	lockInitCmd.Flags().BoolVar(&lockInitForce, "force", false, "Overwrite an existing [sync] table")

	lockCheckCmd.Flags().StringVar(&lockCheckURL, "url", "", "Check a remote pack.toml instead of the pack in the current directory")
	lockCheckCmd.Flags().BoolVar(&lockCheckJSON, "json", false, "Print the result as JSON")
	lockCheckCmd.Flags().BoolVarP(&lockCheckQuiet, "quiet", "q", false, "Only exit with a non-zero status on failure")
}

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
		"out-of-scope files out of index.toml is what makes the installer refuse to download them.\n" +
		"'packwiz sync' enforces the same lock on the client side.",
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
		fmt.Println()
		fmt.Println("Publish pack.toml AND index.toml together, then verify what clients will see with:")
		fmt.Println("  packwiz lock check --url <the pack.toml URL> --preflight")
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
var lockCheckPreflight bool
var lockCheckRequireLock bool
var lockCheckWorkers int

var lockCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Verify a pack obeys its own lock (exits non-zero on failure)",
	Long: "Verify that a pack only contains files inside the folders it locks.\n\n" +
		"Without --url the pack in the current directory is checked. With --url the pack.toml is\n" +
		"downloaded and checked exactly the way a client would see it, which is the only way to\n" +
		"catch a stale index, a half-published pack or a file whose hash no longer matches.\n\n" +
		"--preflight goes one step further and downloads every file the pack lists, verifying its\n" +
		"hash without writing anything. That reproduces the client's \"hash invalid\" failures on\n" +
		"the authoring side.\n\n" +
		"A client can gate packwiz-installer with it:\n\n" +
		"\tpackwiz lock check --url https://example.com/pack.toml --require-lock || exit 1\n" +
		"\tjava -jar packwiz-installer-bootstrap.jar https://example.com/pack.toml",
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		if lockCheckURL != "" {
			runRemoteLockCheck(lockCheckURL)
		} else {
			runLocalLockCheck()
		}
	},
}

type lockCheckOutcome struct {
	lock        core.SyncLock
	source      string
	packName    string
	checked     int
	violations  []core.SyncViolation
	emptyHashes []string
	indexOK     bool
	indexExpect string
	indexActual string
	preflight   []core.PreflightResult
	preflighted int
	missingLock bool
}

type preflightReport struct {
	Path  string "json:\"path\""
	Dest  string "json:\"dest\""
	Error string "json:\"error\""
}

func (o lockCheckOutcome) problems() int {
	count := len(o.violations) + len(o.emptyHashes)
	if !o.indexOK {
		count++
	}
	if o.missingLock {
		count++
	}
	for _, result := range o.preflight {
		if result.Error != nil {
			count++
		}
	}
	return count
}

func printLockCheckOutcome(outcome lockCheckOutcome) {
	failures := make([]preflightReport, 0)
	for _, result := range outcome.preflight {
		if result.Error != nil {
			failures = append(failures, preflightReport{
				Path:  result.Task.IndexPath,
				Dest:  result.Task.Dest,
				Error: result.Error.Error(),
			})
		}
	}

	if lockCheckJSON {
		payload := struct {
			OK            bool                 "json:\"ok\""
			Source        string               "json:\"source\""
			Pack          string               "json:\"pack\""
			Checked       int                  "json:\"checked\""
			Lock          core.SyncLock        "json:\"lock\""
			LockMissing   bool                 "json:\"lock_missing\""
			IndexHashOK   bool                 "json:\"index_hash_ok\""
			IndexExpected string               "json:\"index_hash_expected,omitempty\""
			IndexActual   string               "json:\"index_hash_actual,omitempty\""
			Violations    []core.SyncViolation "json:\"violations\""
			EmptyHashes   []string             "json:\"entries_without_hash,omitempty\""
			Preflighted   int                  "json:\"preflighted\""
			Preflight     []preflightReport    "json:\"preflight_failures,omitempty\""
		}{
			OK:            outcome.problems() == 0,
			Source:        outcome.source,
			Pack:          outcome.packName,
			Checked:       outcome.checked,
			Lock:          outcome.lock,
			LockMissing:   outcome.missingLock,
			IndexHashOK:   outcome.indexOK,
			IndexExpected: outcome.indexExpect,
			IndexActual:   outcome.indexActual,
			Violations:    outcome.violations,
			EmptyHashes:   outcome.emptyHashes,
			Preflighted:   outcome.preflighted,
			Preflight:     failures,
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println(string(encoded))
	} else if !lockCheckQuiet {
		fmt.Println("Pack: " + outcome.packName)
		fmt.Println("Source: " + outcome.source)
		fmt.Println("Sync lock: " + outcome.lock.SyncLockDescription())
		fmt.Printf("Checked %d index entr%s\n", outcome.checked, pluralY(outcome.checked))

		if outcome.missingLock {
			fmt.Println()
			fmt.Println("PROBLEM: this pack does not declare a [sync] lock, so it could write anywhere.")
			fmt.Println("  Ask the pack author to run 'packwiz lock init mods'.")
		}

		if !outcome.indexOK {
			fmt.Println()
			fmt.Println("PROBLEM: index.toml does not match the hash recorded in pack.toml.")
			fmt.Printf("  pack.toml says      : %s\n", outcome.indexExpect)
			fmt.Printf("  the served index is : %s\n", outcome.indexActual)
			fmt.Println("  Clients abort with 'Your index file hash is invalid!'. Publish pack.toml and")
			fmt.Println("  index.toml together, after running 'packwiz refresh'.")
		}

		if len(outcome.violations) > 0 {
			fmt.Println()
			fmt.Printf("PROBLEM: %d path(s) are outside of the locked folders:\n", len(outcome.violations))
			core.PrintSyncViolations(outcome.violations, 25)
			fmt.Println("  packwiz-installer would download these; 'packwiz sync' refuses them.")
		}

		if len(outcome.emptyHashes) > 0 {
			fmt.Println()
			fmt.Printf("PROBLEM: %d entr(y/ies) have no hash recorded:\n", len(outcome.emptyHashes))
			for i, entry := range outcome.emptyHashes {
				if i >= 10 {
					fmt.Printf("  ... and %d more\n", len(outcome.emptyHashes)-10)
					break
				}
				fmt.Println("  - " + entry)
			}
			fmt.Println("  Clients report 'hash invalid!' for these. This happens when a pack is published")
			fmt.Println("  with no-internal-hashes enabled; run 'packwiz refresh --build' before publishing.")
		}

		if outcome.preflighted > 0 {
			fmt.Println()
			if len(failures) == 0 {
				fmt.Printf("Preflight: all %d file(s) downloaded and matched their recorded hash.\n", outcome.preflighted)
			} else {
				fmt.Printf("PROBLEM: %d of %d file(s) failed the preflight:\n", len(failures), outcome.preflighted)
				for i, failure := range failures {
					if i >= 25 {
						fmt.Printf("  ... and %d more\n", len(failures)-25)
						break
					}
					fmt.Printf("  - %s -> %s: %s\n", failure.Path, failure.Dest, failure.Error)
				}
				fmt.Println("  Clients fail to install these files.")
			}
		}

		if outcome.problems() == 0 {
			fmt.Println()
			fmt.Println("OK: this pack stays inside its locked folders and every file checks out.")
		}
	}

	if outcome.problems() > 0 {
		os.Exit(1)
	}
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func normalizePackLock(pack core.Pack) (core.SyncLock, error) {
	if pack.Sync == nil {
		return core.SyncLock{}, nil
	}
	return pack.Sync.Normalize()
}

func finishLockCheck(plan core.InstallPlan, lock core.SyncLock, source string) {
	outcome := lockCheckOutcome{
		lock:        lock,
		source:      source,
		packName:    plan.Pack.Name,
		checked:     len(plan.Index.Files),
		violations:  plan.Violations,
		emptyHashes: plan.EmptyHashes,
		indexOK:     plan.IndexHashOK(),
		indexExpect: plan.IndexExpected,
		indexActual: plan.IndexActual,
		missingLock: lockCheckRequireLock && !lock.IsActive(),
	}
	if lockCheckPreflight {
		outcome.preflight = plan.RunPreflight(plan.Tasks, lockCheckWorkers)
		outcome.preflighted = len(plan.Tasks)
	}
	printLockCheckOutcome(outcome)
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
	indexRaw, err := os.ReadFile(index.IndexFilePath())
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	lock, err := core.LoadSyncLock()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if lockCheckPreflight && !lockCheckQuiet && !lockCheckJSON {
		fmt.Println("Note: checking local files only. Add --url to verify what clients actually download.")
	}
	location := core.NewDirPackLocation(viper.GetString("pack-file"))
	plan := core.BuildInstallPlan(pack, index, location, lock, indexRaw)
	source := viper.GetString("pack-file")
	if strings.TrimSpace(source) == "" {
		source = "pack.toml"
	}
	finishLockCheck(plan, lock, source)
}

func runRemoteLockCheck(packURL string) {
	location, err := core.NewURLPackLocation(packURL)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	packData, err := location.ReadAll("")
	if err != nil {
		fmt.Printf("Could not download %s: %s\n", packURL, err)
		os.Exit(1)
	}
	pack, err := core.LoadPackFromBytes(packData)
	if err != nil {
		fmt.Printf("Could not read %s: %s\n", packURL, err)
		os.Exit(1)
	}
	lock, err := normalizePackLock(pack)
	if err != nil {
		fmt.Printf("%s: %s\n", packURL, err)
		os.Exit(1)
	}
	indexRel := strings.TrimSpace(pack.Index.File)
	if indexRel == "" {
		indexRel = "index.toml"
	}
	indexRaw, err := location.ReadAll(indexRel)
	if err != nil {
		fmt.Printf("Could not download %s: %s\n", location.Resolve(indexRel), err)
		os.Exit(1)
	}
	index, err := core.LoadIndexFromBytes(indexRaw, indexRel)
	if err != nil {
		fmt.Printf("Could not read %s: %s\n", location.Resolve(indexRel), err)
		os.Exit(1)
	}
	plan := core.BuildInstallPlan(pack, index, location, lock, indexRaw)
	finishLockCheck(plan, lock, location.Resolve(indexRel))
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
	lockCheckCmd.Flags().BoolVar(&lockCheckPreflight, "preflight", false, "Also download every file the pack lists and verify its hash")
	lockCheckCmd.Flags().BoolVar(&lockCheckRequireLock, "require-lock", false, "Fail when the pack does not declare a [sync] lock")
	lockCheckCmd.Flags().IntVar(&lockCheckWorkers, "workers", 8, "How many files to verify at the same time during --preflight")
}

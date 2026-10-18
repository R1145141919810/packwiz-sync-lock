package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/packwiz/packwiz/core"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var syncPackURL string
var syncTarget string
var syncSide string
var syncDryRun bool
var syncAllowUnlocked bool
var syncIgnoreLock bool
var syncIncludeOptional bool
var syncWorkers int
var syncNoFallback bool
var syncTimeout int

// syncCmd represents the sync command
var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Install a pack into an instance folder, refusing to touch anything outside the lock",
	Long: "Install (or update) a pack into a game instance folder.\n\n" +
		"This is a native replacement for packwiz-installer that enforces the pack's [sync] lock\n" +
		"itself. Every destination is checked before anything is downloaded, and a file that would\n" +
		"land outside of the locked folders makes the whole run refuse to install it - so a stale\n" +
		"or hand-edited index can never make a client write outside of them.\n\n" +
		"Unlike packwiz-installer it never deletes files, and it refuses packs that do not declare\n" +
		"a lock unless --allow-unlocked is given.\n\n" +
		"Examples:\n" +
		"  packwiz sync --pack-url https://example.com/pack.toml --target ~/.minecraft\n" +
		"  packwiz sync --pack-url https://example.com/pack.toml --target ./server --side server\n" +
		"  packwiz sync --target ./instance --dry-run",
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		runSync()
	},
}

func runSync() {
	side := strings.ToLower(strings.TrimSpace(syncSide))
	switch side {
	case "client", "server", "both":
	default:
		fmt.Println("--side must be client, server or both")
		os.Exit(1)
	}

	targetDir := syncTarget
	if strings.TrimSpace(targetDir) == "" {
		targetDir = "."
	}
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if info, statErr := os.Stat(absTarget); statErr != nil || !info.IsDir() {
		fmt.Printf("The instance folder %s does not exist.\n", absTarget)
		os.Exit(1)
	}

	var (
		pack     core.Pack
		index    core.Index
		indexRaw []byte
		loc      core.PackLocation
	)

	if strings.TrimSpace(syncPackURL) != "" {
		urlLoc, locErr := core.NewURLPackLocation(syncPackURL)
		if locErr != nil {
			fmt.Println(locErr)
			os.Exit(1)
		}
		loc = urlLoc
		packData, readErr := loc.ReadAll("")
		if readErr != nil {
			fmt.Printf("Could not download %s: %s\n", syncPackURL, readErr)
			os.Exit(1)
		}
		pack, err = core.LoadPackFromBytes(packData)
		if err != nil {
			fmt.Printf("Could not read %s: %s\n", syncPackURL, err)
			os.Exit(1)
		}
		indexRel := strings.TrimSpace(pack.Index.File)
		if indexRel == "" {
			indexRel = "index.toml"
			pack.Index.File = indexRel
		}
		indexRaw, err = loc.ReadAll(indexRel)
		if err != nil {
			fmt.Printf("Could not download %s: %s\n", loc.Resolve(indexRel), err)
			os.Exit(1)
		}
		index, err = core.LoadIndexFromBytes(indexRaw, indexRel)
		if err != nil {
			fmt.Printf("Could not read %s: %s\n", loc.Resolve(indexRel), err)
			os.Exit(1)
		}
		fmt.Printf("Pack \"%s\" from %s\n", pack.Name, syncPackURL)
	} else {
		pack, err = core.LoadPack()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		index, err = pack.LoadIndex()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		indexRaw, err = os.ReadFile(index.IndexFilePath())
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		loc = core.NewDirPackLocation(viper.GetString("pack-file"))
		fmt.Printf("Pack \"%s\" from %s\n", pack.Name, loc.Describe())
	}

	lock := core.SyncLock{}
	if pack.Sync != nil {
		normalized, normErr := pack.Sync.Normalize()
		if normErr != nil {
			fmt.Println(normErr)
			os.Exit(1)
		}
		lock = normalized
	}
	if strings.TrimSpace(pack.Index.HashFormat) == "" {
		pack.Index.HashFormat = "sha256"
	}

	plan := core.BuildInstallPlan(pack, index, loc, lock, indexRaw)
	fallbackEnabled := plan.FallbackToServer && !syncNoFallback
	timeout := plan.DownloadTimeout
	if syncTimeout > 0 {
		timeout = time.Duration(syncTimeout) * time.Second
	}
	plan.SetDownloadPolicy(fallbackEnabled, timeout)

	if !plan.IndexHashOK() {
		fmt.Println("The index does not match the hash recorded in pack.toml:")
		fmt.Printf("  pack.toml [index] hash : %s\n", plan.IndexExpected)
		fmt.Printf("  the index file hashes to: %s\n", plan.IndexActual)
		fmt.Println("The pack was published without refreshing the index it serves. Refusing to install a")
		fmt.Println("pack whose file list cannot be trusted.")
		os.Exit(2)
	}

	if !lock.IsActive() {
		if !syncAllowUnlocked {
			fmt.Println("This pack does not lock any folder, so there is no way to tell which files it may touch.")
			fmt.Println("Ask the pack author to add a [sync] table to pack.toml (packwiz lock init mods), or pass")
			fmt.Println("--allow-unlocked to install it anyway.")
			os.Exit(2)
		}
		fmt.Println("Warning: this pack has no lock; only paths that escape the instance folder are refused.")
	} else {
		fmt.Printf("Sync lock: %s\n", lock.SyncLockDescription())
	}

	if len(plan.Violations) > 0 {
		fmt.Printf("%d path(s) of this pack are not allowed to be installed:\n", len(plan.Violations))
		core.PrintSyncViolations(plan.Violations, 25)
		if !syncIgnoreLock {
			fmt.Println()
			fmt.Println("Refusing to install them; nothing was written.")
			os.Exit(2)
		}
		fmt.Println("Warning: --ignore-lock is set, so these files will be installed anyway.")
	}

	if len(plan.EmptyHashes) > 0 {
		fmt.Printf("Warning: %d entr(y/ies) have no hash recorded, so their content cannot be verified:\n", len(plan.EmptyHashes))
		printCappedList(plan.EmptyHashes, 10)
		fmt.Println("This usually means the pack was published with no-internal-hashes enabled; run")
		fmt.Println("'packwiz refresh --build' before publishing.")
	}

	tasks := make([]core.InstallTask, 0, len(plan.Tasks))
	skippedSide, skippedOptional := 0, 0
	for _, task := range plan.Tasks {
		if !core.TaskMatchesSide(task, side) {
			skippedSide++
			continue
		}
		if task.Optional && !syncIncludeOptional {
			skippedOptional++
			continue
		}
		tasks = append(tasks, task)
	}

	if len(tasks) == 0 {
		fmt.Println("Nothing to install.")
		return
	}

	mode := "Installing"
	if syncDryRun {
		mode = "Would install"
	}
	fmt.Printf("%s %d file(s) into %s (side: %s)\n", mode, len(tasks), absTarget, side)

	results := make([]core.InstallOutcome, len(tasks))
	workers := syncWorkers
	if workers < 1 {
		workers = 8
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}
	var wg sync.WaitGroup
	work := make(chan int)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range work {
				results[index] = plan.ApplyTask(tasks[index], absTarget, syncDryRun)
			}
		}()
	}
	for i := range tasks {
		work <- i
	}
	close(work)
	wg.Wait()

	written, upToDate, skipped, failed, fallback := 0, 0, 0, 0, 0
	failures := make([]core.InstallOutcome, 0)
	fallbackOutcomes := make([]core.InstallOutcome, 0)
	for _, result := range results {
		if result.FallbackUsed {
			fallback++
			fallbackOutcomes = append(fallbackOutcomes, result)
		}
		switch {
		case result.Error != nil:
			failed++
			failures = append(failures, result)
		case result.Written:
			written++
		case result.UpToDate:
			upToDate++
		case result.Skipped:
			skipped++
		}
	}

	fmt.Println()
	if syncDryRun {
		fmt.Printf("Dry run: %d file(s) would be downloaded.\n", written)
	} else {
		fmt.Printf("Installed: %d\n", written)
	}
	fmt.Printf("Already up to date: %d\n", upToDate)
	if skipped > 0 {
		fmt.Printf("Skipped: %d\n", skipped)
	}
	if skippedSide > 0 {
		fmt.Printf("Skipped because of the side filter (%s): %d\n", side, skippedSide)
	}
	if skippedOptional > 0 {
		fmt.Printf("Skipped optional file(s): %d (pass --include-optional to install them)\n", skippedOptional)
	}
	if len(plan.Violations) > 0 {
		fmt.Printf("Refused by the sync lock: %d\n", len(plan.Violations))
	}
	if fallback > 0 {
		fmt.Printf("Downloaded from the pack server because the mod host failed: %d\n", fallback)
		for i, outcome := range fallbackOutcomes {
			if i >= 5 {
				fmt.Printf("  ... and %d more\n", len(fallbackOutcomes)-5)
				break
			}
			fmt.Printf("  - %s (%s)\n", outcome.Task.Dest, firstLine(outcome.FallbackReason))
		}
	}
	if failed > 0 {
		fmt.Printf("Failed: %d\n", failed)
		for _, failure := range failures {
			fmt.Printf("  - %s: %s\n", failure.Task.Dest, failure.Error)
		}
		fmt.Println()
		os.Exit(1)
	}
	if !syncDryRun {
		fmt.Println()
		fmt.Println("Done. Files removed from the pack are left in place; 'packwiz sync' never deletes anything.")
	}
}

func firstLine(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return text
}

func printCappedList(entries []string, maxPrinted int) {
	for i, entry := range entries {
		if i >= maxPrinted {
			fmt.Printf("  ... and %d more\n", len(entries)-maxPrinted)
			return
		}
		fmt.Printf("  - %s\n", entry)
	}
}

func init() {
	rootCmd.AddCommand(syncCmd)

	syncCmd.Flags().StringVar(&syncPackURL, "pack-url", "", "Install the pack served at this pack.toml URL instead of the local pack")
	syncCmd.Flags().StringVar(&syncTarget, "target", ".", "The instance folder to install into")
	syncCmd.Flags().StringVarP(&syncSide, "side", "s", "client", "Which side to install for: client, server or both")
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "Show what would be installed without writing anything")
	syncCmd.Flags().BoolVar(&syncAllowUnlocked, "allow-unlocked", false, "Install even when the pack does not declare a [sync] lock")
	syncCmd.Flags().BoolVar(&syncIgnoreLock, "ignore-lock", false, "DANGEROUS: install files even when the lock refuses them")
	syncCmd.Flags().BoolVar(&syncIncludeOptional, "include-optional", false, "Also install files the pack marks as optional")
	syncCmd.Flags().IntVar(&syncWorkers, "workers", 8, "How many files to download at the same time")
	syncCmd.Flags().BoolVar(&syncNoFallback, "no-fallback", false, "Never fall back to the pack server, even when the mod host fails")
	syncCmd.Flags().IntVar(&syncTimeout, "timeout", 0, "Seconds before a single download is considered failed and the fallback is tried (0 = use the pack setting)")
}

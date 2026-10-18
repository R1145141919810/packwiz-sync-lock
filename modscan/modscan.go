// Package modscan adds "packwiz mod scan", which identifies mods on Modrinth and then asks
// CurseForge about whatever Modrinth did not know.
package modscan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/packwiz/packwiz/cmd"
	"github.com/packwiz/packwiz/core"
	"github.com/packwiz/packwiz/curseforge"
	"github.com/packwiz/packwiz/modrinth"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var modCmd = &cobra.Command{
	Use:   "mod",
	Short: "Identify mods on Modrinth and CurseForge",
}

var (
	modScanDryRun    bool
	modScanOverwrite bool
	modScanPrune     bool
	modScanStrict    bool
	modScanRecursive bool
	modScanSide      string
	modScanAlgorithm string
	modScanBatchSize int
)

var modScanCmd = &cobra.Command{
	Use:   "scan [folders...]",
	Short: "Scan folders for mods: Modrinth first, then CurseForge for the rest",
	Long: "Scan folders and turn every file that a mod host knows about into packwiz metadata.\n\n" +
		"Two passes, in this order:\n\n" +
		"  1. Modrinth: every file is hashed (sha1 + sha512) and looked up by hash.\n" +
		"  2. CurseForge: only the files Modrinth did not know are fingerprinted and looked up.\n\n" +
		"The metadata written is the standard packwiz format, so packwiz-installer downloads\n" +
		"Modrinth files from the Modrinth CDN and CurseForge files through the CurseForge API\n" +
		"without any extra configuration.\n\n" +
		"Files outside of the [sync] lock are never turned into metadata, so a scan cannot widen\n" +
		"the lock.",
	Args: cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, args []string) {
		runModScan(args)
	},
}

func runModScan(args []string) {
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

	folders, err := core.ScanFolders(args, lock, packRoot)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	targets, destinations, usedMeta, walkErr := core.WalkScanTargets(folders, packRoot, lock, modScanRecursive)
	if walkErr != nil {
		fmt.Println(walkErr)
		os.Exit(1)
	}
	fmt.Printf("Scanned %d file(s) in %s\n", len(targets), strings.Join(folders, ", "))
	if len(targets) == 0 {
		return
	}
	byRel := make(map[string]core.ScanTarget, len(targets))
	for _, target := range targets {
		byRel[target.RelPath] = target
	}

	// Pass 1: Modrinth
	fmt.Println()
	fmt.Println("=== Modrinth ===")
	fmt.Printf("Looking up %d hash(es)...\n", len(targets))
	modrinthResult := modrinth.ScanTargets(targets, modrinth.ScanOptions{
		PackRoot:     packRoot,
		Lock:         lock,
		Index:        &index,
		Destinations: destinations,
		UsedMeta:     usedMeta,
		Algorithm:    modScanAlgorithm,
		BatchSize:    modScanBatchSize,
		SideOverride: modScanSide,
		DryRun:       modScanDryRun,
		Overwrite:    modScanOverwrite,
		Prune:        modScanPrune,
	})
	fmt.Printf("Matched on Modrinth: %d\n", modrinthResult.Matched)

	// Pass 2: CurseForge, only for what Modrinth could not identify
	curseForgeResult := curseforge.ScanResult{}
	remainingTargets := make([]core.ScanTarget, 0, len(modrinthResult.Remaining))
	for _, rel := range modrinthResult.Remaining {
		if target, ok := byRel[rel]; ok {
			remainingTargets = append(remainingTargets, target)
		}
	}
	if len(remainingTargets) > 0 {
		fmt.Println()
		fmt.Println("=== CurseForge ===")
		fmt.Printf("Fingerprinting the %d file(s) Modrinth did not know...\n", len(remainingTargets))
		curseForgeResult = curseforge.ScanTargets(remainingTargets, curseforge.ScanOptions{
			PackRoot:     packRoot,
			Lock:         lock,
			Index:        &index,
			Destinations: destinations,
			UsedMeta:     usedMeta,
			DryRun:       modScanDryRun,
			Overwrite:    modScanOverwrite,
			Prune:        modScanPrune,
		})
		fmt.Printf("Matched on CurseForge: %d\n", curseForgeResult.Matched)
	} else {
		fmt.Println()
		fmt.Println("=== CurseForge ===")
		fmt.Println("Nothing left to look up.")
	}

	totalWritten := modrinthResult.Written + curseForgeResult.Written
	if !modScanDryRun && totalWritten > 0 {
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

	printCombinedResult(targets, modrinthResult, curseForgeResult, modScanDryRun, modScanStrict)
}

// dedupeSorted returns the entries without duplicates, sorted.
func dedupeSorted(entries []string) []string {
	seen := make(map[string]bool, len(entries))
	unique := make([]string, 0, len(entries))
	for _, entry := range entries {
		if seen[entry] {
			continue
		}
		seen[entry] = true
		unique = append(unique, entry)
	}
	sort.Strings(unique)
	return unique
}

// printCombinedResult prints one summary for both passes.
func printCombinedResult(targets []core.ScanTarget, modrinthResult modrinth.ScanResult, curseForgeResult curseforge.ScanResult, dryRun bool, strict bool) {
	// Both passes may report the same file (Modrinth failed, then CurseForge did not know it),
	// so merge and de-duplicate before printing.
	failed := dedupeSorted(append(append([]string{}, modrinthResult.Failed...), curseForgeResult.Failed...))
	skipped := dedupeSorted(append(append([]string{}, modrinthResult.SkippedOutside...), curseForgeResult.SkippedOutside...))
	remaining := dedupeSorted(append(append([]string{}, modrinthResult.Remaining...), curseForgeResult.Remaining...))

	fmt.Println()
	fmt.Println("=== Summary ===")
	if dryRun {
		fmt.Println("Dry run: no files were written.")
	}
	fmt.Printf("Scanned on disk: %d\n", len(targets))
	fmt.Printf("Downloadable from Modrinth: %d\n", modrinthResult.Matched)
	fmt.Printf("Downloadable from CurseForge: %d\n", curseForgeResult.Matched)
	fmt.Printf("Metadata files written: %d\n", modrinthResult.Written+curseForgeResult.Written)
	if modrinthResult.AlreadyPresent+curseForgeResult.AlreadyPresent > 0 {
		fmt.Printf("Already had metadata: %d\n", modrinthResult.AlreadyPresent+curseForgeResult.AlreadyPresent)
	}
	if modrinthResult.Pruned+curseForgeResult.Pruned > 0 {
		fmt.Printf("Local files pruned: %d\n", modrinthResult.Pruned+curseForgeResult.Pruned)
	}
	if len(curseForgeResult.ManualOnly) > 0 {
		fmt.Printf("Need a manual download: %d\n", len(curseForgeResult.ManualOnly))
		for i, name := range curseForgeResult.ManualOnly {
			if i >= 10 {
				fmt.Printf("  ... and %d more\n", len(curseForgeResult.ManualOnly)-10)
				break
			}
			fmt.Println("  - " + name)
		}
	}
	if len(skipped) > 0 {
		fmt.Printf("Skipped by the sync lock: %d\n", len(skipped))
		for _, entry := range skipped {
			fmt.Println("  - " + entry)
		}
	}
	if len(failed) > 0 {
		fmt.Printf("Failed: %d\n", len(failed))
		for _, entry := range failed {
			fmt.Println("  - " + entry)
		}
	}
	if len(remaining) > 0 {
		fmt.Printf("No metadata anywhere: %d\n", len(remaining))
		for i, entry := range remaining {
			if i >= 25 {
				fmt.Printf("  ... and %d more\n", len(remaining)-25)
				break
			}
			fmt.Println("  - " + entry)
		}
		fmt.Println("  These stay in the pack and are served by your own host.")
	}
	if !dryRun && modrinthResult.Written+curseForgeResult.Written > 0 {
		fmt.Println()
		fmt.Println("The new metadata is already in index.toml. Commit it together with pack.toml.")
	}
	if strict && (len(remaining) > 0 || len(failed) > 0 || len(skipped) > 0) {
		fmt.Println("Strict mode: some files could not be converted, failing.")
		os.Exit(1)
	}
}

func init() {
	modCmd.AddCommand(modScanCmd)
	cmd.Add(modCmd)

	modScanCmd.Flags().BoolVar(&modScanDryRun, "dry-run", false, "Report what would be written without writing anything")
	modScanCmd.Flags().BoolVar(&modScanOverwrite, "overwrite", false, "Also create metadata for files that already have metadata")
	modScanCmd.Flags().BoolVar(&modScanRecursive, "recursive", true, "Descend into subfolders")
	modScanCmd.Flags().BoolVar(&modScanPrune, "prune", false, "Delete the local file after its metadata has been written")
	modScanCmd.Flags().BoolVar(&modScanStrict, "strict", false, "Exit with a non-zero status if any scanned file could not be converted")
	modScanCmd.Flags().StringVar(&modScanSide, "side", "", "Force the side of every added file: client, server or both")
	modScanCmd.Flags().StringVar(&modScanAlgorithm, "algorithm", "sha512", "Hash algorithm used for Modrinth lookups: sha512 or sha1")
	modScanCmd.Flags().IntVar(&modScanBatchSize, "batch-size", 100, "How many hashes to look up per Modrinth request")
}

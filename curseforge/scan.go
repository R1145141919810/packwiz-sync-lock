package curseforge

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/packwiz/packwiz/core"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const cfScanBatchSize = 100

// ScanOptions controls a CurseForge scan pass.
type ScanOptions struct {
	PackRoot string
	Lock     core.SyncLock
	Index    *core.Index
	// Destinations maps a pack-relative file path to the metadata file that already installs it
	Destinations map[string]string
	// UsedMeta is the set of metadata paths that are already taken
	UsedMeta  map[string]bool
	DryRun    bool
	Overwrite bool
	Prune     bool
}

// ScanResult reports what a CurseForge scan pass did.
type ScanResult struct {
	Candidates     int
	Matched        int
	Written        int
	Pruned         int
	AlreadyPresent int
	ManualOnly     []string
	Unmatched      []string
	Failed         []string
	SkippedOutside []string
	// Remaining lists the files that still have no metadata after this pass
	Remaining []string
	// Metafiles maps a file path to the metadata file that now installs it
	Metafiles map[string]string
}

// cfMatch is a CurseForge match for one local file.
type cfMatch struct {
	ModID uint32
	File  modFileInfo
}

type cfCandidate struct {
	target *core.ScanTarget
	hash   uint32
	match  *cfMatch
}

// curseForgeFingerprint computes the fingerprint CurseForge uses: murmur2 with the CurseForge
// variation that ignores whitespace bytes. core's "murmur2" implementation does exactly that,
// so both the scanner and the client verifier agree with the CurseForge API.
func curseForgeFingerprint(path string) (uint32, error) {
	hasher, err := core.GetHashImpl("murmur2")
	if err != nil {
		return 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	if _, err := io.Copy(hasher, file); err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(hasher.HashToString(hasher.Sum(nil)), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(value), nil
}

// ScanTargets identifies the given files on CurseForge and writes metadata for the matches.
func ScanTargets(targets []core.ScanTarget, opts ScanOptions) ScanResult {
	result := ScanResult{
		Candidates:     len(targets),
		ManualOnly:     []string{},
		Unmatched:      []string{},
		Failed:         []string{},
		SkippedOutside: []string{},
		Remaining:      []string{},
		Metafiles:      make(map[string]string),
	}
	if len(targets) == 0 || opts.Index == nil {
		return result
	}
	if opts.Destinations == nil {
		opts.Destinations = make(map[string]string)
	}
	if opts.UsedMeta == nil {
		opts.UsedMeta = make(map[string]bool)
	}

	// 1. Fingerprint every candidate
	candidates := make([]*cfCandidate, 0, len(targets))
	byHash := make(map[uint32]*cfCandidate, len(targets))
	for i := range targets {
		target := &targets[i]
		if opts.Lock.IsActive() {
			if err := opts.Lock.Check(target.RelPath); err != nil {
				result.SkippedOutside = append(result.SkippedOutside, target.RelPath+" ("+err.Error()+")")
				continue
			}
		}
		hash, err := curseForgeFingerprint(target.AbsPath)
		if err != nil {
			result.Failed = append(result.Failed, target.RelPath+" (could not hash it: "+err.Error()+")")
			continue
		}
		candidate := &cfCandidate{target: target, hash: hash}
		candidates = append(candidates, candidate)
		if _, taken := byHash[hash]; !taken {
			byHash[hash] = candidate
		}
	}
	if len(candidates) == 0 {
		return result
	}

	// 2. Ask CurseForge which files it knows
	hashes := make([]uint32, 0, len(candidates))
	for _, candidate := range candidates {
		hashes = append(hashes, candidate.hash)
	}
	for start := 0; start < len(hashes); start += cfScanBatchSize {
		end := start + cfScanBatchSize
		if end > len(hashes) {
			end = len(hashes)
		}
		info, err := cfDefaultClient.getFingerprintInfo(hashes[start:end])
		if err != nil {
			result.Failed = append(result.Failed, "the CurseForge lookup failed: "+err.Error())
			break
		}
		for _, exact := range info.ExactMatches {
			candidate, ok := byHash[exact.File.Fingerprint]
			if !ok {
				// Fall back to the file name, in case the API reported a different fingerprint
				for _, other := range candidates {
					if other.match == nil && exact.File.FileName != "" && other.target.FileName == exact.File.FileName {
						candidate = other
						ok = true
						break
					}
				}
			}
			if !ok || candidate.match != nil {
				continue
			}
			candidate.match = &cfMatch{ModID: exact.ID, File: exact.File}
		}
	}

	// 3. Look up the project metadata for every match
	modIDs := make([]uint32, 0)
	seenMod := make(map[uint32]bool)
	for _, candidate := range candidates {
		if candidate.match != nil && !seenMod[candidate.match.ModID] {
			seenMod[candidate.match.ModID] = true
			modIDs = append(modIDs, candidate.match.ModID)
		}
	}
	projects := make(map[uint32]modInfo)
	for start := 0; start < len(modIDs); start += cfScanBatchSize {
		end := start + cfScanBatchSize
		if end > len(modIDs) {
			end = len(modIDs)
		}
		found, err := cfDefaultClient.getModInfoMultiple(modIDs[start:end])
		if err != nil {
			result.Failed = append(result.Failed, "could not read the CurseForge project metadata: "+err.Error())
			break
		}
		for _, project := range found {
			projects[project.ID] = project
		}
	}

	// 4. Write metadata for the matches
	for _, candidate := range candidates {
		if candidate.match == nil {
			result.Unmatched = append(result.Unmatched, candidate.target.RelPath)
			result.Remaining = append(result.Remaining, candidate.target.RelPath)
			continue
		}
		result.Matched++

		project, ok := projects[candidate.match.ModID]
		if !ok {
			result.Failed = append(result.Failed, candidate.target.RelPath+" (CurseForge returned no project metadata)")
			result.Remaining = append(result.Remaining, candidate.target.RelPath)
			continue
		}

		existingMeta := ""
		if metaRel, exists := opts.Destinations[candidate.target.RelPath]; exists {
			if !opts.Overwrite {
				result.AlreadyPresent++
				if opts.Prune {
					if removeErr := os.Remove(candidate.target.AbsPath); removeErr == nil {
						_ = opts.Index.RemoveFile(candidate.target.AbsPath)
						result.Pruned++
					}
				}
				continue
			}
			existingMeta = metaRel
		}

		metaRel := existingMeta
		if metaRel == "" {
			metaRel = core.UniqueScanMetaPath(filepath.ToSlash(filepath.Dir(candidate.target.RelPath)), metaBaseName(project, candidate.target.FileName), opts.UsedMeta, opts.Destinations)
		}

		if opts.Lock.IsActive() {
			if err := opts.Lock.Check(metaRel); err != nil {
				result.SkippedOutside = append(result.SkippedOutside, metaRel+" ("+err.Error()+")")
				continue
			}
			if _, err := opts.Lock.CheckMetaDest(metaRel, candidate.target.FileName); err != nil {
				result.SkippedOutside = append(result.SkippedOutside, metaRel+" -> "+candidate.target.FileName+" ("+err.Error()+")")
				continue
			}
		}

		if candidate.match.File.DownloadURL == "" {
			result.ManualOnly = append(result.ManualOnly, project.Name)
		}

		updateMap := make(map[string]map[string]interface{})
		hash, hashFormat := candidate.match.File.getBestHash()
		updateMap["curseforge"], _ = cfUpdateData{
			ProjectID: project.ID,
			FileID:    candidate.match.File.ID,
		}.ToMap()

		modMeta := core.Mod{
			Name:     project.Name,
			FileName: candidate.target.FileName,
			Side:     core.UniversalSide,
			Download: core.ModDownload{
				HashFormat: hashFormat,
				Hash:       hash,
				Mode:       core.ModeCF,
			},
			Update: updateMap,
		}

		if opts.DryRun {
			result.Written++
			continue
		}
		if err := core.WriteScanMetaFile(opts.PackRoot, metaRel, modMeta, opts.Index); err != nil {
			result.Failed = append(result.Failed, candidate.target.RelPath+" ("+err.Error()+")")
			result.Remaining = append(result.Remaining, candidate.target.RelPath)
			continue
		}
		opts.UsedMeta[metaRel] = true
		opts.Destinations[candidate.target.RelPath] = metaRel
		result.Metafiles[candidate.target.RelPath] = metaRel
		result.Written++

		if opts.Prune {
			if removeErr := os.Remove(candidate.target.AbsPath); removeErr != nil {
				fmt.Printf("Warning: could not remove %s: %s\n", candidate.target.RelPath, removeErr)
			} else {
				_ = opts.Index.RemoveFile(candidate.target.AbsPath)
				result.Pruned++
			}
		}
	}
	return result
}

// metaBaseName picks the preferred metadata file name for a CurseForge match.
func metaBaseName(project modInfo, fileName string) string {
	if slug := strings.TrimSpace(project.Slug); slug != "" {
		return slug
	}
	if name := core.SlugifyName(project.Name); name != "" {
		return name
	}
	return strings.TrimSuffix(fileName, filepath.Ext(fileName))
}

var cfScanDryRun bool
var cfScanOverwrite bool
var cfScanPrune bool
var cfScanStrict bool
var cfScanRecursive bool

var cfScanCmd = &cobra.Command{
	Use:   "scan [folders...]",
	Short: "Scan folders for files that are on CurseForge and write metadata for them",
	Long: "Scan folders for files that CurseForge knows about, and write a packwiz metadata file\n" +
		"(with the CurseForge download mode) next to each match.\n\n" +
		"Every file is fingerprinted the way CurseForge does it, so only the exact file that\n" +
		"CurseForge hosts is turned into a download. Files that are already covered by metadata\n" +
		"(for example from 'packwiz modrinth scan') are left alone.",
	Args: cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, args []string) {
		runCurseForgeScan(args)
	},
}

func runCurseForgeScan(args []string) {
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

	folders, err := cfScanFolders(args, lock, packRoot)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	targets, destinations, usedMeta, err := core.WalkScanTargets(folders, packRoot, lock, cfScanRecursive)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	fmt.Printf("Scanned %d file(s) in %s\n", len(targets), strings.Join(folders, ", "))
	if len(targets) == 0 {
		return
	}

	fmt.Println("Fingerprinting and asking CurseForge...")
	result := ScanTargets(targets, ScanOptions{
		PackRoot:     packRoot,
		Lock:         lock,
		Index:        &index,
		Destinations: destinations,
		UsedMeta:     usedMeta,
		DryRun:       cfScanDryRun,
		Overwrite:    cfScanOverwrite,
		Prune:        cfScanPrune,
	})

	if !cfScanDryRun && result.Written > 0 {
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
	if cfScanDryRun {
		fmt.Println("Dry run: no files were written.")
	}
	fmt.Printf("Matched on CurseForge: %d\n", result.Matched)
	fmt.Printf("Metadata files written: %d\n", result.Written)
	if result.Pruned > 0 {
		fmt.Printf("Local files pruned: %d\n", result.Pruned)
	}
	if result.AlreadyPresent > 0 {
		fmt.Printf("Already had metadata: %d\n", result.AlreadyPresent)
	}
	if len(result.ManualOnly) > 0 {
		fmt.Printf("Require a manual download (the author disabled third party downloads): %d\n", len(result.ManualOnly))
		for i, name := range result.ManualOnly {
			if i >= 10 {
				fmt.Printf("  ... and %d more\n", len(result.ManualOnly)-10)
				break
			}
			fmt.Println("  - " + name)
		}
	}
	if len(result.SkippedOutside) > 0 {
		fmt.Printf("Skipped because of the sync lock: %d\n", len(result.SkippedOutside))
		for _, entry := range result.SkippedOutside {
			fmt.Println("  - " + entry)
		}
	}
	if len(result.Failed) > 0 {
		fmt.Printf("Failed: %d\n", len(result.Failed))
		for _, entry := range result.Failed {
			fmt.Println("  - " + entry)
		}
	}
	if len(result.Unmatched) > 0 {
		fmt.Printf("Not known to CurseForge: %d\n", len(result.Unmatched))
		for i, entry := range result.Unmatched {
			if i >= 25 {
				fmt.Printf("  ... and %d more\n", len(result.Unmatched)-25)
				break
			}
			fmt.Println("  - " + entry)
		}
	}
	if cfScanStrict && (len(result.Unmatched) > 0 || len(result.Failed) > 0 || len(result.SkippedOutside) > 0) {
		fmt.Println("Strict mode: some files could not be converted, failing.")
		os.Exit(1)
	}
}

// cfScanFolders decides which folders to scan: explicit arguments first, then the locked
// folders, then the folders packwiz knows about that actually exist.
func cfScanFolders(args []string, lock core.SyncLock, packRoot string) ([]string, error) {
	var requested []string
	if len(args) > 0 {
		requested = args
	} else if lock.IsActive() && !lock.AllowsAll() {
		requested = append(requested, lock.AllowedPaths...)
	} else {
		requested = []string{"mods", "plugins", "resourcepacks", "shaderpacks"}
		if datapackFolder := viper.GetString("datapack-folder"); strings.TrimSpace(datapackFolder) != "" {
			requested = append(requested, datapackFolder)
		}
	}

	var folders []string
	for _, folder := range requested {
		folder = strings.TrimSpace(folder)
		if folder == "" || folder == "*" {
			continue
		}
		abs := folder
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(packRoot, filepath.FromSlash(folder))
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			if len(args) > 0 {
				fmt.Printf("Warning: %s is not a folder, skipping\n", folder)
			}
			continue
		}
		folders = append(folders, abs)
	}
	if len(folders) == 0 {
		return nil, fmt.Errorf("no folder to scan; pass one explicitly, for example: packwiz curseforge scan mods")
	}
	return folders, nil
}

func init() {
	curseforgeCmd.AddCommand(cfScanCmd)

	cfScanCmd.Flags().BoolVar(&cfScanDryRun, "dry-run", false, "Report what would be written without writing anything")
	cfScanCmd.Flags().BoolVar(&cfScanOverwrite, "overwrite", false, "Also create metadata for files that already have metadata")
	cfScanCmd.Flags().BoolVar(&cfScanRecursive, "recursive", true, "Descend into subfolders")
	cfScanCmd.Flags().BoolVar(&cfScanPrune, "prune", false, "Delete the local file after its metadata has been written")
	cfScanCmd.Flags().BoolVar(&cfScanStrict, "strict", false, "Exit with a non-zero status if any scanned file could not be converted")
}

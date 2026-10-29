package modrinth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	modrinthApi "codeberg.org/theepicblock/go-modrinth/modrinth"
	"github.com/packwiz/packwiz/core"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const modrinthAPIBase = "https://api.modrinth.com/v2"

var errModrinthNotFound = fmt.Errorf("not found on Modrinth")

// mrFile is the subset of a Modrinth version file that the scanner needs
type mrFile struct {
	Hashes   map[string]string "json:\"hashes\""
	URL      string            "json:\"url\""
	Filename string            "json:\"filename\""
	Primary  bool              "json:\"primary\""
	Size     int64             "json:\"size\""
}

// mrVersion is the subset of a Modrinth version that the scanner needs
type mrVersion struct {
	ID            string   "json:\"id\""
	ProjectID     string   "json:\"project_id\""
	VersionNumber string   "json:\"version_number\""
	Name          string   "json:\"name\""
	VersionType   string   "json:\"version_type\""
	Loaders       []string "json:\"loaders\""
	GameVersions  []string "json:\"game_versions\""
	Environment   *string  "json:\"environment\""
	Files         []mrFile "json:\"files\""
}

// mrProject is the subset of a Modrinth project that the scanner needs
type mrProject struct {
	ID          string "json:\"id\""
	Slug        string "json:\"slug\""
	Title       string "json:\"title\""
	ProjectType string "json:\"project_type\""
}

func mrDoJSON(method string, endpoint string, body interface{}, out interface{}) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, modrinthAPIBase+endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", core.UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return errModrinthNotFound
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("Modrinth returned HTTP %d for %s", resp.StatusCode, endpoint)
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// mrLookupVersionsByHash asks Modrinth which versions the given file hashes belong to.
func mrLookupVersionsByHash(hashes []string, algorithm string) (map[string]mrVersion, error) {
	found := make(map[string]mrVersion)
	if len(hashes) == 0 {
		return found, nil
	}
	err := mrDoJSON("POST", "/version_files", map[string]interface{}{
		"hashes":    hashes,
		"algorithm": algorithm,
	}, &found)
	if err == errModrinthNotFound {
		return found, nil
	}
	if err != nil {
		return nil, err
	}
	return found, nil
}

func mrGetProjects(ids []string) (map[string]mrProject, error) {
	projects := make(map[string]mrProject)
	if len(ids) == 0 {
		return projects, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	var list []mrProject
	if err := mrDoJSON("GET", "/projects?ids="+url.QueryEscape(string(encoded)), nil, &list); err != nil {
		return nil, err
	}
	for _, project := range list {
		projects[project.ID] = project
	}
	return projects, nil
}

func chunkStrings(values []string, size int) [][]string {
	if size <= 0 {
		size = 100
	}
	var chunks [][]string
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		chunks = append(chunks, values[start:end])
	}
	return chunks
}

// scanCandidate is a file on disk that may be available on Modrinth
type scanCandidate struct {
	AbsPath  string
	RelPath  string
	FileName string
	Sha1     string
	Sha512   string
}

func hashFileForScan(path string) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	sha1Hasher, err := core.GetHashImpl("sha1")
	if err != nil {
		return "", "", err
	}
	sha512Hasher, err := core.GetHashImpl("sha512")
	if err != nil {
		return "", "", err
	}
	if _, err := io.Copy(io.MultiWriter(sha1Hasher, sha512Hasher), file); err != nil {
		return "", "", err
	}
	return sha1Hasher.HashToString(sha1Hasher.Sum(nil)), sha512Hasher.HashToString(sha512Hasher.Sum(nil)), nil
}

func hashOfCandidate(candidate scanCandidate, algorithm string) string {
	if algorithm == "sha1" {
		return strings.ToLower(candidate.Sha1)
	}
	return strings.ToLower(candidate.Sha512)
}

func scanSide(version mrVersion) (string, bool) {
	// "either" means the project works on one side only, but Modrinth does not say which
	side := getSide(&modrinthApi.Version{Environment: version.Environment})
	if side == "either" || side == "" {
		return core.UniversalSide, true
	}
	return side, false
}

// uniqueMetaPath picks the metadata file name for a match, preferring the project slug and
// falling back to a numbered suffix when that name is already taken.
func uniqueMetaPath(candidate scanCandidate, project mrProject, used map[string]bool, destinations map[string]string) string {
	dir := filepath.ToSlash(filepath.Dir(candidate.RelPath))
	base := strings.TrimSpace(project.Slug)
	if base == "" {
		base = core.SlugifyName(project.Title)
	}
	if base == "" {
		base = strings.TrimSuffix(candidate.FileName, filepath.Ext(candidate.FileName))
	}
	return core.UniqueScanMetaPath(dir, base, used, destinations)
}

func printLimitedList(entries []string) {
	const maxPrinted = 25
	for i, entry := range entries {
		if i >= maxPrinted {
			fmt.Printf("  ... and %d more\n", len(entries)-maxPrinted)
			return
		}
		fmt.Printf("  - %s\n", entry)
	}
}

// ScanOptions controls one Modrinth scan pass.
type ScanOptions struct {
	PackRoot string
	Lock     core.SyncLock
	Index    *core.Index
	// Destinations maps a pack-relative file path to the metadata file that already installs it
	Destinations map[string]string
	// UsedMeta is the set of metadata paths that are already taken
	UsedMeta map[string]bool
	// Algorithm is "sha512" (default) or "sha1"
	Algorithm string
	// BatchSize is how many hashes to look up per request
	BatchSize int
	// SideOverride forces the side of every written file
	SideOverride string
	DryRun       bool
	Overwrite    bool
	Prune        bool
}

// ScanResult reports what one Modrinth scan pass did.
type ScanResult struct {
	Candidates     int
	Matched        int
	Written        int
	Pruned         int
	AlreadyPresent int
	Unmatched      []string
	Failed         []string
	SkippedOutside []string
	// Remaining lists the files this pass could not give metadata to, but which a later pass
	// (CurseForge) may still be able to handle
	Remaining []string
	// Metafiles maps a file path to the metadata file that now installs it
	Metafiles map[string]string
}

// ScanTargets identifies the given files on Modrinth and writes metadata for the matches.
func ScanTargets(targets []core.ScanTarget, opts ScanOptions) ScanResult {
	algorithm := opts.Algorithm
	if algorithm != "sha1" && algorithm != "sha512" {
		algorithm = "sha512"
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}

	result := ScanResult{
		Candidates:     len(targets),
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

	// 1. Hash every candidate
	candidates := make([]scanCandidate, 0, len(targets))
	for _, target := range targets {
		if opts.Lock.IsActive() {
			if err := opts.Lock.Check(target.RelPath); err != nil {
				result.SkippedOutside = append(result.SkippedOutside, target.RelPath+" ("+err.Error()+")")
				continue
			}
		}
		sha1Sum, sha512Sum, hashErr := hashFileForScan(target.AbsPath)
		if hashErr != nil {
			result.Failed = append(result.Failed, target.RelPath+" (could not hash it: "+hashErr.Error()+")")
			continue
		}
		candidates = append(candidates, scanCandidate{
			AbsPath:  target.AbsPath,
			RelPath:  target.RelPath,
			FileName: target.FileName,
			Sha1:     sha1Sum,
			Sha512:   sha512Sum,
		})
	}
	if len(candidates) == 0 {
		return result
	}

	// 2. Look every file up on Modrinth by hash
	uniqueHashes := make([]string, 0, len(candidates))
	seenHash := make(map[string]bool)
	for _, candidate := range candidates {
		hash := hashOfCandidate(candidate, algorithm)
		if !seenHash[hash] {
			seenHash[hash] = true
			uniqueHashes = append(uniqueHashes, hash)
		}
	}
	matches := make(map[string]mrVersion)
	for _, chunk := range chunkStrings(uniqueHashes, batchSize) {
		found, lookupErr := mrLookupVersionsByHash(chunk, algorithm)
		if lookupErr != nil {
			result.Failed = append(result.Failed, "the Modrinth lookup failed: "+lookupErr.Error())
			break
		}
		for hash, version := range found {
			matches[strings.ToLower(hash)] = version
		}
	}

	// 3. Resolve the projects of every matched version (for the slug and the display name)
	projectIDs := make([]string, 0)
	seenProject := make(map[string]bool)
	for _, candidate := range candidates {
		version, ok := matches[hashOfCandidate(candidate, algorithm)]
		if !ok {
			continue
		}
		if !seenProject[version.ProjectID] {
			seenProject[version.ProjectID] = true
			projectIDs = append(projectIDs, version.ProjectID)
		}
	}
	projects := make(map[string]mrProject)
	for _, chunk := range chunkStrings(projectIDs, batchSize) {
		found, projectErr := mrGetProjects(chunk)
		if projectErr != nil {
			result.Failed = append(result.Failed, "could not read the Modrinth project metadata: "+projectErr.Error())
			break
		}
		for id, project := range found {
			projects[id] = project
		}
	}

	// 4. Write metadata for the matches
	for _, candidate := range candidates {
		existingMetaRel := ""
		if metaRel, exists := opts.Destinations[candidate.RelPath]; exists {
			if !opts.Overwrite {
				result.AlreadyPresent++
				if opts.Prune {
					if removeErr := os.Remove(candidate.AbsPath); removeErr == nil {
						_ = opts.Index.RemoveFile(candidate.AbsPath)
						result.Pruned++
					}
				}
				continue
			}
			existingMetaRel = metaRel
		}

		localHash := hashOfCandidate(candidate, algorithm)
		version, ok := matches[localHash]
		if !ok {
			result.Unmatched = append(result.Unmatched, candidate.RelPath)
			result.Remaining = append(result.Remaining, candidate.RelPath)
			continue
		}
		project, ok := projects[version.ProjectID]
		if !ok {
			result.Unmatched = append(result.Unmatched, candidate.RelPath)
			result.Remaining = append(result.Remaining, candidate.RelPath)
			continue
		}
		result.Matched++

		// Find the exact file inside the version that matches the local file
		var matchedFile *mrFile
		for i := range version.Files {
			remoteHash, hasHash := version.Files[i].Hashes[algorithm]
			if hasHash && strings.EqualFold(remoteHash, localHash) {
				matchedFile = &version.Files[i]
				break
			}
		}
		if matchedFile == nil {
			result.Failed = append(result.Failed, candidate.RelPath+" (Modrinth did not return the matching file)")
			result.Remaining = append(result.Remaining, candidate.RelPath)
			continue
		}

		metaRel := existingMetaRel
		if metaRel == "" {
			metaRel = uniqueMetaPath(candidate, project, opts.UsedMeta, opts.Destinations)
		}
		if opts.Lock.IsActive() {
			if lockErr := opts.Lock.Check(metaRel); lockErr != nil {
				result.SkippedOutside = append(result.SkippedOutside, metaRel+" ("+lockErr.Error()+")")
				continue
			}
			if _, destErr := opts.Lock.CheckMetaDest(metaRel, candidate.FileName); destErr != nil {
				result.SkippedOutside = append(result.SkippedOutside, metaRel+" -> "+candidate.FileName+" ("+destErr.Error()+")")
				continue
			}
		}

		if opts.DryRun {
			fmt.Printf("  would write %s (%s %s)\n", metaRel, project.Slug, version.VersionNumber)
			result.Written++
			continue
		}

		side, ambiguous := scanSide(version)
		if opts.SideOverride != "" {
			side = opts.SideOverride
		} else if ambiguous {
			fmt.Printf("Warning: %s does not declare which side it belongs to; marking it as both\n", project.Title)
		}

		updateMap := make(map[string]map[string]interface{})
		var err error
		updateMap["modrinth"], err = mrUpdateData{
			ProjectID:        version.ProjectID,
			InstalledVersion: version.ID,
		}.ToMap()
		if err != nil {
			result.Failed = append(result.Failed, candidate.RelPath+" ("+err.Error()+")")
			result.Remaining = append(result.Remaining, candidate.RelPath)
			continue
		}

		modMeta := core.Mod{
			Name:     project.Title,
			FileName: candidate.FileName,
			Side:     side,
			Download: core.ModDownload{
				URL:        matchedFile.URL,
				HashFormat: algorithm,
				Hash:       matchedFile.Hashes[algorithm],
			},
			Update: updateMap,
		}
		if writeErr := core.WriteScanMetaFile(opts.PackRoot, metaRel, modMeta, opts.Index); writeErr != nil {
			result.Failed = append(result.Failed, candidate.RelPath+" ("+writeErr.Error()+")")
			result.Remaining = append(result.Remaining, candidate.RelPath)
			continue
		}
		opts.UsedMeta[metaRel] = true
		opts.Destinations[candidate.RelPath] = metaRel
		result.Metafiles[candidate.RelPath] = metaRel
		result.Written++

		if opts.Prune {
			if removeErr := os.Remove(candidate.AbsPath); removeErr != nil {
				fmt.Printf("Warning: could not remove %s: %s\n", candidate.RelPath, removeErr)
			} else {
				_ = opts.Index.RemoveFile(candidate.AbsPath)
				result.Pruned++
			}
		}
	}
	return result
}

var scanDryRun bool
var scanOverwrite bool
var scanRecursive bool
var scanPrune bool
var scanSideOverride string
var scanBatchSize int
var scanAlgorithm string
var scanStrict bool

var scanCmd = &cobra.Command{
	Use:     "scan [folders...]",
	Aliases: []string{"detect", "import"},
	Short:   "Scan folders for files that are on Modrinth and write metadata for them",
	Long: "Scan folders for files that are available on Modrinth and write a packwiz metadata\n" +
		"file (.pw.toml) next to each match.\n\n" +
		"Every file is hashed and looked up on Modrinth by hash, so only the exact file that\n" +
		"Modrinth hosts is turned into a download. The generated metadata stores the Modrinth\n" +
		"CDN URL, its hash and the [update.modrinth] block, which is exactly what\n" +
		"packwiz-installer understands.\n\n" +
		"This command only looks at Modrinth. Use 'packwiz mod scan' to also try CurseForge for\n" +
		"whatever Modrinth does not know, or 'packwiz curseforge scan' to only try CurseForge.",
	Args: cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, args []string) {
		runModrinthScan(args)
	},
}

func runModrinthScan(args []string) {
	if scanAlgorithm != "sha1" && scanAlgorithm != "sha512" {
		fmt.Println("--algorithm must be sha1 or sha512")
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

	folders, err := core.ScanFolders(args, lock, packRoot)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	targets, destinations, usedMeta, walkErr := core.WalkScanTargets(folders, packRoot, lock, scanRecursive)
	if walkErr != nil {
		fmt.Println(walkErr)
		os.Exit(1)
	}
	fmt.Printf("Scanned %d file(s) in %s\n", len(targets), strings.Join(folders, ", "))
	if len(targets) == 0 {
		return
	}

	fmt.Println("Looking up hashes on Modrinth...")
	result := ScanTargets(targets, ScanOptions{
		PackRoot:     packRoot,
		Lock:         lock,
		Index:        &index,
		Destinations: destinations,
		UsedMeta:     usedMeta,
		Algorithm:    scanAlgorithm,
		BatchSize:    scanBatchSize,
		SideOverride: scanSideOverride,
		DryRun:       scanDryRun,
		Overwrite:    scanOverwrite,
		Prune:        scanPrune,
	})

	if !scanDryRun && result.Written > 0 {
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

	printModrinthScanResult(result, scanDryRun, scanStrict)
}

// printModrinthScanResult prints the summary of a Modrinth pass.
func printModrinthScanResult(result ScanResult, dryRun bool, strict bool) {
	fmt.Println()
	if dryRun {
		fmt.Println("Dry run: no files were written.")
	}
	sort.Strings(result.Failed)
	sort.Strings(result.Unmatched)
	fmt.Printf("Matched on Modrinth: %d\n", result.Matched)
	fmt.Printf("Metadata files written: %d\n", result.Written)
	if result.Pruned > 0 {
		fmt.Printf("Local files pruned: %d\n", result.Pruned)
		fmt.Println("Note: pruned files are no longer on this server, so the pack-server fallback has")
		fmt.Println("      nothing to serve for them; keep a copy if you rely on that.")
	}
	if result.AlreadyPresent > 0 {
		fmt.Printf("Already had metadata: %d\n", result.AlreadyPresent)
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
		fmt.Printf("Not available on Modrinth: %d\n", len(result.Unmatched))
		printLimitedList(result.Unmatched)
		fmt.Println("  (run 'packwiz mod scan' to also try CurseForge)")
	}
	if strict && (len(result.Unmatched) > 0 || len(result.Failed) > 0 || len(result.SkippedOutside) > 0) {
		fmt.Println("Strict mode: some files could not be converted, failing.")
		os.Exit(1)
	}
}

func init() {
	modrinthCmd.AddCommand(scanCmd)

	scanCmd.Flags().BoolVar(&scanDryRun, "dry-run", false, "Report what would be written without writing anything")
	scanCmd.Flags().BoolVar(&scanOverwrite, "overwrite", false, "Also create metadata for files that already have metadata")
	scanCmd.Flags().BoolVar(&scanRecursive, "recursive", true, "Descend into subfolders")
	scanCmd.Flags().BoolVar(&scanPrune, "prune", false, "Delete the local file after its metadata has been written")
	scanCmd.Flags().StringVar(&scanSideOverride, "side", "", "Force the side of every added file: client, server or both")
	scanCmd.Flags().IntVar(&scanBatchSize, "batch-size", 100, "How many hashes to look up per Modrinth request")
	scanCmd.Flags().StringVar(&scanAlgorithm, "algorithm", "sha512", "Hash algorithm used for lookups: sha512 or sha1")
	scanCmd.Flags().BoolVar(&scanStrict, "strict", false, "Exit with a non-zero status if any scanned file could not be converted")
}

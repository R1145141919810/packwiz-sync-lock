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
	"github.com/BurntSushi/toml"
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
// Modrinth accepts the sha1 and sha512 algorithms for this endpoint.
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

func scanSide(version mrVersion) (string, bool) {
	// "either" means the project works on one side only, but Modrinth does not say which
	side := getSide(&modrinthApi.Version{Environment: version.Environment})
	if side == "either" || side == "" {
		return core.UniversalSide, true
	}
	return side, false
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
		"packwiz-installer understands - so clients download those files straight from Modrinth\n" +
		"instead of from your own file host.\n\n" +
		"Without folder arguments the locked folders from the [sync] table are scanned, falling\n" +
		"back to mods, plugins, resourcepacks and shaderpacks. Files outside of the lock are\n" +
		"never given metadata, so a scan cannot widen the lock.",
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

	folders, err := scanTargetFolders(args, lock, packRoot)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	metafileDestinations := make(map[string]string)
	usedMetaPaths := make(map[string]bool)
	var candidates []scanCandidate

	for _, folder := range folders {
		walkErr := filepath.WalkDir(folder, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == folder {
				return nil
			}
			if entry.IsDir() {
				if strings.HasPrefix(entry.Name(), ".") || !scanRecursive {
					return filepath.SkipDir
				}
				return nil
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				return nil
			}
			relPath, relErr := filepath.Rel(packRoot, path)
			if relErr != nil {
				return nil
			}
			relSlash := filepath.ToSlash(relPath)

			if strings.HasSuffix(name, core.MetaExtension) {
				usedMetaPaths[relSlash] = true
				dest, destErr := core.SyncLock{}.CheckMetaDest(relSlash, readMetaFilename(path))
				if destErr == nil && dest != "" {
					metafileDestinations[dest] = relSlash
				}
				return nil
			}
			if strings.HasSuffix(name, ".disabled") || strings.HasSuffix(name, ".toml") {
				return nil
			}
			if lock.IsActive() {
				if lockErr := lock.Check(relSlash); lockErr != nil {
					return nil
				}
			}
			sha1Sum, sha512Sum, hashErr := hashFileForScan(path)
			if hashErr != nil {
				fmt.Printf("Warning: could not hash %s: %s\n", relSlash, hashErr)
				return nil
			}
			candidates = append(candidates, scanCandidate{
				AbsPath:  path,
				RelPath:  relSlash,
				FileName: name,
				Sha1:     sha1Sum,
				Sha512:   sha512Sum,
			})
			return nil
		})
		if walkErr != nil {
			fmt.Println(walkErr)
			os.Exit(1)
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].RelPath < candidates[j].RelPath })
	fmt.Printf("Scanned %d file(s) in %s\n", len(candidates), strings.Join(folders, ", "))
	if len(candidates) == 0 {
		return
	}

	// Look every file up on Modrinth by hash, in batches
	uniqueHashes := make([]string, 0, len(candidates))
	seenHash := make(map[string]bool)
	for _, candidate := range candidates {
		hash := hashOfCandidate(candidate, scanAlgorithm)
		if !seenHash[hash] {
			seenHash[hash] = true
			uniqueHashes = append(uniqueHashes, hash)
		}
	}

	fmt.Printf("Looking up %d unique hash(es) on Modrinth...\n", len(uniqueHashes))
	matches := make(map[string]mrVersion)
	for _, chunk := range chunkStrings(uniqueHashes, scanBatchSize) {
		found, lookupErr := mrLookupVersionsByHash(chunk, scanAlgorithm)
		if lookupErr != nil {
			fmt.Printf("Failed to look up hashes: %s\n", lookupErr)
			os.Exit(1)
		}
		for hash, version := range found {
			matches[strings.ToLower(hash)] = version
		}
	}

	// Resolve the projects of every matched version (for the slug and the display name)
	projectIDs := make([]string, 0)
	seenProject := make(map[string]bool)
	for _, candidate := range candidates {
		version, ok := matches[hashOfCandidate(candidate, scanAlgorithm)]
		if !ok {
			continue
		}
		if !seenProject[version.ProjectID] {
			seenProject[version.ProjectID] = true
			projectIDs = append(projectIDs, version.ProjectID)
		}
	}
	projects := make(map[string]mrProject)
	for _, chunk := range chunkStrings(projectIDs, scanBatchSize) {
		found, projectErr := mrGetProjects(chunk)
		if projectErr != nil {
			fmt.Printf("Failed to look up projects: %s\n", projectErr)
			os.Exit(1)
		}
		for id, project := range found {
			projects[id] = project
		}
	}

	matched, written, alreadyPresent, pruned := 0, 0, 0, 0
	unmatched := make([]string, 0)
	failed := make([]string, 0)
	skippedOutsideLock := make([]string, 0)

	for _, candidate := range candidates {
		existingMetaRel := ""
		if metaRel, exists := metafileDestinations[candidate.RelPath]; exists {
			if !scanOverwrite {
				alreadyPresent++
				if scanPrune {
					if removeErr := os.Remove(candidate.AbsPath); removeErr == nil {
						pruned++
					}
				}
				continue
			}
			// --overwrite: refresh the metadata that is already there, in place
			existingMetaRel = metaRel
		}
		localHash := hashOfCandidate(candidate, scanAlgorithm)
		version, ok := matches[localHash]
		if !ok {
			unmatched = append(unmatched, candidate.RelPath)
			continue
		}
		project, ok := projects[version.ProjectID]
		if !ok {
			unmatched = append(unmatched, candidate.RelPath)
			continue
		}
		matched++

		// Find the exact file inside the version that matches the local file
		var matchedFile *mrFile
		for i := range version.Files {
			remoteHash, hasHash := version.Files[i].Hashes[scanAlgorithm]
			if hasHash && strings.EqualFold(remoteHash, localHash) {
				matchedFile = &version.Files[i]
				break
			}
		}
		if matchedFile == nil {
			failed = append(failed, candidate.RelPath+" (Modrinth did not return the matching file)")
			continue
		}

		metaRel := existingMetaRel
		if metaRel == "" {
			metaRel = uniqueMetaPath(candidate, project, usedMetaPaths, metafileDestinations)
		}
		if lock.IsActive() {
			if lockErr := lock.Check(metaRel); lockErr != nil {
				skippedOutsideLock = append(skippedOutsideLock, metaRel+" ("+lockErr.Error()+")")
				continue
			}
			if _, destErr := lock.CheckMetaDest(metaRel, candidate.FileName); destErr != nil {
				skippedOutsideLock = append(skippedOutsideLock, metaRel+" -> "+candidate.FileName+" ("+destErr.Error()+")")
				continue
			}
		}

		if scanDryRun {
			fmt.Printf("  would write %s (%s %s)\n", metaRel, project.Slug, version.VersionNumber)
			written++
			continue
		}

		side, ambiguous := scanSide(version)
		if scanSideOverride != "" {
			side = scanSideOverride
		} else if ambiguous {
			fmt.Printf("Warning: %s does not declare which side it belongs to; marking it as both\n", project.Title)
		}

		updateMap := make(map[string]map[string]interface{})
		updateMap["modrinth"], err = mrUpdateData{
			ProjectID:        version.ProjectID,
			InstalledVersion: version.ID,
		}.ToMap()
		if err != nil {
			failed = append(failed, candidate.RelPath+" ("+err.Error()+")")
			continue
		}

		modMeta := core.Mod{
			Name:     project.Title,
			FileName: candidate.FileName,
			Side:     side,
			Download: core.ModDownload{
				URL:        matchedFile.URL,
				HashFormat: scanAlgorithm,
				Hash:       matchedFile.Hashes[scanAlgorithm],
			},
			Update: updateMap,
		}
		metaAbs := filepath.Join(packRoot, filepath.FromSlash(metaRel))
		modMeta.SetMetaPath(metaAbs)
		format, hash, writeErr := modMeta.Write()
		if writeErr != nil {
			failed = append(failed, candidate.RelPath+" ("+writeErr.Error()+")")
			continue
		}
		if indexErr := index.RefreshFileWithHash(metaAbs, format, hash, true); indexErr != nil {
			failed = append(failed, candidate.RelPath+" ("+indexErr.Error()+")")
			continue
		}
		usedMetaPaths[metaRel] = true
		metafileDestinations[candidate.RelPath] = metaRel
		written++

		if scanPrune {
			if removeErr := os.Remove(candidate.AbsPath); removeErr != nil {
				fmt.Printf("Warning: could not remove %s: %s\n", candidate.RelPath, removeErr)
			} else {
				pruned++
			}
		}
	}

	if !scanDryRun && written > 0 {
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
	if scanDryRun {
		fmt.Println("Dry run: no files were written.")
	}
	fmt.Printf("Matched on Modrinth: %d\n", matched)
	fmt.Printf("Metadata files written: %d\n", written)
	if pruned > 0 {
		fmt.Printf("Local files pruned: %d\n", pruned)
	}
	if alreadyPresent > 0 {
		fmt.Printf("Already had metadata: %d\n", alreadyPresent)
	}
	if len(skippedOutsideLock) > 0 {
		fmt.Printf("Skipped because of the sync lock: %d\n", len(skippedOutsideLock))
		for _, entry := range skippedOutsideLock {
			fmt.Printf("  - %s\n", entry)
		}
	}
	if len(failed) > 0 {
		fmt.Printf("Failed: %d\n", len(failed))
		for _, entry := range failed {
			fmt.Printf("  - %s\n", entry)
		}
	}
	if len(unmatched) > 0 {
		fmt.Printf("Not available on Modrinth: %d\n", len(unmatched))
		printLimitedList(unmatched)
	}
	if !scanDryRun && written > 0 {
		fmt.Println()
		fmt.Println("The new metadata is already in index.toml. Commit it together with pack.toml.")
	}
	if scanStrict && (len(unmatched) > 0 || len(failed) > 0 || len(skippedOutsideLock) > 0) {
		fmt.Println("Strict mode: some files could not be converted, failing.")
		os.Exit(1)
	}
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

func hashOfCandidate(candidate scanCandidate, algorithm string) string {
	if algorithm == "sha1" {
		return strings.ToLower(candidate.Sha1)
	}
	return strings.ToLower(candidate.Sha512)
}

func readMetaFilename(path string) string {
	var meta struct {
		Filename string "toml:\"filename\""
	}
	if _, err := toml.DecodeFile(path, &meta); err != nil {
		return ""
	}
	return meta.Filename
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
	for attempt := 1; attempt < 100; attempt++ {
		name := base + core.MetaExtension
		if attempt > 1 {
			name = fmt.Sprintf("%s-%d%s", base, attempt, core.MetaExtension)
		}
		rel := name
		if dir != "." && dir != "" {
			rel = dir + "/" + name
		}
		if !used[rel] {
			if _, taken := destinations[rel]; !taken {
				return rel
			}
		}
	}
	return core.SlugifyName(project.Title) + "-" + fmt.Sprint(len(used)) + core.MetaExtension
}

// scanTargetFolders decides which folders to scan: explicit arguments first, then the locked
// folders, then the folders packwiz knows about that actually exist.
func scanTargetFolders(args []string, lock core.SyncLock, packRoot string) ([]string, error) {
	var requested []string
	if len(args) > 0 {
		requested = args
	} else if lock.IsActive() && !lock.AllowsAll() {
		requested = append(requested, lock.AllowedPaths...)
		fmt.Printf("Using the folders from the sync lock: %s\n", strings.Join(requested, ", "))
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
		return nil, fmt.Errorf("no folder to scan; pass one explicitly, for example: packwiz modrinth scan mods")
	}
	return folders, nil
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

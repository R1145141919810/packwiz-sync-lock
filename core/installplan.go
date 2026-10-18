package core

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/packwiz/packwiz/cfapi"
)

// installMetaFile is the part of a metadata (.pw.toml) file that a client needs in order to
// place the file it describes. It mirrors what packwiz-installer reads.
type installMetaFile struct {
	Name     string `toml:"name"`
	FileName string `toml:"filename"`
	Side     string `toml:"side"`
	Download struct {
		URL        string `toml:"url"`
		HashFormat string `toml:"hash-format"`
		Hash       string `toml:"hash"`
		Mode       string `toml:"mode"`
	} `toml:"download"`
	Option struct {
		Optional    bool   `toml:"optional"`
		Default     bool   `toml:"default"`
		Description string `toml:"description"`
	} `toml:"option"`
	// Update carries the per-source update blocks, e.g. [update.curseforge] file-id.
	Update map[string]installMetaUpdate `toml:"update"`
}

// installMetaUpdate is one [update.<source>] table of a metadata file.
type installMetaUpdate struct {
	FileID    int    `toml:"file-id"`
	ProjectID int    `toml:"project-id"`
	ModID     string `toml:"mod-id"`
	Version   string `toml:"version"`
}

// InstallTask is a single file that a client has to place.
type InstallTask struct {
	IndexPath  string
	Dest       string
	Name       string
	HashFormat string
	Hash       string
	SourceRel  string
	SourceURL  string
	Side       string
	Optional   bool
	Preserve   bool
	// CurseForgeFileID is set for "metadata:curseforge" entries. Their download URL is
	// resolved through the CurseForge API at download time, exactly like packwiz-installer.
	CurseForgeFileID int
}

// InstallPlan is everything a client has to do in order to install a pack.
type InstallPlan struct {
	Location      PackLocation
	Pack          Pack
	Index         Index
	Lock          SyncLock
	Tasks         []InstallTask
	Violations    []SyncViolation
	IndexExpected string
	IndexActual   string
	// FallbackToServer makes the client fall back to the pack server (same path as the
	// destination) when the mod host is unreachable, times out, or serves a wrong file.
	FallbackToServer bool
	// DownloadTimeout bounds a single download attempt before the fallback is tried.
	DownloadTimeout time.Duration
	cf              *cfResolver
	// EmptyHashes lists entries a client can never verify, which is what makes
	// packwiz-installer fail with "hash invalid". It is almost always caused by
	// no-internal-hashes being enabled while the pack was published.
	EmptyHashes []string
}

// IndexHashOK reports whether the served index matches the hash recorded in pack.toml.
func (p InstallPlan) IndexHashOK() bool {
	if strings.TrimSpace(p.IndexExpected) == "" {
		return true
	}
	return strings.EqualFold(p.IndexExpected, p.IndexActual)
}

// BuildInstallPlan turns a pack and its index into the list of files a client has to place.
// It never writes anything, so it is safe to run against a live instance.
func BuildInstallPlan(pack Pack, index Index, loc PackLocation, lock SyncLock, indexRaw []byte) InstallPlan {
	plan := InstallPlan{
		Location:    loc,
		Pack:        pack,
		Index:       index,
		Lock:        lock,
		Violations:  []SyncViolation{},
		EmptyHashes: []string{},
	}
	plan.FallbackToServer = lock.Download.FallbackEnabled()
	plan.DownloadTimeout = lock.Download.Timeout()
	plan.cf = &cfResolver{timeout: plan.DownloadTimeout}

	indexFormat := pack.Index.HashFormat
	if strings.TrimSpace(indexFormat) == "" {
		indexFormat = "sha256"
	}
	plan.IndexExpected = pack.Index.Hash
	if actual, _, err := HashReaderWithFormat(indexFormat, bytes.NewReader(indexRaw)); err == nil {
		plan.IndexActual = actual
	}

	paths := make([]string, 0, len(index.Files))
	for p := range index.Files {
		paths = append(paths, string(p))
	}
	sort.Strings(paths)

	// Metadata files are small but there can be hundreds of them, so fetch them in parallel
	metaData := make(map[string][]byte)
	metaErrors := make(map[string]error)
	metaPaths := make([]string, 0)
	for _, p := range paths {
		if index.Files[p].IsMetaFile() {
			metaPaths = append(metaPaths, p)
		}
	}
	if len(metaPaths) > 0 {
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for _, p := range metaPaths {
			wg.Add(1)
			go func(relPath string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				data, err := loc.ReadAll(relPath)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					metaErrors[relPath] = err
					return
				}
				metaData[relPath] = data
			}(p)
		}
		wg.Wait()
	}

	for _, p := range paths {
		holder := index.Files[p]

		// The entry path itself has to stay inside the pack folder, and inside the locked
		// folders when the pack locks any. This is the check that keeps a client from ever
		// being asked to write outside of them.
		if err := checkClientPath(lock, p); err != nil {
			plan.Violations = append(plan.Violations, SyncViolation{Path: p, Reason: err.Error()})
			continue
		}
		alias := holder.getPrimaryAlias()
		if alias != "" {
			if err := checkClientPath(lock, alias); err != nil {
				plan.Violations = append(plan.Violations, SyncViolation{
					Path:   p,
					Reason: fmt.Sprintf("alias %q: %s", alias, err),
				})
				continue
			}
		}

		entryFormat := holder.getHashFormat()
		if strings.TrimSpace(entryFormat) == "" {
			entryFormat = indexFormat
		}
		if strings.TrimSpace(holder.getHash()) == "" {
			plan.EmptyHashes = append(plan.EmptyHashes, p)
		}

		if !holder.IsMetaFile() {
			dest := alias
			if dest == "" {
				dest = p
			}
			plan.Tasks = append(plan.Tasks, InstallTask{
				IndexPath:  p,
				Dest:       dest,
				Name:       p,
				HashFormat: entryFormat,
				Hash:       holder.getHash(),
				SourceRel:  p,
				Preserve:   holder.isPreserve(),
			})
			continue
		}

		data, hasData := metaData[p]
		if !hasData {
			reason := "could not read the metadata file"
			if readErr, hasErr := metaErrors[p]; hasErr {
				reason += ": " + readErr.Error()
			}
			plan.Violations = append(plan.Violations, SyncViolation{Path: p, Reason: reason})
			continue
		}
		if expected := holder.getHash(); strings.TrimSpace(expected) != "" {
			actual, _, hashErr := HashReaderWithFormat(entryFormat, bytes.NewReader(data))
			if hashErr != nil || !strings.EqualFold(actual, expected) {
				plan.Violations = append(plan.Violations, SyncViolation{
					Path: p,
					Reason: fmt.Sprintf("the metadata file does not match the index (expected %s %s, got %s); run 'packwiz refresh' and publish the pack again",
						entryFormat, expected, actual),
				})
				continue
			}
		}
		var meta installMetaFile
		if _, err := toml.Decode(string(data), &meta); err != nil {
			plan.Violations = append(plan.Violations, SyncViolation{
				Path:   p,
				Reason: "could not parse the metadata file: " + err.Error(),
			})
			continue
		}
		dest, err := lock.CheckMetaDest(p, meta.FileName)
		if err != nil {
			plan.Violations = append(plan.Violations, SyncViolation{Path: p, Dest: dest, Reason: err.Error()})
			continue
		}
		if alias != "" {
			dest = alias
		}
		curseForgeFileID := 0
		mode := strings.TrimSpace(meta.Download.Mode)
		switch {
		case mode == "" || mode == ModeURL:
			if strings.TrimSpace(meta.Download.URL) == "" {
				plan.Violations = append(plan.Violations, SyncViolation{
					Path:   p,
					Reason: "the metadata file does not have a download URL",
				})
				continue
			}
		case mode == ModeCF:
			// CurseForge entries carry a file ID instead of a URL; the client resolves it
			// through the CurseForge API, which is what packwiz-installer does too.
			update, ok := meta.Update["curseforge"]
			if !ok || update.FileID == 0 {
				plan.Violations = append(plan.Violations, SyncViolation{
					Path:   p,
					Reason: "the metadata file uses the CurseForge download mode but has no [update.curseforge] file-id",
				})
				continue
			}
			curseForgeFileID = update.FileID
		default:
			plan.Violations = append(plan.Violations, SyncViolation{
				Path:   p,
				Reason: fmt.Sprintf("download mode %q is not supported (run 'packwiz update' to rewrite this file)", mode),
			})
			continue
		}
		if strings.TrimSpace(meta.Download.Hash) == "" {
			plan.EmptyHashes = append(plan.EmptyHashes, p)
		}
		downloadFormat := meta.Download.HashFormat
		if strings.TrimSpace(downloadFormat) == "" {
			downloadFormat = "sha256"
		}
		name := meta.Name
		if strings.TrimSpace(name) == "" {
			name = dest
		}
		plan.Tasks = append(plan.Tasks, InstallTask{
			IndexPath:        p,
			Dest:             dest,
			Name:             name,
			HashFormat:       downloadFormat,
			Hash:             meta.Download.Hash,
			SourceURL:        meta.Download.URL,
			Side:             meta.Side,
			Optional:         meta.Option.Optional,
			Preserve:         holder.isPreserve(),
			CurseForgeFileID: curseForgeFileID,
		})
	}
	plan.cf.fileIDs = collectCurseForgeIDs(plan.Tasks)
	return plan
}

// collectCurseForgeIDs returns every distinct CurseForge file ID a plan needs.
func collectCurseForgeIDs(tasks []InstallTask) []int {
	seen := make(map[int]bool, len(tasks))
	ids := make([]int, 0, len(tasks))
	for _, task := range tasks {
		if task.CurseForgeFileID != 0 && !seen[task.CurseForgeFileID] {
			seen[task.CurseForgeFileID] = true
			ids = append(ids, task.CurseForgeFileID)
		}
	}
	return ids
}

// cfResolver resolves CurseForge file IDs once per plan and caches the answers.
type cfResolver struct {
	once    sync.Once
	fileIDs []int
	timeout time.Duration
	files   map[int]cfapi.FileInfo
	err     error
}

func (r *cfResolver) get(fileID int) (cfapi.FileInfo, error) {
	r.once.Do(func() {
		r.files, r.err = cfapi.ResolveFiles(r.fileIDs, r.timeout)
	})
	if r.err != nil {
		return cfapi.FileInfo{}, r.err
	}
	file, ok := r.files[fileID]
	if !ok {
		return cfapi.FileInfo{}, fmt.Errorf("the CurseForge API did not return file %d", fileID)
	}
	return file, nil
}

// checkClientPath validates a pack-relative path for a client: it must stay inside the pack
// folder, and it must be inside the locked folders when the pack locks any.
func checkClientPath(lock SyncLock, relPath string) error {
	if _, err := NormalizeRelPath(relPath); err != nil {
		return err
	}
	return lock.Check(relPath)
}

// OpenTaskSource opens the content of a task.
func (p InstallPlan) OpenTaskSource(task InstallTask) (io.ReadCloser, error) {
	if strings.TrimSpace(task.SourceURL) != "" {
		return OpenURL(task.SourceURL)
	}
	return p.Location.Open(task.SourceRel)
}

// DescribeTaskSource returns a human readable description of where a task comes from.
func (p InstallPlan) DescribeTaskSource(task InstallTask) string {
	if strings.TrimSpace(task.SourceURL) != "" {
		return task.SourceURL
	}
	return p.Location.Resolve(task.SourceRel)
}

// SafeTargetPath joins a pack-relative destination onto the instance folder and refuses
// anything that would end up outside of it. It is the last line of defence on the client.
func SafeTargetPath(targetDir string, destRel string) (string, error) {
	cleaned, err := NormalizeRelPath(destRel)
	if err != nil {
		return "", err
	}
	if cleaned == "." {
		return "", fmt.Errorf("refusing to install an entry without a destination path")
	}
	root, err := filepath.Abs(targetDir)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(root, filepath.FromSlash(cleaned))
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to write %q outside of the instance folder", destRel)
	}
	return joined, nil
}

// InstallOutcome describes what happened to a single task.
type InstallOutcome struct {
	Task       InstallTask
	Written    bool
	UpToDate   bool
	Skipped    bool
	SkipReason string
	Error      error
	// FallbackUsed is true when the mod host could not be used and the file came from the
	// pack server instead.
	FallbackUsed bool
	// FallbackReason explains why the mod host was not used.
	FallbackReason error
}

// ApplyTask downloads a task, verifies its hash and writes it below targetDir.
func (p InstallPlan) ApplyTask(task InstallTask, targetDir string, dryRun bool) InstallOutcome {
	outcome := InstallOutcome{Task: task}

	destAbs, err := SafeTargetPath(targetDir, task.Dest)
	if err != nil {
		outcome.Error = err
		return outcome
	}
	if task.Preserve {
		if _, statErr := os.Stat(destAbs); statErr == nil {
			outcome.Skipped = true
			outcome.SkipReason = "marked as preserve and already present"
			return outcome
		}
	}
	format := task.HashFormat
	if strings.TrimSpace(format) == "" {
		format = "sha256"
	}
	if task.Hash != "" {
		if matches, matchErr := FileMatchesHash(destAbs, format, task.Hash); matchErr == nil && matches {
			outcome.UpToDate = true
			return outcome
		}
	}
	if dryRun {
		outcome.Written = true
		return outcome
	}

	// Try the mod host first (Modrinth/CurseForge), then fall back to the pack server.
	primaryOpen, primaryDescription, primaryErr := p.primarySource(task)
	var downloadErr error
	if primaryErr != nil {
		downloadErr = primaryErr
	} else {
		downloadErr = p.downloadInto(destAbs, format, task.Hash, primaryOpen, primaryDescription)
	}
	if downloadErr == nil {
		outcome.Written = true
		return outcome
	}

	if p.FallbackToServer {
		fallbackOpen := func() (io.ReadCloser, error) { return p.Location.Open(task.Dest) }
		fallbackErr := p.downloadInto(destAbs, format, task.Hash, fallbackOpen, p.Location.Resolve(task.Dest))
		if fallbackErr == nil {
			outcome.Written = true
			outcome.FallbackUsed = true
			outcome.FallbackReason = downloadErr
			return outcome
		}
		outcome.Error = fmt.Errorf("%s (the fallback to the pack server also failed: %s)", downloadErr, fallbackErr)
		return outcome
	}
	outcome.Error = downloadErr
	return outcome
}

// SetDownloadPolicy overrides how downloads are attempted, after a plan has been built.
func (p *InstallPlan) SetDownloadPolicy(fallback bool, timeout time.Duration) {
	p.FallbackToServer = fallback
	p.DownloadTimeout = timeout
	if p.cf != nil {
		p.cf.timeout = timeout
	}
}

// primarySource returns the opener, a description and any preparation error for the source the
// pack prefers: a plain download URL, a CurseForge file, or the pack itself.
func (p InstallPlan) primarySource(task InstallTask) (func() (io.ReadCloser, error), string, error) {
	if strings.TrimSpace(task.SourceURL) != "" {
		url := task.SourceURL
		return func() (io.ReadCloser, error) { return OpenURLWithTimeout(url, p.DownloadTimeout) }, url, nil
	}
	if task.CurseForgeFileID != 0 {
		if p.cf == nil {
			return nil, "", fmt.Errorf("CurseForge metadata cannot be resolved in this context")
		}
		file, err := p.cf.get(task.CurseForgeFileID)
		if err != nil {
			return nil, "", fmt.Errorf("could not resolve CurseForge file %d: %w", task.CurseForgeFileID, err)
		}
		if file.DownloadURL == nil || strings.TrimSpace(*file.DownloadURL) == "" {
			return nil, "", fmt.Errorf("CurseForge file %d has no download URL (the author disabled third party downloads)", task.CurseForgeFileID)
		}
		url := *file.DownloadURL
		return func() (io.ReadCloser, error) { return OpenURLWithTimeout(url, p.DownloadTimeout) }, url, nil
	}
	rel := task.SourceRel
	return func() (io.ReadCloser, error) { return p.Location.Open(rel) }, p.Location.Resolve(rel), nil
}

// downloadInto streams one source into destAbs and verifies it against the expected hash.
func (p InstallPlan) downloadInto(destAbs string, format string, expected string, open func() (io.ReadCloser, error), description string) error {
	source, err := open()
	if err != nil {
		return fmt.Errorf("could not download %s: %w", description, err)
	}
	defer source.Close()

	if err := os.MkdirAll(filepath.Dir(destAbs), 0755); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(filepath.Dir(destAbs), ".packwiz-sync-*.tmp")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	discardTemp := func() {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
	}

	hasher, err := GetHashImpl(format)
	if err != nil {
		discardTemp()
		return err
	}
	if _, err := io.Copy(io.MultiWriter(tempFile, hasher), source); err != nil {
		discardTemp()
		return fmt.Errorf("failed while downloading %s: %w", description, err)
	}
	actual := hasher.HashToString(hasher.Sum(nil))
	if expected != "" && !strings.EqualFold(actual, expected) {
		discardTemp()
		return fmt.Errorf("hash invalid: %s served a file that hashes to %s %s, but the pack expects %s",
			description, format, actual, expected)
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, destAbs); err != nil {
		// Windows cannot rename over an existing file
		if removeErr := os.Remove(destAbs); removeErr == nil {
			err = os.Rename(tempPath, destAbs)
		}
		if err != nil {
			_ = os.Remove(tempPath)
			return err
		}
	}
	return nil
}

// PreflightResult is the outcome of checking one task without writing anything.
type PreflightResult struct {
	Task  InstallTask
	OK    bool
	Hash  string
	Bytes int64
	Error error
	// FallbackUsed is true when only the pack server copy worked.
	FallbackUsed   bool
	FallbackReason error
}

// VerifyTask downloads a task and checks that its content matches what the pack promises.
// It never writes to the instance, which makes it the authoring-side equivalent of what
// packwiz-installer does on a client - including its "hash invalid" failures.
func (p InstallPlan) VerifyTask(task InstallTask) PreflightResult {
	result := PreflightResult{Task: task}
	format := task.HashFormat
	if strings.TrimSpace(format) == "" {
		format = "sha256"
	}
	primaryOpen, primaryDescription, err := p.primarySource(task)
	if err == nil {
		err = p.verifyInto(primaryOpen, primaryDescription, format, task.Hash, &result)
	}
	if err == nil {
		result.OK = true
		return result
	}

	// If the client would fall back to the pack server, report that as a working-but-degraded
	// result instead of a hard failure.
	if p.FallbackToServer {
		fallbackOpen := func() (io.ReadCloser, error) { return p.Location.Open(task.Dest) }
		fallbackErr := p.verifyInto(fallbackOpen, p.Location.Resolve(task.Dest), format, task.Hash, &result)
		if fallbackErr == nil {
			result.OK = true
			result.FallbackUsed = true
			result.FallbackReason = err
			return result
		}
		result.Error = fmt.Errorf("%s (the fallback also failed: %s)", err, fallbackErr)
		return result
	}
	result.Error = err
	return result
}

// verifyInto streams a source and compares it against the expected hash.
func (p InstallPlan) verifyInto(open func() (io.ReadCloser, error), description string, format string, expected string, result *PreflightResult) error {
	source, err := open()
	if err != nil {
		return fmt.Errorf("could not download %s: %w", description, err)
	}
	defer source.Close()
	hasher, err := GetHashImpl(format)
	if err != nil {
		return err
	}
	written, err := io.Copy(hasher, source)
	result.Bytes = written
	if err != nil {
		return fmt.Errorf("failed while downloading %s: %w", description, err)
	}
	result.Hash = hasher.HashToString(hasher.Sum(nil))
	if strings.TrimSpace(expected) == "" {
		return nil
	}
	if !strings.EqualFold(result.Hash, expected) {
		return fmt.Errorf("hash invalid: the served file hashes to %s %s, but the pack expects %s", format, result.Hash, expected)
	}
	return nil
}

// RunPreflight verifies every task in parallel and returns the results in task order.
func (p InstallPlan) RunPreflight(tasks []InstallTask, workers int) []PreflightResult {
	if workers < 1 {
		workers = 8
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}
	results := make([]PreflightResult, len(tasks))
	if len(tasks) == 0 {
		return results
	}
	var wg sync.WaitGroup
	work := make(chan int)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range work {
				results[index] = p.VerifyTask(tasks[index])
			}
		}()
	}
	for i := range tasks {
		work <- i
	}
	close(work)
	wg.Wait()
	return results
}

// TaskMatchesSide reports whether a task belongs on the given side (client or server).
func TaskMatchesSide(task InstallTask, side string) bool {
	taskSide := strings.ToLower(strings.TrimSpace(task.Side))
	switch side {
	case "client":
		return taskSide == "" || taskSide == "both" || taskSide == "client"
	case "server":
		return taskSide == "" || taskSide == "both" || taskSide == "server"
	default:
		return true
	}
}

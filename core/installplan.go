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

	"github.com/BurntSushi/toml"
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
		if mode := strings.TrimSpace(meta.Download.Mode); mode != "" && mode != ModeURL {
			plan.Violations = append(plan.Violations, SyncViolation{
				Path:   p,
				Reason: fmt.Sprintf("download mode %q needs packwiz-installer (run 'packwiz update' to rewrite this file)", mode),
			})
			continue
		}
		if strings.TrimSpace(meta.Download.URL) == "" {
			plan.Violations = append(plan.Violations, SyncViolation{
				Path:   p,
				Reason: "the metadata file does not have a download URL",
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
			IndexPath:  p,
			Dest:       dest,
			Name:       name,
			HashFormat: downloadFormat,
			Hash:       meta.Download.Hash,
			SourceURL:  meta.Download.URL,
			Side:       meta.Side,
			Optional:   meta.Option.Optional,
			Preserve:   holder.isPreserve(),
		})
	}
	return plan
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

	source, err := p.OpenTaskSource(task)
	if err != nil {
		outcome.Error = fmt.Errorf("could not download %s: %w", p.DescribeTaskSource(task), err)
		return outcome
	}
	defer source.Close()

	if err := os.MkdirAll(filepath.Dir(destAbs), 0755); err != nil {
		outcome.Error = err
		return outcome
	}
	tempFile, err := os.CreateTemp(filepath.Dir(destAbs), ".packwiz-sync-*.tmp")
	if err != nil {
		outcome.Error = err
		return outcome
	}
	tempPath := tempFile.Name()
	discardTemp := func() {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
	}

	hasher, err := GetHashImpl(format)
	if err != nil {
		discardTemp()
		outcome.Error = err
		return outcome
	}
	if _, err := io.Copy(io.MultiWriter(tempFile, hasher), source); err != nil {
		discardTemp()
		outcome.Error = fmt.Errorf("failed while downloading %s: %w", p.DescribeTaskSource(task), err)
		return outcome
	}
	actual := hasher.HashToString(hasher.Sum(nil))
	if task.Hash != "" && !strings.EqualFold(actual, task.Hash) {
		discardTemp()
		outcome.Error = fmt.Errorf("hash invalid for %s: the downloaded file hashes to %s %s but the pack expects %s (source: %s)",
			task.Dest, format, actual, task.Hash, p.DescribeTaskSource(task))
		return outcome
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		outcome.Error = err
		return outcome
	}
	if err := os.Rename(tempPath, destAbs); err != nil {
		// Windows cannot rename over an existing file
		if removeErr := os.Remove(destAbs); removeErr == nil {
			err = os.Rename(tempPath, destAbs)
		}
		if err != nil {
			_ = os.Remove(tempPath)
			outcome.Error = err
			return outcome
		}
	}
	outcome.Written = true
	return outcome
}

// PreflightResult is the outcome of checking one task without writing anything.
type PreflightResult struct {
	Task  InstallTask
	OK    bool
	Hash  string
	Bytes int64
	Error error
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
	source, err := p.OpenTaskSource(task)
	if err != nil {
		result.Error = fmt.Errorf("could not download %s: %w", p.DescribeTaskSource(task), err)
		return result
	}
	defer source.Close()
	hasher, err := GetHashImpl(format)
	if err != nil {
		result.Error = err
		return result
	}
	written, err := io.Copy(hasher, source)
	result.Bytes = written
	if err != nil {
		result.Error = fmt.Errorf("failed while downloading %s: %w", p.DescribeTaskSource(task), err)
		return result
	}
	result.Hash = hasher.HashToString(hasher.Sum(nil))
	if strings.TrimSpace(task.Hash) == "" {
		result.OK = true
		return result
	}
	if !strings.EqualFold(result.Hash, task.Hash) {
		result.Error = fmt.Errorf("hash invalid: the served file hashes to %s %s, but the pack expects %s",
			format, result.Hash, task.Hash)
		return result
	}
	result.OK = true
	return result
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

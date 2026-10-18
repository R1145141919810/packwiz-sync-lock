package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// ScanTarget is a local file that a metadata scanner may be able to identify on a mod host.
type ScanTarget struct {
	AbsPath  string
	RelPath  string
	FileName string
}

// UniqueScanMetaPath picks a free metadata file path for a match.
//
// It prefers "<dir>/<base>.pw.toml" and falls back to a numbered suffix when that name is
// already taken, either by an existing metadata file or by a file of the same name.
func UniqueScanMetaPath(dir string, base string, used map[string]bool, destinations map[string]string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "mod"
	}
	for attempt := 1; attempt < 1000; attempt++ {
		name := base + MetaExtension
		if attempt > 1 {
			name = fmt.Sprintf("%s-%d%s", base, attempt, MetaExtension)
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
	return base + "-" + MetaExtension
}

// ScanFolders decides which folders a scanner should walk: explicit arguments first, then the
// locked folders, then the folders packwiz knows about that actually exist.
func ScanFolders(args []string, lock SyncLock, packRoot string) ([]string, error) {
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
		return nil, fmt.Errorf("no folder to scan; pass one explicitly, for example: packwiz mod scan mods")
	}
	return folders, nil
}

// WalkScanTargets walks the given folders (relative to packRoot or absolute) and returns the
// files a metadata scanner should look at.
//
// It also reports which files already have metadata (destinations) and which metadata paths
// are already in use, so a scanner can skip those and pick a free name. Files outside of the
// sync lock are never returned, which is what keeps a scan from widening the lock.
func WalkScanTargets(folders []string, packRoot string, lock SyncLock, recursive bool) ([]ScanTarget, map[string]string, map[string]bool, error) {
	targets := make([]ScanTarget, 0)
	destinations := make(map[string]string)
	usedMeta := make(map[string]bool)

	for _, folder := range folders {
		walkErr := filepath.WalkDir(folder, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == folder {
				return nil
			}
			if entry.IsDir() {
				if strings.HasPrefix(entry.Name(), ".") || !recursive {
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

			if strings.HasSuffix(name, MetaExtension) {
				usedMeta[relSlash] = true
				filename, readErr := readMetaFilenameLocal(path)
				if readErr == nil {
					if dest, destErr := (SyncLock{}).CheckMetaDest(relSlash, filename); destErr == nil && dest != "" {
						destinations[dest] = relSlash
					}
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
			targets = append(targets, ScanTarget{AbsPath: path, RelPath: relSlash, FileName: name})
			return nil
		})
		if walkErr != nil {
			return nil, nil, nil, walkErr
		}
	}
	return targets, destinations, usedMeta, nil
}

// MetaFilename reads the "filename" that a metadata file declares.
func MetaFilename(absPath string) (string, error) {
	return readMetaFilenameLocal(absPath)
}

// WriteScanMetaFile writes a metadata file that a scanner produced and records it in the index.
func WriteScanMetaFile(packRoot string, metaRel string, mod Mod, index *Index) error {
	metaAbs := filepath.Join(packRoot, filepath.FromSlash(metaRel))
	mod.SetMetaPath(metaAbs)
	format, hash, err := mod.Write()
	if err != nil {
		return err
	}
	return index.RefreshFileWithHash(metaAbs, format, hash, true)
}

// Package cfapi is a minimal CurseForge API client.
//
// packwiz-installer resolves "metadata:curseforge" entries by asking the CurseForge API for
// the download URL of a file ID. "packwiz sync" has to do the same thing, and it lives in
// core, which cannot import the curseforge package (that package imports core). Keeping the
// few calls we need in a dependency-free package avoids that cycle.
package cfapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const apiServer = "https://api.curseforge.com"

// defaultAPIKeyB64 is the same embedded key that packwiz and packwiz-installer ship with,
// so that packs created by this tool keep working with the stock installer.
const defaultAPIKeyB64 = "JDJhJDEwJHNBWVhqblU1N0EzSmpzcmJYM3JVdk92UWk2NHBLS3BnQ2VpbGc1TUM1UGNKL0RYTmlGWWxh"

// APIKey returns the CurseForge API key to authenticate with.
func APIKey() string {
	decoded, err := base64.StdEncoding.DecodeString(defaultAPIKeyB64)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// FileInfo is the part of a CurseForge file that we need.
type FileInfo struct {
	ID          int     `json:"id"`
	ModID       int     `json:"modId"`
	DisplayName string  `json:"displayName"`
	FileName    string  `json:"fileName"`
	DownloadURL *string `json:"downloadUrl"`
}

// MaxBatch is the largest number of file IDs the CurseForge API accepts in one request.
const MaxBatch = 100

// ResolveFiles asks CurseForge for the given file IDs.
//
// A nil DownloadURL means the author disabled third party downloads, which is reported as an
// error for that file by the caller.
func ResolveFiles(fileIDs []int, timeout time.Duration) (map[int]FileInfo, error) {
	resolved := make(map[int]FileInfo)
	if len(fileIDs) == 0 {
		return resolved, nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	for start := 0; start < len(fileIDs); start += MaxBatch {
		end := start + MaxBatch
		if end > len(fileIDs) {
			end = len(fileIDs)
		}
		batch := fileIDs[start:end]

		body, err := json.Marshal(struct {
			FileIDs []int `json:"fileIds"`
		}{FileIDs: batch})
		if err != nil {
			return resolved, err
		}

		req, err := http.NewRequest(http.MethodPost, apiServer+"/v1/mods/files", bytes.NewReader(body))
		if err != nil {
			return resolved, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "packwiz")
		req.Header.Set("X-API-Key", APIKey())

		client := &http.Client{Timeout: timeout}
		resp, err := client.Do(req)
		if err != nil {
			return resolved, fmt.Errorf("could not reach the CurseForge API: %w", err)
		}
		payload, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return resolved, readErr
		}
		if closeErr != nil {
			return resolved, closeErr
		}
		if resp.StatusCode != http.StatusOK {
			return resolved, fmt.Errorf("the CurseForge API returned HTTP %d", resp.StatusCode)
		}

		var decoded struct {
			Data []FileInfo `json:"data"`
		}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return resolved, fmt.Errorf("could not parse the CurseForge response: %w", err)
		}
		for _, file := range decoded.Data {
			resolved[file.ID] = file
		}
	}
	return resolved, nil
}

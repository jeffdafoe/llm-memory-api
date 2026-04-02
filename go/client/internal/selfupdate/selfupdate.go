// Package selfupdate checks GitHub Releases for a newer version of the
// binary and replaces itself in-place when one is found.
//
// On Windows a running exe cannot be overwritten, so the update renames
// the current binary to a .old temp file (allowed while running), writes
// the new binary to the original path, and cleans up the .old file on
// the next run.
package selfupdate

import (
    "archive/tar"
    "archive/zip"
    "compress/gzip"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "runtime"
    "strings"
    "time"
)

// Version is set at build time via ldflags:
//
//	-ldflags "-X github.com/jeffdafoe/llm-memory-api/go/client/internal/selfupdate.Version=0.1.1"
//
// When unset (development builds), update checking is skipped.
var Version = "dev"

const (
    owner = "jeffdafoe"
    repo  = "llm-memory-api"
)

// releaseResponse is the subset of the GitHub Releases API response we need.
type releaseResponse struct {
    TagName string  `json:"tag_name"`
    Assets  []asset `json:"assets"`
}

type asset struct {
    Name               string `json:"name"`
    BrowserDownloadURL string `json:"browser_download_url"`
}

// Check looks for a newer release on GitHub and applies it if found.
// Errors are non-fatal — printed to stderr but never returned. The
// caller should always proceed with normal operation regardless.
func Check() {
    if Version == "dev" {
        return
    }

    // Clean up .old binary from a previous update (Windows leaves it
    // behind because you can't delete a running exe, but it's deletable
    // once the old process has exited).
    cleanOld()

    latest, err := fetchLatestRelease()
    if err != nil {
        fmt.Fprintf(os.Stderr, "  Update check failed: %s\n", err)
        return
    }

    latestVersion := strings.TrimPrefix(latest.TagName, "v")
    if latestVersion == Version {
        return
    }

    // Find the right asset for this platform/arch
    assetName := buildAssetName(latestVersion)
    var downloadURL string
    for _, a := range latest.Assets {
        if a.Name == assetName {
            downloadURL = a.BrowserDownloadURL
            break
        }
    }
    if downloadURL == "" {
        fmt.Fprintf(os.Stderr, "  Update: no asset found for %s/%s (%s)\n", runtime.GOOS, runtime.GOARCH, assetName)
        return
    }

    fmt.Fprintf(os.Stderr, "  Updating memory-sync from v%s to v%s...\n", Version, latestVersion)

    if err := downloadAndReplace(downloadURL, assetName); err != nil {
        fmt.Fprintf(os.Stderr, "  Update failed: %s\n", err)
        return
    }

    fmt.Fprintf(os.Stderr, "  Updated to v%s\n", latestVersion)
}

// fetchLatestRelease calls the GitHub Releases API.
func fetchLatestRelease() (*releaseResponse, error) {
    url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)

    client := &http.Client{Timeout: 10 * time.Second}
    req, err := http.NewRequest("GET", url, nil)
    if err != nil {
        return nil, err
    }
    req.Header.Set("Accept", "application/vnd.github+json")
    req.Header.Set("User-Agent", "memory-sync/"+Version)

    resp, err := client.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode != 200 {
        return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
    }

    var release releaseResponse
    if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
        return nil, err
    }
    return &release, nil
}

// buildAssetName returns the expected archive filename for the current
// platform, matching the GoReleaser name_template.
func buildAssetName(version string) string {
    ext := "tar.gz"
    if runtime.GOOS == "windows" {
        ext = "zip"
    }
    return fmt.Sprintf("memory-sync_%s_%s_%s.%s", version, runtime.GOOS, runtime.GOARCH, ext)
}

// downloadAndReplace downloads the release archive, extracts the binary,
// and replaces the currently running executable.
func downloadAndReplace(url, assetName string) error {
    client := &http.Client{Timeout: 60 * time.Second}
    resp, err := client.Get(url)
    if err != nil {
        return fmt.Errorf("download: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != 200 {
        return fmt.Errorf("download returned %d", resp.StatusCode)
    }

    // Extract binary bytes from the archive
    var binaryData []byte
    if strings.HasSuffix(assetName, ".zip") {
        binaryData, err = extractFromZip(resp.Body)
    } else {
        binaryData, err = extractFromTarGz(resp.Body)
    }
    if err != nil {
        return fmt.Errorf("extract: %w", err)
    }

    // Replace the running binary
    return replaceBinary(binaryData)
}

// extractFromTarGz extracts the memory-sync binary from a .tar.gz archive.
func extractFromTarGz(r io.Reader) ([]byte, error) {
    gz, err := gzip.NewReader(r)
    if err != nil {
        return nil, err
    }
    defer gz.Close()

    tr := tar.NewReader(gz)
    for {
        header, err := tr.Next()
        if err == io.EOF {
            break
        }
        if err != nil {
            return nil, err
        }
        name := filepath.Base(header.Name)
        if name == "memory-sync" || name == "memory-sync.exe" {
            return io.ReadAll(tr)
        }
    }
    return nil, fmt.Errorf("memory-sync binary not found in archive")
}

// extractFromZip extracts the memory-sync binary from a .zip archive.
// Since zip needs random access, we buffer the entire response to a temp file.
func extractFromZip(r io.Reader) ([]byte, error) {
    tmp, err := os.CreateTemp("", "memory-sync-update-*.zip")
    if err != nil {
        return nil, err
    }
    defer os.Remove(tmp.Name())
    defer tmp.Close()

    size, err := io.Copy(tmp, r)
    if err != nil {
        return nil, err
    }

    zr, err := zip.NewReader(tmp, size)
    if err != nil {
        return nil, err
    }

    for _, f := range zr.File {
        name := filepath.Base(f.Name)
        if name == "memory-sync" || name == "memory-sync.exe" {
            rc, err := f.Open()
            if err != nil {
                return nil, err
            }
            defer rc.Close()
            return io.ReadAll(rc)
        }
    }
    return nil, fmt.Errorf("memory-sync binary not found in archive")
}

// replaceBinary swaps the currently running executable with new binary data.
// On Windows: rename current → .old, write new, clean up .old next run.
// On Unix: write to temp file in same dir, rename over current (atomic).
func replaceBinary(newBinary []byte) error {
    execPath, err := os.Executable()
    if err != nil {
        return fmt.Errorf("find executable path: %w", err)
    }
    execPath, err = filepath.EvalSymlinks(execPath)
    if err != nil {
        return fmt.Errorf("resolve symlinks: %w", err)
    }

    dir := filepath.Dir(execPath)

    if runtime.GOOS == "windows" {
        // Windows: can't overwrite running exe, but can rename it
        oldPath := execPath + ".old"
        _ = os.Remove(oldPath) // clean up any leftover from failed update
        if err := os.Rename(execPath, oldPath); err != nil {
            return fmt.Errorf("rename current binary: %w", err)
        }
        if err := os.WriteFile(execPath, newBinary, 0755); err != nil {
            // Try to restore the old binary
            _ = os.Rename(oldPath, execPath)
            return fmt.Errorf("write new binary: %w", err)
        }
        // .old will be cleaned up on next run by cleanOld()
    } else {
        // Unix: write to temp file, then atomic rename
        tmp, err := os.CreateTemp(dir, "memory-sync-new-*")
        if err != nil {
            return fmt.Errorf("create temp file: %w", err)
        }
        tmpPath := tmp.Name()

        if _, err := tmp.Write(newBinary); err != nil {
            tmp.Close()
            os.Remove(tmpPath)
            return fmt.Errorf("write temp file: %w", err)
        }
        tmp.Close()

        if err := os.Chmod(tmpPath, 0755); err != nil {
            os.Remove(tmpPath)
            return fmt.Errorf("chmod temp file: %w", err)
        }

        if err := os.Rename(tmpPath, execPath); err != nil {
            os.Remove(tmpPath)
            return fmt.Errorf("rename new binary into place: %w", err)
        }
    }

    return nil
}

// cleanOld removes the .old binary left behind by a previous Windows update.
func cleanOld() {
    execPath, err := os.Executable()
    if err != nil {
        return
    }
    execPath, _ = filepath.EvalSymlinks(execPath)
    oldPath := execPath + ".old"
    if _, err := os.Stat(oldPath); err == nil {
        _ = os.Remove(oldPath)
    }
}

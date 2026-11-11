// Package selfupdate checks GitHub Releases for a newer version of the
// binary and replaces itself in-place when one is found.
//
// On Windows a running exe cannot be overwritten, so the update renames
// the current binary to a .old temp file (allowed while running), writes
// the new binary to the original path, and cleans up the .old file on
// the next run.
//
// Before anything is extracted or replaced, the downloaded archive's
// SHA-256 must match its entry in the release's checksums.txt (written by
// GoReleaser, see .goreleaser.yml). A release with no checksums.txt, no
// entry for the archive, or a different digest is not applied: the error
// is printed and the current binary keeps running.
package selfupdate

import (
    "archive/tar"
    "archive/zip"
    "bytes"
    "compress/gzip"
    "crypto/sha256"
    "encoding/hex"
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

    // checksumsAssetName is GoReleaser's checksum.name_template. Each line
    // is "<sha256 hex>  <archive file name>".
    checksumsAssetName = "checksums.txt"

    // maxChecksumsSize caps how much of checksums.txt is read. The real
    // file is about 2 KB (one line per archive).
    maxChecksumsSize = 1 << 20
)

// These package-level variables are seams for the tests in
// selfupdate_test.go: the tests point apiBaseURL at a local fake server,
// swap replaceExecutable for a stub so the test binary is never
// overwritten, and capture output. Production code never changes them.
var (
    apiBaseURL        = "https://api.github.com"
    replaceExecutable = replaceBinary
    output            = io.Writer(os.Stderr)
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
// The binaryName parameter identifies which binary to update (e.g.,
// "memory-sync" or "discuss") — it must match the GoReleaser archive
// name prefix and the binary filename inside the archive.
// Errors are non-fatal — printed to stderr but never returned. The
// caller should always proceed with normal operation regardless.
func Check(binaryName string) {
    if Version == "dev" {
        return
    }

    // Clean up .old binary from a previous update (Windows leaves it
    // behind because you can't delete a running exe, but it's deletable
    // once the old process has exited).
    cleanOld()

    latest, err := fetchLatestRelease(binaryName)
    if err != nil {
        fmt.Fprintf(output, "  Update check failed: %s\n", err)
        return
    }

    latestVersion := strings.TrimPrefix(latest.TagName, "v")
    if latestVersion == Version {
        return
    }

    // Find the right asset for this platform/arch
    assetName := buildAssetName(binaryName, latestVersion)
    downloadURL := findAssetURL(latest.Assets, assetName)
    if downloadURL == "" {
        fmt.Fprintf(output, "  Update: no asset found for %s/%s (%s)\n", runtime.GOOS, runtime.GOARCH, assetName)
        return
    }

    // Without checksums.txt there is nothing to verify the archive
    // against, so the update is not applied.
    checksumsURL := findAssetURL(latest.Assets, checksumsAssetName)
    if checksumsURL == "" {
        fmt.Fprintf(output, "  Update failed: release v%s has no %s; keeping v%s\n", latestVersion, checksumsAssetName, Version)
        return
    }

    fmt.Fprintf(output, "  Updating %s from v%s to v%s...\n", binaryName, Version, latestVersion)

    if err := downloadAndReplace(binaryName, downloadURL, assetName, checksumsURL); err != nil {
        fmt.Fprintf(output, "  Update failed: %s; keeping v%s\n", err, Version)
        return
    }

    fmt.Fprintf(output, "  Updated to v%s\n", latestVersion)
}

// findAssetURL returns the download URL of the release asset with the
// given file name, or "" when the release has no such asset.
func findAssetURL(assets []asset, name string) string {
    for _, a := range assets {
        if a.Name == name {
            return a.BrowserDownloadURL
        }
    }
    return ""
}

// fetchLatestRelease calls the GitHub Releases API.
func fetchLatestRelease(binaryName string) (*releaseResponse, error) {
    url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", apiBaseURL, owner, repo)

    client := &http.Client{Timeout: 10 * time.Second}
    req, err := http.NewRequest("GET", url, nil)
    if err != nil {
        return nil, err
    }
    req.Header.Set("Accept", "application/vnd.github+json")
    req.Header.Set("User-Agent", binaryName+"/"+Version)

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
func buildAssetName(binaryName, version string) string {
    ext := "tar.gz"
    if runtime.GOOS == "windows" {
        ext = "zip"
    }
    return fmt.Sprintf("%s_%s_%s_%s.%s", binaryName, version, runtime.GOOS, runtime.GOARCH, ext)
}

// downloadAndReplace downloads the release archive, checks its SHA-256
// against the release's checksums.txt, extracts the binary, and replaces
// the currently running executable. Nothing is extracted or replaced
// unless the digest matches.
func downloadAndReplace(binaryName, url, assetName, checksumsURL string) error {
    client := &http.Client{Timeout: 60 * time.Second}

    // Fetch the expected digest first: it is a small file, and a release
    // without an entry for this archive fails before the big download.
    want, err := fetchExpectedDigest(client, checksumsURL, assetName)
    if err != nil {
        return err
    }

    resp, err := client.Get(url)
    if err != nil {
        return fmt.Errorf("download: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != 200 {
        return fmt.Errorf("download returned %d", resp.StatusCode)
    }

    // The archive goes to a temp file so it can be hashed in full before
    // any of it is unpacked (and zip needs random access anyway).
    // `defer` runs when this function returns, last-registered first:
    // the file is closed, then deleted (Windows cannot delete an open file).
    tmp, err := os.CreateTemp("", binaryName+"-update-*")
    if err != nil {
        return fmt.Errorf("create temp file: %w", err)
    }
    defer os.Remove(tmp.Name())
    defer tmp.Close()

    // io.MultiWriter sends every byte read from the response to both the
    // temp file and the SHA-256 hasher, so the file is read only once.
    hasher := sha256.New()
    size, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body)
    if err != nil {
        return fmt.Errorf("download: %w", err)
    }

    got := hasher.Sum(nil)
    if !bytes.Equal(got, want) {
        return fmt.Errorf("checksum mismatch for %s: %s says %s, download is %s",
            assetName, checksumsAssetName, hex.EncodeToString(want), hex.EncodeToString(got))
    }

    // Extract binary bytes from the verified archive
    var binaryData []byte
    if strings.HasSuffix(assetName, ".zip") {
        binaryData, err = extractFromZip(binaryName, tmp, size)
    } else {
        // The copy left the file offset at the end; rewind to read it again.
        if _, err := tmp.Seek(0, io.SeekStart); err != nil {
            return fmt.Errorf("rewind temp file: %w", err)
        }
        binaryData, err = extractFromTarGz(binaryName, tmp)
    }
    if err != nil {
        return fmt.Errorf("extract: %w", err)
    }

    // Replace the running binary
    return replaceExecutable(binaryName, binaryData)
}

// fetchExpectedDigest downloads checksums.txt and returns the SHA-256
// listed for assetName.
func fetchExpectedDigest(client *http.Client, checksumsURL, assetName string) ([]byte, error) {
    resp, err := client.Get(checksumsURL)
    if err != nil {
        return nil, fmt.Errorf("download %s: %w", checksumsAssetName, err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != 200 {
        return nil, fmt.Errorf("download %s returned %d", checksumsAssetName, resp.StatusCode)
    }

    body, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumsSize))
    if err != nil {
        return nil, fmt.Errorf("download %s: %w", checksumsAssetName, err)
    }
    return parseChecksums(body, assetName)
}

// parseChecksums finds assetName in a checksums.txt body. Each line is
// "<sha256 hex>  <file name>" (the sha256sum format; a leading "*" on the
// name marks binary mode and is ignored). It fails when the file has no
// entry for assetName, when the entry is not a SHA-256, or when two
// entries for assetName disagree.
func parseChecksums(body []byte, assetName string) ([]byte, error) {
    var found []byte
    for _, line := range strings.Split(string(body), "\n") {
        fields := strings.Fields(line)
        if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != assetName {
            continue
        }
        digest, err := hex.DecodeString(fields[0])
        if err != nil || len(digest) != sha256.Size {
            return nil, fmt.Errorf("%s has a malformed entry for %s", checksumsAssetName, assetName)
        }
        if found != nil && !bytes.Equal(found, digest) {
            return nil, fmt.Errorf("%s has conflicting entries for %s", checksumsAssetName, assetName)
        }
        found = digest
    }
    if found == nil {
        return nil, fmt.Errorf("%s has no entry for %s", checksumsAssetName, assetName)
    }
    return found, nil
}

// extractFromTarGz extracts the named binary from a .tar.gz archive.
func extractFromTarGz(binaryName string, r io.Reader) ([]byte, error) {
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
        if name == binaryName || name == binaryName+".exe" {
            return io.ReadAll(tr)
        }
    }
    return nil, fmt.Errorf("%s binary not found in archive", binaryName)
}

// extractFromZip extracts the named binary from a .zip archive.
// Zip needs random access (its index is at the end of the file), so it
// takes an io.ReaderAt — here the temp file the download was saved to.
func extractFromZip(binaryName string, r io.ReaderAt, size int64) ([]byte, error) {
    zr, err := zip.NewReader(r, size)
    if err != nil {
        return nil, err
    }

    for _, f := range zr.File {
        name := filepath.Base(f.Name)
        if name == binaryName || name == binaryName+".exe" {
            rc, err := f.Open()
            if err != nil {
                return nil, err
            }
            defer rc.Close()
            return io.ReadAll(rc)
        }
    }
    return nil, fmt.Errorf("%s binary not found in archive", binaryName)
}

// replaceBinary swaps the currently running executable with new binary data.
// On Windows: rename current → .old, write new, clean up .old next run.
// On Unix: write to temp file in same dir, rename over current (atomic).
func replaceBinary(binaryName string, newBinary []byte) error {
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
        tmp, err := os.CreateTemp(dir, binaryName+"-new-*")
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

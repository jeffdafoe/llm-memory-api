package selfupdate

// These tests run the update against a fake GitHub: an httptest server (a
// real HTTP server on a random local port, started and stopped by the
// test) that serves the "latest release" API response, the release archive
// and checksums.txt. The package-level seams in selfupdate.go point the
// code at that server and stub out the final binary swap, so the test
// binary on disk is never touched.

import (
    "archive/tar"
    "archive/zip"
    "bytes"
    "compress/gzip"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "net/http"
    "net/http/httptest"
    "os"
    "runtime"
    "strconv"
    "strings"
    "sync/atomic"
    "testing"
)

const (
    testBinary      = "mail-send"
    testOldVersion  = "1.0.0"
    testNewVersion  = "1.1.0"
    testBinaryBytes = "new mail-send binary contents"
)

// fakeRelease describes what the fake GitHub serves. A nil checksums
// leaves checksums.txt out of the release's asset list.
type fakeRelease struct {
    archive   []byte
    checksums []byte
    // checksumsStatus, when set, is the HTTP status for checksums.txt
    // (the asset is still listed in the release).
    checksumsStatus int
    // truncateArchive declares the full archive length but sends only half
    // of it, like a connection that drops mid-download.
    truncateArchive bool
}

// replaceCall records what the update tried to install.
type replaceCall struct {
    binaryName string
    data       []byte
}

// fakeGitHub is what a test inspects afterwards. replaced stays nil when
// the update never reached the binary swap.
type fakeGitHub struct {
    url             string
    out             bytes.Buffer
    replaced        *replaceCall
    archiveRequests atomic.Int32
}

// startFakeGitHub serves release under archiveName and points the package
// seams at it. The t.Cleanup calls restore the seams after the test, like
// a tearDown().
func startFakeGitHub(t *testing.T, archiveName string, release fakeRelease) *fakeGitHub {
    t.Helper()

    fake := &fakeGitHub{}
    mux := http.NewServeMux()
    server := httptest.NewServer(mux)
    t.Cleanup(server.Close)
    fake.url = server.URL

    assets := []asset{{Name: archiveName, BrowserDownloadURL: server.URL + "/download/" + archiveName}}
    if release.checksums != nil {
        assets = append(assets, asset{Name: checksumsAssetName, BrowserDownloadURL: server.URL + "/download/" + checksumsAssetName})
    }

    mux.HandleFunc("/repos/"+owner+"/"+repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
        json.NewEncoder(w).Encode(releaseResponse{TagName: "v" + testNewVersion, Assets: assets})
    })
    mux.HandleFunc("/download/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
        fake.archiveRequests.Add(1)
        if release.truncateArchive {
            // The server closes the connection when a handler writes less
            // than the Content-Length it declared.
            w.Header().Set("Content-Length", strconv.Itoa(len(release.archive)))
            w.Write(release.archive[:len(release.archive)/2])
            return
        }
        w.Write(release.archive)
    })
    mux.HandleFunc("/download/"+checksumsAssetName, func(w http.ResponseWriter, r *http.Request) {
        if release.checksumsStatus != 0 {
            w.WriteHeader(release.checksumsStatus)
            return
        }
        w.Write(release.checksums)
    })

    savedBase, savedReplace, savedOutput, savedVersion := apiBaseURL, replaceExecutable, output, Version
    t.Cleanup(func() {
        apiBaseURL, replaceExecutable, output, Version = savedBase, savedReplace, savedOutput, savedVersion
    })
    apiBaseURL = server.URL
    output = &fake.out
    Version = testOldVersion
    replaceExecutable = func(binaryName string, data []byte) error {
        fake.replaced = &replaceCall{binaryName: binaryName, data: data}
        return nil
    }

    return fake
}

// isolateTempDir points os.TempDir at an empty directory for this test
// (TMPDIR on Unix, TMP/TEMP on Windows), so the test can check that the
// update left no temp files behind.
func isolateTempDir(t *testing.T) string {
    t.Helper()
    dir := t.TempDir()
    t.Setenv("TMPDIR", dir)
    t.Setenv("TMP", dir)
    t.Setenv("TEMP", dir)
    return dir
}

func assertTempDirEmpty(t *testing.T, dir string) {
    t.Helper()
    entries, err := os.ReadDir(dir)
    if err != nil {
        t.Fatal(err)
    }
    for _, e := range entries {
        t.Errorf("temp file left behind: %s", e.Name())
    }
}

// hostFormat is the archive format the release uses for this platform.
func hostFormat() string {
    if runtime.GOOS == "windows" {
        return "zip"
    }
    return "tar.gz"
}

// buildArchive packs contents as the binary in the given format ("zip"
// or "tar.gz").
func buildArchive(t *testing.T, format, contents string) []byte {
    t.Helper()
    var buf bytes.Buffer
    if format == "zip" {
        zw := zip.NewWriter(&buf)
        f, err := zw.Create(testBinary + ".exe")
        if err != nil {
            t.Fatal(err)
        }
        f.Write([]byte(contents))
        if err := zw.Close(); err != nil {
            t.Fatal(err)
        }
        return buf.Bytes()
    }
    gz := gzip.NewWriter(&buf)
    tw := tar.NewWriter(gz)
    if err := tw.WriteHeader(&tar.Header{Name: testBinary, Mode: 0755, Size: int64(len(contents))}); err != nil {
        t.Fatal(err)
    }
    tw.Write([]byte(contents))
    if err := tw.Close(); err != nil {
        t.Fatal(err)
    }
    if err := gz.Close(); err != nil {
        t.Fatal(err)
    }
    return buf.Bytes()
}

func sha256Hex(data []byte) string {
    sum := sha256.Sum256(data)
    return hex.EncodeToString(sum[:])
}

// checksumsFile renders a checksums.txt the way GoReleaser does, with an
// unrelated line first so the lookup has to pick the right entry.
func checksumsFile(entries map[string]string) []byte {
    var b strings.Builder
    fmt.Fprintf(&b, "%s  %s\n", strings.Repeat("ab", 32), "discuss_"+testNewVersion+"_linux_amd64.tar.gz")
    for name, digest := range entries {
        fmt.Fprintf(&b, "%s  %s\n", digest, name)
    }
    return []byte(b.String())
}

// --- Check, end to end, with this platform's archive name ---

func TestCheckInstallsArchiveThatMatchesChecksum(t *testing.T) {
    tempDir := isolateTempDir(t)
    archive := buildArchive(t, hostFormat(), testBinaryBytes)
    archiveName := buildAssetName(testBinary, testNewVersion)
    fake := startFakeGitHub(t, archiveName, fakeRelease{
        archive:   archive,
        checksums: checksumsFile(map[string]string{archiveName: sha256Hex(archive)}),
    })

    Check(testBinary)

    if fake.replaced == nil {
        t.Fatalf("binary was not replaced; output:\n%s", &fake.out)
    }
    if fake.replaced.binaryName != testBinary || string(fake.replaced.data) != testBinaryBytes {
        t.Fatalf("replaced with %q / %q, want %q / %q", fake.replaced.binaryName, fake.replaced.data, testBinary, testBinaryBytes)
    }
    if !strings.Contains(fake.out.String(), "Updated to v"+testNewVersion) {
        t.Fatalf("output missing success line:\n%s", &fake.out)
    }
    assertTempDirEmpty(t, tempDir)
}

func TestCheckRejectsChecksumMismatch(t *testing.T) {
    tempDir := isolateTempDir(t)
    archive := buildArchive(t, hostFormat(), testBinaryBytes)
    archiveName := buildAssetName(testBinary, testNewVersion)
    // The digest of a different archive: what a swapped or corrupted
    // download looks like.
    other := buildArchive(t, hostFormat(), "tampered binary")
    fake := startFakeGitHub(t, archiveName, fakeRelease{
        archive:   archive,
        checksums: checksumsFile(map[string]string{archiveName: sha256Hex(other)}),
    })

    Check(testBinary)

    assertNotReplaced(t, fake, "checksum mismatch for "+archiveName)
    assertTempDirEmpty(t, tempDir)
}

func TestCheckRejectsMissingChecksumEntry(t *testing.T) {
    archive := buildArchive(t, hostFormat(), testBinaryBytes)
    archiveName := buildAssetName(testBinary, testNewVersion)
    fake := startFakeGitHub(t, archiveName, fakeRelease{
        archive:   archive,
        checksums: checksumsFile(map[string]string{"memory-sync_" + testNewVersion + "_linux_amd64.tar.gz": sha256Hex(archive)}),
    })

    Check(testBinary)

    assertNotReplaced(t, fake, "no entry for "+archiveName)
}

func TestCheckRejectsReleaseWithoutChecksumsFile(t *testing.T) {
    fake := startFakeGitHub(t, buildAssetName(testBinary, testNewVersion), fakeRelease{
        archive: buildArchive(t, hostFormat(), testBinaryBytes),
    })

    Check(testBinary)

    assertNotReplaced(t, fake, "has no checksums.txt")
}

func TestCheckRejectsUnreadableChecksumsFile(t *testing.T) {
    fake := startFakeGitHub(t, buildAssetName(testBinary, testNewVersion), fakeRelease{
        archive:         buildArchive(t, hostFormat(), testBinaryBytes),
        checksums:       []byte{},
        checksumsStatus: http.StatusNotFound,
    })

    Check(testBinary)

    assertNotReplaced(t, fake, "download checksums.txt returned 404")
}

// An oversized checksums.txt is rejected whole. Here the correct entry is
// on line 2 and a conflicting one sits past the size cap; reading only up
// to the cap would accept the first and never see the second.
func TestCheckRejectsOversizedChecksumsFile(t *testing.T) {
    archive := buildArchive(t, hostFormat(), testBinaryBytes)
    archiveName := buildAssetName(testBinary, testNewVersion)
    body := checksumsFile(map[string]string{archiveName: sha256Hex(archive)})
    padding := []byte(strings.Repeat("#\n", maxChecksumsSize/2))
    body = append(body, padding...)
    body = append(body, []byte(strings.Repeat("cd", 32)+"  "+archiveName+"\n")...)
    fake := startFakeGitHub(t, archiveName, fakeRelease{archive: archive, checksums: body})

    Check(testBinary)

    assertNotReplaced(t, fake, "checksums.txt is larger than")
    if n := fake.archiveRequests.Load(); n != 0 {
        t.Fatalf("archive was requested %d times; want 0", n)
    }
}

// assertNotReplaced checks that Check stopped before the binary swap, said
// why, and said the current version stays.
func assertNotReplaced(t *testing.T, fake *fakeGitHub, reason string) {
    t.Helper()
    if fake.replaced != nil {
        t.Fatalf("binary was replaced (%d bytes); want no update. Output:\n%s", len(fake.replaced.data), &fake.out)
    }
    if !strings.Contains(fake.out.String(), reason) {
        t.Fatalf("output does not contain %q:\n%s", reason, &fake.out)
    }
    if !strings.Contains(fake.out.String(), "keeping v"+testOldVersion) {
        t.Fatalf("output does not say the current version stays:\n%s", &fake.out)
    }
}

// --- downloadAndReplace, both archive formats on every platform ---

// runDownload serves archive and a checksums.txt that lists its real
// digest, then calls downloadAndReplace directly with a name in the given
// format, so the zip path also runs on Linux CI and tar.gz on Windows.
func runDownload(t *testing.T, format string, archive []byte, truncate bool) (*fakeGitHub, error) {
    t.Helper()
    archiveName := testBinary + "_" + testNewVersion + "_test." + format
    fake := startFakeGitHub(t, archiveName, fakeRelease{
        archive:         archive,
        checksums:       checksumsFile(map[string]string{archiveName: sha256Hex(archive)}),
        truncateArchive: truncate,
    })
    err := downloadAndReplace(testBinary, fake.url+"/download/"+archiveName, archiveName, fake.url+"/download/"+checksumsAssetName)
    return fake, err
}

func TestDownloadAndReplaceFormats(t *testing.T) {
    for _, format := range []string{"zip", "tar.gz"} {
        t.Run(format+" valid", func(t *testing.T) {
            tempDir := isolateTempDir(t)
            fake, err := runDownload(t, format, buildArchive(t, format, testBinaryBytes), false)
            if err != nil {
                t.Fatalf("unexpected error: %v", err)
            }
            if fake.replaced == nil || string(fake.replaced.data) != testBinaryBytes {
                t.Fatalf("binary not installed with the archive's contents: %+v", fake.replaced)
            }
            assertTempDirEmpty(t, tempDir)
        })

        // The checksum matches, but the archive itself is broken: the
        // update still has to fail cleanly at extraction.
        t.Run(format+" matching checksum, broken archive", func(t *testing.T) {
            tempDir := isolateTempDir(t)
            fake, err := runDownload(t, format, []byte("this is not an archive"), false)
            if err == nil || !strings.Contains(err.Error(), "extract") {
                t.Fatalf("got %v; want an extract error", err)
            }
            if fake.replaced != nil {
                t.Fatal("binary was replaced")
            }
            assertTempDirEmpty(t, tempDir)
        })

        t.Run(format+" archive without the binary", func(t *testing.T) {
            tempDir := isolateTempDir(t)
            var other []byte
            if format == "zip" {
                var buf bytes.Buffer
                zw := zip.NewWriter(&buf)
                f, _ := zw.Create("README.md")
                f.Write([]byte("readme"))
                zw.Close()
                other = buf.Bytes()
            } else {
                var buf bytes.Buffer
                gz := gzip.NewWriter(&buf)
                tw := tar.NewWriter(gz)
                tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0644, Size: 6})
                tw.Write([]byte("readme"))
                tw.Close()
                gz.Close()
                other = buf.Bytes()
            }
            fake, err := runDownload(t, format, other, false)
            if err == nil || !strings.Contains(err.Error(), "not found in archive") {
                t.Fatalf("got %v; want a not-found error", err)
            }
            if fake.replaced != nil {
                t.Fatal("binary was replaced")
            }
            assertTempDirEmpty(t, tempDir)
        })

        t.Run(format+" connection drops mid-download", func(t *testing.T) {
            tempDir := isolateTempDir(t)
            fake, err := runDownload(t, format, buildArchive(t, format, testBinaryBytes), true)
            if err == nil || !strings.Contains(err.Error(), "download") {
                t.Fatalf("got %v; want a download error", err)
            }
            if fake.replaced != nil {
                t.Fatal("binary was replaced")
            }
            assertTempDirEmpty(t, tempDir)
        })
    }
}

// --- checksums.txt parsing ---

func TestParseChecksums(t *testing.T) {
    const name = "mail-send_1.1.0_linux_amd64.tar.gz"
    digest := strings.Repeat("0f", 32)
    other := strings.Repeat("1e", 32)

    tests := []struct {
        label   string
        body    string
        want    string // expected hex digest; "" means an error is expected
        errText string
    }{
        {"plain entry", digest + "  " + name + "\n", digest, ""},
        {"uppercase hex and binary-mode marker", strings.ToUpper(digest) + " *" + name + "\n", digest, ""},
        {"windows line endings", other + "  other.zip\r\n" + digest + "  " + name + "\r\n", digest, ""},
        {"repeated identical entry", digest + "  " + name + "\n" + digest + "  " + name + "\n", digest, ""},
        {"name is only a prefix", digest + "  " + name + ".sig\n", "", "no entry"},
        {"empty file", "", "", "no entry"},
        {"short digest", "0f0f  " + name + "\n", "", "malformed entry"},
        {"not hex", strings.Repeat("zz", 32) + "  " + name + "\n", "", "malformed entry"},
        {"conflicting entries", digest + "  " + name + "\n" + other + "  " + name + "\n", "", "conflicting entries"},
    }
    for _, tc := range tests {
        t.Run(tc.label, func(t *testing.T) {
            got, err := parseChecksums([]byte(tc.body), name)
            if tc.want == "" {
                if err == nil || !strings.Contains(err.Error(), tc.errText) {
                    t.Fatalf("got %x, %v; want error containing %q", got, err, tc.errText)
                }
                return
            }
            if err != nil {
                t.Fatalf("unexpected error: %v", err)
            }
            if hex.EncodeToString(got) != tc.want {
                t.Fatalf("got %x, want %s", got, tc.want)
            }
        })
    }
}

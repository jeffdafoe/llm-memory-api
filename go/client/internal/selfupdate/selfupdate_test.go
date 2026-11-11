package selfupdate

// These tests run Check against a fake GitHub: an httptest server (a real
// HTTP server on a random local port, started and stopped by the test)
// that serves the "latest release" API response, the release archive and
// checksums.txt. The package-level seams in selfupdate.go point Check at
// that server and stub out the final binary swap, so the test binary on
// disk is never touched.

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
    "runtime"
    "strings"
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
}

// replaceCall records what Check tried to install. Nil means Check never
// reached the binary swap.
type replaceCall struct {
    binaryName string
    data       []byte
}

// startFakeGitHub serves release and points the package seams at it. It
// returns the captured output and a pointer to the recorded swap. The
// t.Cleanup calls restore the seams after the test, like a tearDown().
func startFakeGitHub(t *testing.T, release fakeRelease) (*bytes.Buffer, **replaceCall) {
    t.Helper()

    archiveName := buildAssetName(testBinary, testNewVersion)
    mux := http.NewServeMux()
    server := httptest.NewServer(mux)
    t.Cleanup(server.Close)

    assets := []asset{{Name: archiveName, BrowserDownloadURL: server.URL + "/download/" + archiveName}}
    if release.checksums != nil {
        assets = append(assets, asset{Name: checksumsAssetName, BrowserDownloadURL: server.URL + "/download/" + checksumsAssetName})
    }

    mux.HandleFunc("/repos/"+owner+"/"+repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
        json.NewEncoder(w).Encode(releaseResponse{TagName: "v" + testNewVersion, Assets: assets})
    })
    mux.HandleFunc("/download/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
        w.Write(release.archive)
    })
    mux.HandleFunc("/download/"+checksumsAssetName, func(w http.ResponseWriter, r *http.Request) {
        if release.checksumsStatus != 0 {
            w.WriteHeader(release.checksumsStatus)
            return
        }
        w.Write(release.checksums)
    })

    var recorded *replaceCall
    var out bytes.Buffer

    savedBase, savedReplace, savedOutput, savedVersion := apiBaseURL, replaceExecutable, output, Version
    t.Cleanup(func() {
        apiBaseURL, replaceExecutable, output, Version = savedBase, savedReplace, savedOutput, savedVersion
    })
    apiBaseURL = server.URL
    output = &out
    Version = testOldVersion
    replaceExecutable = func(binaryName string, data []byte) error {
        recorded = &replaceCall{binaryName: binaryName, data: data}
        return nil
    }

    return &out, &recorded
}

// buildArchive packs contents as the binary, in the format the release
// uses for this platform (zip on Windows, tar.gz elsewhere).
func buildArchive(t *testing.T, contents string) []byte {
    t.Helper()
    var buf bytes.Buffer
    if runtime.GOOS == "windows" {
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

func TestCheckInstallsArchiveThatMatchesChecksum(t *testing.T) {
    archive := buildArchive(t, testBinaryBytes)
    archiveName := buildAssetName(testBinary, testNewVersion)
    out, recorded := startFakeGitHub(t, fakeRelease{
        archive:   archive,
        checksums: checksumsFile(map[string]string{archiveName: sha256Hex(archive)}),
    })

    Check(testBinary)

    if *recorded == nil {
        t.Fatalf("binary was not replaced; output:\n%s", out)
    }
    if (*recorded).binaryName != testBinary || string((*recorded).data) != testBinaryBytes {
        t.Fatalf("replaced with %q / %q, want %q / %q", (*recorded).binaryName, (*recorded).data, testBinary, testBinaryBytes)
    }
    if !strings.Contains(out.String(), "Updated to v"+testNewVersion) {
        t.Fatalf("output missing success line:\n%s", out)
    }
}

func TestCheckRejectsChecksumMismatch(t *testing.T) {
    archive := buildArchive(t, testBinaryBytes)
    archiveName := buildAssetName(testBinary, testNewVersion)
    // The digest of a different archive: what a swapped or corrupted
    // download looks like.
    other := buildArchive(t, "tampered binary")
    out, recorded := startFakeGitHub(t, fakeRelease{
        archive:   archive,
        checksums: checksumsFile(map[string]string{archiveName: sha256Hex(other)}),
    })

    Check(testBinary)

    assertNotReplaced(t, out, *recorded, "checksum mismatch for "+archiveName)
}

func TestCheckRejectsMissingChecksumEntry(t *testing.T) {
    archive := buildArchive(t, testBinaryBytes)
    out, recorded := startFakeGitHub(t, fakeRelease{
        archive:   archive,
        checksums: checksumsFile(map[string]string{"memory-sync_" + testNewVersion + "_linux_amd64.tar.gz": sha256Hex(archive)}),
    })

    Check(testBinary)

    assertNotReplaced(t, out, *recorded, "no entry for "+buildAssetName(testBinary, testNewVersion))
}

func TestCheckRejectsReleaseWithoutChecksumsFile(t *testing.T) {
    out, recorded := startFakeGitHub(t, fakeRelease{archive: buildArchive(t, testBinaryBytes)})

    Check(testBinary)

    assertNotReplaced(t, out, *recorded, "has no checksums.txt")
}

func TestCheckRejectsUnreadableChecksumsFile(t *testing.T) {
    out, recorded := startFakeGitHub(t, fakeRelease{
        archive:         buildArchive(t, testBinaryBytes),
        checksums:       []byte{},
        checksumsStatus: http.StatusNotFound,
    })

    Check(testBinary)

    assertNotReplaced(t, out, *recorded, "download checksums.txt returned 404")
}

// assertNotReplaced checks that Check stopped before the binary swap, said
// why, and said the current version stays.
func assertNotReplaced(t *testing.T, out *bytes.Buffer, recorded *replaceCall, reason string) {
    t.Helper()
    if recorded != nil {
        t.Fatalf("binary was replaced (%d bytes); want no update. Output:\n%s", len(recorded.data), out)
    }
    if !strings.Contains(out.String(), reason) {
        t.Fatalf("output does not contain %q:\n%s", reason, out)
    }
    if !strings.Contains(out.String(), "keeping v"+testOldVersion) {
        t.Fatalf("output does not say the current version stays:\n%s", out)
    }
}

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

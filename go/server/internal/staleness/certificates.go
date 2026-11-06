package staleness

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Certificates is what CollectCertificates found: findings from
// AssessCertificate, failed checks as report lines, and how many
// certificates were read.
type Certificates struct {
	Findings []CertFinding `json:"findings"`
	Failures []string      `json:"failures"`
	Checked  int           `json:"checked"`
}

// CollectCertificates reads certbot's certificates under dir
// (/etc/letsencrypt). certbot keeps a renewal config renewal/<name>.conf for
// every certificate it manages and a directory live/<name>/ whose cert.pem
// links to the current issue; both are listed. A name in renewal/ with no
// readable live/<name>/cert.pem fails on the read, and a name in live/ with
// no renewal config is reported too, because certbot will never renew it.
// When neither directory exists the server does not use Let's Encrypt and
// there is nothing to check; any other error listing them is a failure.
func CollectCertificates(dir string, now time.Time) Certificates {
	out := Certificates{Findings: []CertFinding{}, Failures: []string{}}
	type source struct {
		name string
		pick func(fs.DirEntry) (string, bool)
	}
	sources := []source{
		{"renewal", func(e fs.DirEntry) (string, bool) {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".conf") {
				return strings.TrimSuffix(e.Name(), ".conf"), true
			}
			return "", false
		}},
		{"live", func(e fs.DirEntry) (string, bool) { return e.Name(), e.IsDir() }},
	}
	// Per source: the names, or nil when the directory does not exist or
	// could not be listed.
	found := map[string]map[string]bool{}
	renewalUnlisted := false
	for _, src := range sources {
		path := filepath.Join(dir, src.name)
		entries, err := os.ReadDir(path)
		if err != nil {
			reason := fsMessage(err, "scandir", path)
			if errors.Is(err, fs.ErrNotExist) {
				// Windows reports a file in the directory's place as not
				// found; libuv, and Linux, call it ENOTDIR.
				info, statErr := os.Stat(path)
				if statErr != nil || info.IsDir() {
					continue
				}
				reason = fmt.Sprintf("ENOTDIR: not a directory, scandir '%s'", path)
			}
			out.Failures = append(out.Failures, fmt.Sprintf("Could not list the TLS certificates in %s: %s", path, reason))
			if src.name == "renewal" {
				renewalUnlisted = true
			}
			continue
		}
		names := map[string]bool{}
		for _, e := range entries {
			if name, ok := src.pick(e); ok {
				names[name] = true
			}
		}
		found[src.name] = names
	}

	all := map[string]bool{}
	for _, names := range found {
		for n := range names {
			all[n] = true
		}
	}
	if (found["renewal"] != nil || found["live"] != nil) && len(all) == 0 {
		out.Failures = append(out.Failures, dir+" holds no certificates, so none were checked.")
	}
	// When renewal/ could not be listed, that failure already covers every
	// certificate; naming each one again would only repeat it.
	if found["live"] != nil && !renewalUnlisted {
		for _, name := range sortedNames(found["live"]) {
			if found["renewal"] == nil || !found["renewal"][name] {
				out.Failures = append(out.Failures, fmt.Sprintf("The TLS certificate `%s` has no renewal config in %s, so certbot will not renew it.", name, filepath.Join(dir, "renewal")))
			}
		}
	}

	for _, name := range sortedNames(all) {
		finding, err := readCertificate(filepath.Join(dir, "live", name, "cert.pem"), name, now)
		if err != nil {
			out.Failures = append(out.Failures, fmt.Sprintf("Could not read the TLS certificate `%s`: %s", name, err))
			continue
		}
		out.Checked++
		if finding != nil {
			out.Findings = append(out.Findings, *finding)
		}
	}
	return out
}

func sortedNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// readCertificate reads the first certificate in the file (PEM, else DER,
// as Node's X509Certificate did) and assesses it.
func readCertificate(path, name string, now time.Time) (*CertFinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New(fsMessage(err, "open", path))
	}
	der := []byte(nil)
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			der = block.Bytes
			break
		}
	}
	if der == nil {
		der = data
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return AssessCertificate(name, cert.NotBefore, cert.NotAfter, now)
}

// fsMessage is the text Node's fs errors carried: "<CODE>: <description>,
// <syscall> '<path>'" (a read error names no path). An error with no code
// Node would know keeps Go's text.
func fsMessage(err error, syscall, path string) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && pathErr.Op == "read" {
		syscall, path = "read", ""
	}
	code, desc, ok := errnoText(err)
	if !ok {
		return err.Error()
	}
	if path == "" {
		return fmt.Sprintf("%s: %s, %s", code, desc, syscall)
	}
	return fmt.Sprintf("%s: %s, %s '%s'", code, desc, syscall, path)
}

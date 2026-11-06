// Package staleness is the weekly server staleness check (Node's
// services/staleness.js, services/staleness-certificates.js and
// scripts/staleness-check.js): pending apt upgrades, runtimes near end of
// life, a pending reboot, and TLS certificates near expiry, mailed to an
// actor from `system`. It only reports; nothing is ever installed.
//
// This file is the pure part: the parsers, the assessments and the report
// text. check.go gathers the facts.
package staleness

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// EOLWarnDays: warn when a runtime's support ends within this many days.
const EOLWarnDays = 90

// CertWarnDays: warn when a certificate has at most this many days left.
// certbot renews a 90-day certificate at 30 days left, so one inside this
// window means its renewal has been failing for over a week. A shorter-lived
// certificate gets the same share of its lifetime (21 of 90 days), because
// certbot renews those later.
const CertWarnDays = 21

const certWarnFraction = float64(CertWarnDays) / 90

const dayMs = 86400000

// The apt line patterns are JavaScript's: \S is anything outside JS's \s
// set and . is anything but a line terminator, which RE2's shorthands
// (ASCII only, and . matching \r) do not give.
const (
	jsNotSpace = `[^\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
	jsAny      = `[^\n\r\x{2028}\x{2029}]`
)

// `apt-get -s dist-upgrade` (LC_ALL=C) prints one line per package it would
// install or remove:
//
//	Inst libssl3t64 [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])
//	Remv libold1 [1.0-1]
//
// A new package has no "[old]" part, a package from a source with no
// release file can have no origin, and while the simulation still has
// broken packages apt ends the line with their list (possibly empty), which
// is matched and dropped.
var (
	aptInstLine    = regexp.MustCompile(`^Inst (` + jsNotSpace + `+)(?: \[([^\]]*)\])? \((` + jsNotSpace + `+)(?: (` + jsAny + `*?))? \[[^\]]+\]\)(?: \[[^\]]*\])?$`)
	aptRemvLine    = regexp.MustCompile(`^Remv (` + jsNotSpace + `+)(?: \[([^\]]*)\])?(?: \[[^\]]*\])?$`)
	aptSummaryLine = regexp.MustCompile(`^(\d+) upgraded, (\d+) newly installed, (\d+) to remove and (\d+) not upgraded\.$`)
	securityOrigin = regexp.MustCompile(`(?i)security`)
)

// Upgrade is one Inst line. From is nil for a new package.
type Upgrade struct {
	Name     string  `json:"name"`
	From     *string `json:"from"`
	To       string  `json:"to"`
	Origins  string  `json:"origins"`
	Security bool    `json:"security"`
}

// Removal is one Remv line.
type Removal struct {
	Name string  `json:"name"`
	From *string `json:"from"`
}

// Apt is what ParseAptSimulation read. Problems lists output it could not
// account for (an unrecognised Inst/Remv line, a missing summary, counts
// that disagree with the lines), which the check reports as failed checks,
// so a change in apt's output can never read as a clean system.
type Apt struct {
	Upgrades []Upgrade `json:"upgrades"`
	Removals []Removal `json:"removals"`
	HeldBack float64   `json:"heldBack"`
	Problems []string  `json:"problems"`
}

func optional(s string, present bool) *string {
	if !present {
		return nil
	}
	return &s
}

// ParseAptSimulation reads `apt-get -s dist-upgrade` output.
func ParseAptSimulation(output string) Apt {
	out := Apt{Upgrades: []Upgrade{}, Removals: []Removal{}, Problems: []string{}}
	var summary []float64
	for _, raw := range strings.Split(output, "\n") {
		line := jsutil.Trim(raw)
		switch {
		case strings.HasPrefix(line, "Inst "):
			m := aptInstLine.FindStringSubmatchIndex(line)
			if m == nil {
				out.Problems = append(out.Problems, "unrecognised apt line: "+line)
				continue
			}
			origins := ""
			if m[8] >= 0 {
				origins = line[m[8]:m[9]]
			}
			// The "[old]" group can match empty text ("[]"), which is a
			// version of "" in Node (`match[2] || null` makes it null).
			from := ""
			if m[4] >= 0 {
				from = line[m[4]:m[5]]
			}
			out.Upgrades = append(out.Upgrades, Upgrade{
				Name:    line[m[2]:m[3]],
				From:    optional(from, from != ""),
				To:      line[m[6]:m[7]],
				Origins: origins,
				// Debian publishes security fixes from "<codename>-security",
				// which apt shows as "Debian-Security:13/stable-security".
				Security: securityOrigin.MatchString(origins),
			})
		case strings.HasPrefix(line, "Remv "):
			m := aptRemvLine.FindStringSubmatchIndex(line)
			if m == nil {
				out.Problems = append(out.Problems, "unrecognised apt line: "+line)
				continue
			}
			from := ""
			if m[4] >= 0 {
				from = line[m[4]:m[5]]
			}
			out.Removals = append(out.Removals, Removal{Name: line[m[2]:m[3]], From: optional(from, from != "")})
		default:
			if m := aptSummaryLine.FindStringSubmatch(line); m != nil {
				summary = make([]float64, 4)
				for i := range summary {
					summary[i] = jsutil.StringToNumber(m[i+1])
				}
			}
		}
	}

	if summary == nil {
		out.Problems = append(out.Problems, `apt printed no "N upgraded, N newly installed, ..." summary line`)
		return out
	}
	upgraded, newlyInstalled, toRemove := summary[0], summary[1], summary[2]
	out.HeldBack = summary[3]
	// An Inst line with an "[old]" version is an upgrade, one without is a
	// new package; each must match apt's own count.
	parsedUpgraded := 0
	for _, u := range out.Upgrades {
		if u.From != nil {
			parsedUpgraded++
		}
	}
	parsedNew := len(out.Upgrades) - parsedUpgraded
	if upgraded != float64(parsedUpgraded) || newlyInstalled != float64(parsedNew) || toRemove != float64(len(out.Removals)) {
		out.Problems = append(out.Problems, fmt.Sprintf("apt's summary (%s upgraded, %s newly installed, %s to remove) does not match the lines read (%d upgraded, %d newly installed, %d to remove)",
			jsutil.NumberString(upgraded), jsutil.NumberString(newlyInstalled), jsutil.NumberString(toRemove), parsedUpgraded, parsedNew, len(out.Removals)))
	}
	return out
}

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// daysUntil is whole days from today to an ISO date, negative once passed.
// A date that does not exist (Feb 30) is an error, as in Node, which read
// the parsed date back to catch Date.parse rolling it over.
func daysUntil(isoDate string, today time.Time) (int, error) {
	target, err := time.Parse("2006-01-02", isoDate)
	if err != nil || target.Format("2006-01-02") != isoDate {
		return 0, fmt.Errorf("not a calendar date: %s", isoDate)
	}
	// Seconds, not a Duration, which saturates near 292 years ("0000-01-01").
	return int(math.Round(float64(target.Unix()-utcDay(today).Unix()) / 86400)), nil
}

// EOLFinding is a runtime near or past end of life. Eol and DaysLeft are nil
// for a cycle that ended without a recorded date.
type EOLFinding struct {
	Label    string  `json:"label"`
	Cycle    string  `json:"cycle"`
	Eol      *string `json:"eol"`
	DaysLeft *int    `json:"daysLeft"`
	Ended    bool    `json:"ended"`
}

var isoDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// AssessEOL reads endoflife.date's `eol` field for a release cycle, given as
// the raw JSON value (nil when absent): a date when the end is known, false
// when none is announced, true when the cycle ended without a recorded date.
// It returns nil when there is nothing to warn about.
func AssessEOL(label, cycle string, eol []byte, today time.Time) (*EOLFinding, error) {
	text := strings.TrimSpace(string(eol))
	switch text {
	case "", "false", "null":
		return nil, nil
	case "true":
		return &EOLFinding{Label: label, Cycle: cycle, Ended: true}, nil
	}
	var date string
	if text[0] == '"' {
		if v, err := jsutil.Parse([]byte(text)); err == nil {
			date, _ = v.(string)
		}
	}
	if date == "" || !isoDateRe.MatchString(date) {
		shown, err := jsutil.Stringify([]byte(text))
		if err != nil {
			shown = text
		}
		return nil, fmt.Errorf("unexpected eol value for %s %s: %s", label, cycle, shown)
	}
	daysLeft, err := daysUntil(date, today)
	if err != nil {
		return nil, err
	}
	if daysLeft > EOLWarnDays {
		return nil, nil
	}
	return &EOLFinding{Label: label, Cycle: cycle, Eol: &date, DaysLeft: &daysLeft, Ended: daysLeft < 0}, nil
}

// CertFinding is a certificate near or past expiry. Days is whole days,
// rounded toward the expiry: 0 is under a day left (or since it expired).
type CertFinding struct {
	Name    string `json:"name"`
	Expires string `json:"expires"`
	Expired bool   `json:"expired"`
	Days    int    `json:"days"`
}

func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// AssessCertificate checks one certificate's notBefore / notAfter against
// now. It returns nil when there is nothing to warn about. A certificate
// that is not valid yet is as broken as an expired one, so it is an error,
// which the caller reports as a failed check.
func AssessCertificate(name string, validFrom, validTo, now time.Time) (*CertFinding, error) {
	for _, t := range []time.Time{validFrom, validTo, now} {
		if t.IsZero() {
			return nil, fmt.Errorf("certificate %s has no readable validity dates", name)
		}
	}
	from, to, at := validFrom.UnixMilli(), validTo.UnixMilli(), now.UnixMilli()
	if to <= from {
		return nil, fmt.Errorf("certificate %s has an invalid validity period", name)
	}
	if at < from {
		return nil, fmt.Errorf("certificate %s is not valid until %s", name, isoMillis(validFrom))
	}
	lifetimeDays := float64(to-from) / dayMs
	warnDays := math.Min(CertWarnDays, lifetimeDays*certWarnFraction)
	msLeft := to - at
	if float64(msLeft)/dayMs > warnDays {
		return nil, nil
	}
	abs := msLeft
	if abs < 0 {
		abs = -abs
	}
	return &CertFinding{
		Name:    name,
		Expires: isoMillis(validTo)[:10],
		Expired: msLeft <= 0,
		Days:    int(abs / dayMs),
	}, nil
}

// Kernel is a parsed Debian kernel release: its version numbers (as digit
// text) and its flavour.
type Kernel struct {
	Version []string `json:"version"`
	Flavour string   `json:"flavour"`
}

var (
	kernelCurrent = regexp.MustCompile(`^(\d+(?:\.\d+)+)\+[a-z0-9.~]+-([a-z0-9][a-z0-9-]*)$`)
	kernelOlder   = regexp.MustCompile(`^(\d+(?:\.\d+)+)-(\d+)-([a-z0-9][a-z0-9-]*)$`)
)

// ParseKernelRelease splits a release (`uname -r`, or the part after
// "vmlinuz-"):
//
//	6.12.113+deb13-amd64    → [6 12 113]    amd64
//	6.12.113+deb13-rt-amd64 → [6 12 113]    rt-amd64
//	6.1.0-25-amd64          → [6 1 0 25]    amd64 (pre-trixie: ABI number)
//
// The provenance tag after "+" says where a kernel was built, not which is
// newer, so it is dropped. Anything else is nil.
func ParseKernelRelease(release string) *Kernel {
	if m := kernelCurrent.FindStringSubmatch(release); m != nil {
		return &Kernel{Version: strings.Split(m[1], "."), Flavour: m[2]}
	}
	if m := kernelOlder.FindStringSubmatch(release); m != nil {
		return &Kernel{Version: append(strings.Split(m[1], "."), m[2]), Flavour: m[3]}
	}
	return nil
}

// stripZeros drops what Node's /^0+(?=\d)/ replace dropped: the leading
// zeros that a digit follows, so an all-zero run keeps its last zero.
func stripZeros(s string) string {
	for len(s) > 1 && s[0] == '0' && s[1] >= '0' && s[1] <= '9' {
		s = s[1:]
	}
	return s
}

// compareDigits compares digit strings as whole numbers of any length.
func compareDigits(a, b string) int {
	x, y := stripZeros(a), stripZeros(b)
	if len(x) != len(y) {
		return len(x) - len(y)
	}
	return strings.Compare(x, y)
}

// CompareKernelVersions compares parsed versions part by part; a missing
// part counts as 0.
func CompareKernelVersions(a, b []string) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		x, y := "0", "0"
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if order := compareDigits(x, y); order != 0 {
			return order
		}
	}
	return 0
}

// RebootInput are the facts AssessReboot reads.
type RebootInput struct {
	FlagExists       bool     `json:"flagExists"`
	FlagPackages     string   `json:"flagPackages"`
	RunningKernel    string   `json:"runningKernel"`
	InstalledKernels []string `json:"installedKernels"`
}

// Reboot is a pending reboot.
type Reboot struct {
	Reasons       []string `json:"reasons"`
	RunningKernel string   `json:"runningKernel"`
	NewestKernel  *string  `json:"newestKernel"`
	Packages      []string `json:"packages"`
}

// AssessReboot decides whether the server needs a reboot, from two signals,
// since Debian does not always create /var/run/reboot-required: the flag
// file (whose .pkgs list is read only beside it), and an installed kernel of
// the running kernel's flavour with a higher version. Other flavours (rt,
// cloud) never count. Which entry the bootloader picks is not read: Debian's
// GRUB boots the newest kernel unless pinned. Nil means no reboot pending.
func AssessReboot(in RebootInput) *Reboot {
	running := ParseKernelRelease(in.RunningKernel)
	var newest string
	var newestVersion []string
	if running != nil {
		for _, release := range in.InstalledKernels {
			parsed := ParseKernelRelease(release)
			if parsed == nil || parsed.Flavour != running.Flavour {
				continue
			}
			if CompareKernelVersions(parsed.Version, running.Version) <= 0 {
				continue
			}
			if newestVersion == nil || CompareKernelVersions(parsed.Version, newestVersion) > 0 {
				newest = release
				newestVersion = parsed.Version
			}
		}
	}
	if !in.FlagExists && newestVersion == nil {
		return nil
	}
	r := &Reboot{Reasons: []string{}, RunningKernel: in.RunningKernel, Packages: []string{}}
	if in.FlagExists {
		r.Reasons = append(r.Reasons, "/var/run/reboot-required is set")
	}
	if newestVersion != nil {
		r.NewestKernel = &newest
		r.Reasons = append(r.Reasons, fmt.Sprintf("kernel %s is installed but %s is running", newest, in.RunningKernel))
	}
	if in.FlagExists {
		seen := map[string]bool{}
		for _, p := range strings.Split(in.FlagPackages, "\n") {
			p = jsutil.Trim(p)
			if p != "" && !seen[p] {
				seen[p] = true
				r.Packages = append(r.Packages, p)
			}
		}
	}
	return r
}

func plural(count int, word string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, word)
	}
	return fmt.Sprintf("%d %ss", count, word)
}

func describeCertificate(f CertFinding) string {
	if f.Expired {
		if f.Days == 0 {
			return fmt.Sprintf("`%s`: EXPIRED today (%s).", f.Name, f.Expires)
		}
		return fmt.Sprintf("`%s`: EXPIRED on %s, %s ago.", f.Name, f.Expires, plural(f.Days, "day"))
	}
	if f.Days == 0 {
		return fmt.Sprintf("`%s`: expires in under a day (%s).", f.Name, f.Expires)
	}
	return fmt.Sprintf("`%s`: expires %s, in %s.", f.Name, f.Expires, plural(f.Days, "day"))
}

func describeEOL(f EOLFinding) string {
	name := f.Label + " " + f.Cycle
	switch {
	case f.Eol == nil:
		return name + ": no longer supported (end date not recorded)."
	case f.Ended:
		return fmt.Sprintf("%s: support ENDED on %s, %d days ago.", name, *f.Eol, -*f.DaysLeft)
	case *f.DaysLeft == 0:
		return fmt.Sprintf("%s: support ends TODAY (%s).", name, *f.Eol)
	}
	return fmt.Sprintf("%s: support ends %s, in %d days.", name, *f.Eol, *f.DaysLeft)
}

func describeUpgrade(u Upgrade) string {
	from := "new: "
	if u.From != nil && *u.From != "" {
		from = *u.From + " → "
	}
	return fmt.Sprintf("`%s` %s%s", u.Name, from, u.To)
}

// ReportInput is one run's findings.
type ReportInput struct {
	Host         string        `json:"host"`
	Upgrades     []Upgrade     `json:"upgrades"`
	Removals     []Removal     `json:"removals"`
	HeldBack     float64       `json:"heldBack"`
	EOLFindings  []EOLFinding  `json:"eolFindings"`
	Reboot       *Reboot       `json:"reboot"`
	CertFindings []CertFinding `json:"certFindings"`
	Failures     []string      `json:"failures"`
}

// Report is the mail for one run.
type Report struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// BuildReport builds the mail, or nil when nothing needs action, so a clean
// week sends nothing. Failures are checks that could not run; they are
// reported, because a check that silently failed would read as all clear.
func BuildReport(in ReportInput) *Report {
	heldBack := in.HeldBack != 0 && !math.IsNaN(in.HeldBack)
	if len(in.Upgrades) == 0 && len(in.Removals) == 0 && !heldBack && len(in.EOLFindings) == 0 &&
		in.Reboot == nil && len(in.CertFindings) == 0 && len(in.Failures) == 0 {
		return nil
	}

	var security, node, other []Upgrade
	for _, u := range in.Upgrades {
		switch {
		case u.Security:
			security = append(security, u)
		case u.Name == "nodejs":
			node = append(node, u)
		default:
			other = append(other, u)
		}
	}

	var counts []string
	if n := len(in.CertFindings); n > 0 {
		counts = append(counts, plural(n, "certificate warning"))
	}
	if n := len(in.EOLFindings); n > 0 {
		counts = append(counts, plural(n, "end-of-life warning"))
	}
	if in.Reboot != nil {
		counts = append(counts, "reboot needed")
	}
	if n := len(security); n > 0 {
		counts = append(counts, plural(n, "security update"))
	}
	if len(node) > 0 {
		counts = append(counts, "Node.js update")
	}
	if n := len(other); n > 0 {
		counts = append(counts, plural(n, "other update"))
	}
	if n := len(in.Removals); n > 0 {
		counts = append(counts, plural(n, "removal"))
	}
	if heldBack {
		counts = append(counts, jsutil.NumberString(in.HeldBack)+" held back")
	}
	if n := len(in.Failures); n > 0 {
		counts = append(counts, plural(n, "failed check"))
	}

	lines := []string{fmt.Sprintf("Weekly staleness check for **%s**. Nothing was installed.", in.Host), ""}
	// First, because an expired certificate takes a site down at once.
	if len(in.CertFindings) > 0 {
		lines = append(lines, "## TLS certificates", "")
		for _, f := range in.CertFindings {
			lines = append(lines, "- "+describeCertificate(f))
		}
		lines = append(lines, "", "certbot renews well before this point, so its renewal is failing: `sudo certbot renew --dry-run` and `sudo journalctl -u certbot` show why. A certificate for a site no longer hosted here should be removed with `sudo certbot delete --cert-name <name>`.", "")
	}
	if len(in.EOLFindings) > 0 {
		lines = append(lines, "## End of life", "")
		for _, f := range in.EOLFindings {
			lines = append(lines, "- "+describeEOL(f))
		}
		lines = append(lines, "", "Plan each move to a supported version before its date.", "")
	}
	if in.Reboot != nil {
		lines = append(lines, "## Reboot needed", "")
		for _, reason := range in.Reboot.Reasons {
			lines = append(lines, "- "+upperFirst(reason)+".")
		}
		if len(in.Reboot.Packages) > 0 {
			quoted := make([]string, len(in.Reboot.Packages))
			for i, p := range in.Reboot.Packages {
				quoted[i] = "`" + p + "`"
			}
			lines = append(lines, "- Asked for by: "+strings.Join(quoted, ", ")+".")
		}
		lines = append(lines, "", "Installed fixes in these packages are not in effect until the server restarts. Reboot with `sudo reboot`; every service on the server stops for about a minute.", "")
	}
	if len(security) > 0 {
		lines = append(lines, "## Security updates", "")
		for _, u := range security {
			lines = append(lines, "- "+describeUpgrade(u))
		}
		lines = append(lines, "")
	}
	if len(node) > 0 {
		lines = append(lines, "## Node.js", "")
		for _, u := range node {
			lines = append(lines, "- "+describeUpgrade(u))
		}
		lines = append(lines, "", "After installing, restart `memory-api` so it runs the new binary.", "")
	}
	if len(other) > 0 {
		lines = append(lines, "## Other updates", "")
		for _, u := range other {
			lines = append(lines, "- "+describeUpgrade(u))
		}
		lines = append(lines, "")
	}
	if len(in.Removals) > 0 {
		lines = append(lines, "## Packages the upgrade would remove", "")
		for _, r := range in.Removals {
			line := "- `" + r.Name + "`"
			if r.From != nil && *r.From != "" {
				line += " " + *r.From
			}
			lines = append(lines, line)
		}
		lines = append(lines, "")
	}
	if heldBack {
		what := "s have updates"
		if in.HeldBack == 1 {
			what = " has an update"
		}
		lines = append(lines, "## Held back", "",
			jsutil.NumberString(in.HeldBack)+" package"+what+" that the upgrade would not install (held with `apt-mark hold`, or blocked by dependencies). `apt list --upgradable` and `apt-mark showhold` show which.", "")
	}
	if len(in.Failures) > 0 {
		lines = append(lines, "## Checks that failed", "")
		for _, f := range in.Failures {
			lines = append(lines, "- "+f)
		}
		lines = append(lines, "")
	}
	if len(in.Upgrades) > 0 || len(in.Removals) > 0 {
		// full-upgrade follows the same plan as the dist-upgrade simulation,
		// removals included; plain `apt upgrade` would not.
		lines = append(lines, "To apply this plan, run `sudo apt full-upgrade` on the server.")
	}

	return &Report{
		Subject: fmt.Sprintf("Staleness check (%s): %s", in.Host, strings.Join(counts, ", ")),
		Body:    trimEndJS(strings.Join(lines, "\n")) + "\n",
	}
}

// upperFirst is `s[0].toUpperCase() + s.slice(1)` for the reason texts,
// which start with ASCII.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// trimEndJS is String.prototype.trimEnd.
func trimEndJS(s string) string {
	return strings.TrimRightFunc(s, jsutil.IsSpace)
}

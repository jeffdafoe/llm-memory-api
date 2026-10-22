// Weekly server staleness check: pending apt upgrades and end-of-life
// warnings for the runtimes this service depends on (Node.js, Debian,
// PostgreSQL).
//
// This module is pure — no shell, network or database. The cron entry point
// (scripts/staleness-check.js) gathers the facts and hands them in here, so
// the decisions can be unit-tested. Nothing in this check ever installs a
// package: it only reports, and a human runs `apt upgrade` deliberately.

// Warn when a runtime's security support ends within this many days.
const EOL_WARN_DAYS = 90;

// `apt-get -s dist-upgrade` (run with LC_ALL=C) prints one line per package
// it would install or remove, e.g.
//   Inst libssl3t64 [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])
//   Inst nodejs [24.22.0-1nodesource1] (24.22.1-1nodesource1 Node Source:nodistro [amd64])
//   Remv libold1 [1.0-1]
// A package that is new (pulled in as a dependency) has no "[old]" part.
// Several origins are separated by ", " before the "[arch]"; a package from
// a source with no release file can have no origin at all.
const APT_INST_LINE = /^Inst (\S+)(?: \[([^\]]*)\])? \((\S+)(?: (.*?))? \[[^\]]+\]\)$/;
const APT_REMV_LINE = /^Remv (\S+)(?: \[([^\]]*)\])?$/;
// "4 upgraded, 1 newly installed, 0 to remove and 2 not upgraded."
const APT_SUMMARY_LINE = /^(\d+) upgraded, (\d+) newly installed, (\d+) to remove and (\d+) not upgraded\.$/;

// Returns { upgrades, removals, heldBack, problems }. `problems` lists output
// the parser could not account for — an unrecognised Inst/Remv line, a missing
// summary, or a summary whose counts disagree with the lines parsed. The
// caller reports those as failed checks, so a change in apt's output format
// can never look like a clean system.
function parseAptSimulation(output) {
    const upgrades = [];
    const removals = [];
    const problems = [];
    let summary = null;

    for (const raw of String(output || '').split('\n')) {
        const line = raw.trim();
        if (line.startsWith('Inst ')) {
            const match = APT_INST_LINE.exec(line);
            if (!match) {
                problems.push(`unrecognised apt line: ${line}`);
                continue;
            }
            const origins = match[4] || '';
            upgrades.push({
                name: match[1],
                from: match[2] || null,
                to: match[3],
                origins,
                // Debian publishes security fixes from the "<codename>-security"
                // suite, which apt shows as "Debian-Security:13/stable-security".
                security: /security/i.test(origins),
            });
        } else if (line.startsWith('Remv ')) {
            const match = APT_REMV_LINE.exec(line);
            if (!match) {
                problems.push(`unrecognised apt line: ${line}`);
                continue;
            }
            removals.push({ name: match[1], from: match[2] || null });
        } else {
            const match = APT_SUMMARY_LINE.exec(line);
            if (match) summary = match.slice(1, 5).map(Number);
        }
    }

    let heldBack = 0;
    if (!summary) {
        problems.push('apt printed no "N upgraded, N newly installed, ..." summary line');
    } else {
        const [upgraded, newlyInstalled, toRemove, notUpgraded] = summary;
        heldBack = notUpgraded;
        // An Inst line with an "[old]" version is an upgrade; without one it
        // is a new package. Each category must match apt's own count.
        const parsedUpgraded = upgrades.filter((u) => u.from !== null).length;
        const parsedNew = upgrades.length - parsedUpgraded;
        if (upgraded !== parsedUpgraded || newlyInstalled !== parsedNew || toRemove !== removals.length) {
            problems.push(`apt's summary (${upgraded} upgraded, ${newlyInstalled} newly installed, ${toRemove} to remove) does not match the lines read (${parsedUpgraded} upgraded, ${parsedNew} newly installed, ${removals.length} to remove)`);
        }
    }
    return { upgrades, removals, heldBack, problems };
}

// Whole days from `today` to an ISO date (YYYY-MM-DD), negative once passed.
function daysUntil(isoDate, today) {
    const target = Date.parse(`${isoDate}T00:00:00Z`);
    // Date.parse accepts some impossible dates by rolling them over, so also
    // require the parsed date to read back as the same string.
    if (!Number.isFinite(target) || new Date(target).toISOString().slice(0, 10) !== isoDate) {
        throw new Error(`not a calendar date: ${isoDate}`);
    }
    const start = Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), today.getUTCDate());
    return Math.round((target - start) / 86400000);
}

// `eol` is the field endoflife.date returns for a release cycle: a date string
// when the end is known, `false` when no end is announced, `true` when the
// cycle has ended without a recorded date. Returns a finding, or null when
// there is nothing to warn about.
function assessEol(label, cycle, eol, today, warnDays = EOL_WARN_DAYS) {
    if (eol === false || eol === null || eol === undefined) return null;
    if (eol === true) {
        return { label, cycle, eol: null, daysLeft: null, ended: true };
    }
    if (typeof eol !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(eol)) {
        throw new Error(`unexpected eol value for ${label} ${cycle}: ${JSON.stringify(eol)}`);
    }
    const daysLeft = daysUntil(eol, today);
    if (daysLeft > warnDays) return null;
    return { label, cycle, eol, daysLeft, ended: daysLeft < 0 };
}

function describeEol(finding) {
    const name = `${finding.label} ${finding.cycle}`;
    if (finding.eol === null) return `${name}: no longer supported (end date not recorded).`;
    if (finding.ended) return `${name}: support ENDED on ${finding.eol}, ${-finding.daysLeft} days ago.`;
    if (finding.daysLeft === 0) return `${name}: support ends TODAY (${finding.eol}).`;
    return `${name}: support ends ${finding.eol}, in ${finding.daysLeft} days.`;
}

function describeUpgrade(upgrade) {
    const from = upgrade.from ? `${upgrade.from} → ` : 'new: ';
    return `\`${upgrade.name}\` ${from}${upgrade.to}`;
}

// Builds the mail for one run. Returns null when nothing needs action, so the
// caller sends no mail on a clean week. `failures` are checks that could not
// run (apt, a lookup); they are reported, because a check that silently fails
// would read as "all clear".
// `upgrades`, `removals` and `heldBack` come from parseAptSimulation.
function buildReport({ host, upgrades = [], removals = [], heldBack = 0, eolFindings = [], failures = [] }) {
    if (upgrades.length === 0 && removals.length === 0 && heldBack === 0
        && eolFindings.length === 0 && failures.length === 0) {
        return null;
    }

    const security = upgrades.filter((u) => u.security);
    const node = upgrades.filter((u) => !u.security && u.name === 'nodejs');
    const other = upgrades.filter((u) => !u.security && u.name !== 'nodejs');

    const counts = [];
    if (eolFindings.length) counts.push(`${eolFindings.length} end-of-life warning${eolFindings.length === 1 ? '' : 's'}`);
    if (security.length) counts.push(`${security.length} security update${security.length === 1 ? '' : 's'}`);
    if (node.length) counts.push('Node.js update');
    if (other.length) counts.push(`${other.length} other update${other.length === 1 ? '' : 's'}`);
    if (removals.length) counts.push(`${removals.length} removal${removals.length === 1 ? '' : 's'}`);
    if (heldBack) counts.push(`${heldBack} held back`);
    if (failures.length) counts.push(`${failures.length} failed check${failures.length === 1 ? '' : 's'}`);

    const lines = [
        `Weekly staleness check for **${host}**. Nothing was installed.`,
        '',
    ];
    if (eolFindings.length) {
        lines.push('## End of life', '');
        for (const finding of eolFindings) lines.push(`- ${describeEol(finding)}`);
        lines.push('', 'Plan each move to a supported version before its date.', '');
    }
    if (security.length) {
        lines.push('## Security updates', '');
        for (const upgrade of security) lines.push(`- ${describeUpgrade(upgrade)}`);
        lines.push('');
    }
    if (node.length) {
        lines.push('## Node.js', '');
        for (const upgrade of node) lines.push(`- ${describeUpgrade(upgrade)}`);
        lines.push('', 'After installing, restart `memory-api` so it runs the new binary.', '');
    }
    if (other.length) {
        lines.push('## Other updates', '');
        for (const upgrade of other) lines.push(`- ${describeUpgrade(upgrade)}`);
        lines.push('');
    }
    if (removals.length) {
        lines.push('## Packages the upgrade would remove', '');
        for (const removal of removals) lines.push(`- \`${removal.name}\`${removal.from ? ` ${removal.from}` : ''}`);
        lines.push('');
    }
    if (heldBack) {
        lines.push('## Held back', '',
            `${heldBack} package${heldBack === 1 ? ' has an update' : 's have updates'} that the upgrade would not install (held with \`apt-mark hold\`, or blocked by dependencies). \`apt list --upgradable\` and \`apt-mark showhold\` show which.`, '');
    }
    if (failures.length) {
        lines.push('## Checks that failed', '');
        for (const failure of failures) lines.push(`- ${failure}`);
        lines.push('');
    }
    if (upgrades.length || removals.length) {
        // full-upgrade follows the same plan as the dist-upgrade simulation
        // above, removals included; plain `apt upgrade` would not.
        lines.push('To apply this plan, run `sudo apt full-upgrade` on the server.');
    }

    return {
        subject: `Staleness check (${host}): ${counts.join(', ')}`,
        body: lines.join('\n').trimEnd() + '\n',
    };
}

module.exports = {
    EOL_WARN_DAYS,
    parseAptSimulation,
    assessEol,
    buildReport,
};

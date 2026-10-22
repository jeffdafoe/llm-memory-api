#!/usr/bin/env node
// staleness-check.js — weekly report of pending apt upgrades and runtimes near
// end of life. Installed as a root cron job by the memory-api Ansible role.
//
// Usage: node scripts/staleness-check.js <recipient>
//
// It refreshes apt's package lists and SIMULATES an upgrade; it never installs
// anything. When something needs action it mails <recipient> (an actor name)
// from the built-in `system` actor through the app's own mail service, so the
// report lands in this instance's mailbox and admin dashboard — no SMTP and no
// outside service. A clean week sends nothing and logs one line.

const fs = require('fs');
const os = require('os');
const { execFileSync } = require('child_process');

const ENV_FILE = '/etc/memory-api/env';
const LOOKUP_TIMEOUT_MS = 15000;

const recipient = process.argv[2];
if (!recipient) {
    console.error('Usage: node scripts/staleness-check.js <recipient>');
    process.exit(1);
}

// The service reads DATABASE_URL from the systemd env file; the cron job has
// no such environment. src/db.js builds its pool from process.env at require
// time, so this must run before the app modules below are required.
function loadDatabaseUrl() {
    if (process.env.DATABASE_URL) return;
    const line = fs.readFileSync(ENV_FILE, 'utf8').split('\n').find((l) => l.startsWith('DATABASE_URL='));
    if (!line) throw new Error(`DATABASE_URL not found in ${ENV_FILE}`);
    process.env.DATABASE_URL = line.slice('DATABASE_URL='.length).trim();
}

function stamp(message) {
    console.log(`staleness-check: ${new Date().toISOString()} ${message}`);
}

// Ask endoflife.date for one release cycle, e.g. nodejs/24. Returns its `eol`
// field: a date string, or true/false (see staleness.assessEol).
async function lookupEol(product, cycle) {
    const url = `https://endoflife.date/api/${product}/${encodeURIComponent(cycle)}.json`;
    const response = await fetch(url, { signal: AbortSignal.timeout(LOOKUP_TIMEOUT_MS) });
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    const body = await response.json();
    return body.eol;
}

async function main() {
    loadDatabaseUrl();
    const pool = require('../src/db');
    // Close the pool on every path; an open pool keeps the process alive.
    try {
        await check(pool);
    } finally {
        await pool.end();
    }
}

async function check(pool) {
    const { mailSend } = require('../src/services/mail');
    const { parseAptSimulation, assessEol, buildReport } = require('../src/services/staleness');

    const today = new Date();
    const failures = [];
    let upgrades = [];

    // Refreshing the lists is what makes the simulation current; it changes
    // no installed package.
    try {
        execFileSync('apt-get', ['update', '-qq'], { stdio: ['ignore', 'ignore', 'pipe'] });
    } catch (err) {
        failures.push(`\`apt-get update\` failed, so the update list below may be out of date: ${String(err.stderr || err.message).trim()}`);
    }
    try {
        const output = execFileSync('apt-get', ['-s', 'dist-upgrade'], { encoding: 'utf8', env: { ...process.env, LC_ALL: 'C' } });
        upgrades = parseAptSimulation(output);
    } catch (err) {
        failures.push(`\`apt-get -s dist-upgrade\` failed: ${String(err.stderr || err.message).trim()}`);
    }

    // The running versions, read from the system itself rather than config,
    // so the check reports what is actually installed.
    const runtimes = [{ label: 'Node.js', product: 'nodejs', cycle: process.versions.node.split('.')[0] }];
    try {
        const debianVersion = fs.readFileSync('/etc/debian_version', 'utf8').trim();
        const major = debianVersion.split('.')[0];
        if (!/^\d+$/.test(major)) throw new Error(`not a numbered release: ${debianVersion}`);
        runtimes.push({ label: 'Debian', product: 'debian', cycle: major });
    } catch (err) {
        failures.push(`Could not read the Debian version: ${err.message}`);
    }
    try {
        // server_version_num is e.g. 170006 for 17.6.
        const result = await pool.query('SHOW server_version_num');
        runtimes.push({ label: 'PostgreSQL', product: 'postgresql', cycle: String(Math.floor(Number(result.rows[0].server_version_num) / 10000)) });
    } catch (err) {
        failures.push(`Could not read the PostgreSQL version: ${err.message}`);
    }

    const eolFindings = [];
    for (const runtime of runtimes) {
        try {
            const eol = await lookupEol(runtime.product, runtime.cycle);
            const finding = assessEol(runtime.label, runtime.cycle, eol, today);
            if (finding) eolFindings.push(finding);
        } catch (err) {
            failures.push(`endoflife.date lookup for ${runtime.label} ${runtime.cycle} failed: ${err.message}`);
        }
    }

    const report = buildReport({ host: os.hostname(), upgrades, eolFindings, failures });
    const checked = runtimes.map((r) => `${r.label} ${r.cycle}`).join(', ');
    if (!report) {
        stamp(`nothing to report (0 pending upgrades; checked ${checked})`);
    } else {
        const sent = await mailSend(recipient, 'system', report.subject, report.body);
        stamp(`mailed ${recipient} (${sent.id}): ${report.subject}`);
    }
}

main().catch((err) => {
    stamp(`FAILED: ${err.stack || err.message}`);
    process.exitCode = 1;
});

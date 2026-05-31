// Liveness watchdog — two independent guards so a blocked event loop can never
// again become a silent multi-minute outage (see the logout-PBKDF2 incident).
//
// 1. systemd hardware-style watchdog. When the unit sets WatchdogSec=, systemd
//    exports WATCHDOG_USEC and expects a periodic "WATCHDOG=1" sd_notify ping;
//    if the ping stops arriving it kills and restarts the service. We send that
//    ping on a timer at half the interval. Crucially the timer lives ON the
//    event loop, so if the loop is blocked (a runaway synchronous scan, an
//    infinite loop) the pings stop and systemd auto-recovers within WatchdogSec
//    — no human in the loop.
//
//    Node can't open the AF_UNIX *datagram* NOTIFY_SOCKET itself (its dgram
//    module is UDP-only), so the ping goes through the systemd-notify CLI. The
//    unit must set NotifyAccess=all so systemd accepts the ping from this child
//    process (it runs inside the service cgroup). The heartbeat activates only
//    when WATCHDOG_USEC is present, so in dev or a unit without WatchdogSec it
//    is a no-op and nothing is killed.
//
// 2. Event-loop lag observability. perf_hooks samples loop delay; if the max in
//    a window crosses a threshold we log it, so a near-miss is visible in the
//    journal before it escalates into a watchdog restart.

const { execFile } = require('child_process');
const { monitorEventLoopDelay } = require('perf_hooks');
const { log, logError } = require('./logger');

// How often we summarize accumulated event-loop delay, and the max-delay
// threshold (ms) above which we log a warning for that window.
const LAG_SAMPLE_MS = 10000;
const LAG_WARN_MS = 1000;

function startSystemdHeartbeat() {
    const watchdogUsec = parseInt(process.env.WATCHDOG_USEC, 10);
    if (!process.env.NOTIFY_SOCKET || !watchdogUsec || watchdogUsec <= 0) {
        // No systemd watchdog configured (dev, or a unit without WatchdogSec).
        return;
    }
    // Ping at half the watchdog interval, per systemd guidance, so a single
    // missed beat doesn't trip the watchdog.
    const intervalMs = Math.max(1000, Math.floor(watchdogUsec / 1000 / 2));

    function ping() {
        // Best-effort: a failed ping is just a missed beat. Accepted because
        // the unit sets NotifyAccess=all and this child runs in the cgroup.
        execFile('systemd-notify', ['WATCHDOG=1'], (err) => {
            if (err) {
                logError('watchdog', 'systemd-notify failed', { message: err.message });
            }
        });
    }

    ping();
    // Not unref'd on purpose: the heartbeat must stay scheduled. If the loop
    // blocks it stops on its own, which is exactly what triggers the restart.
    setInterval(ping, intervalMs);
    log('watchdog', 'systemd-heartbeat-started', {
        watchdogSec: watchdogUsec / 1e6,
        heartbeatMs: intervalMs
    });
}

function startLagMonitor() {
    let histogram;
    try {
        histogram = monitorEventLoopDelay({ resolution: 20 });
        histogram.enable();
    } catch (err) {
        logError('watchdog', 'lag-monitor-unavailable', { message: err.message });
        return;
    }
    const timer = setInterval(() => {
        // Histogram values are nanoseconds.
        const maxMs = histogram.max / 1e6;
        histogram.reset();
        if (maxMs >= LAG_WARN_MS) {
            log('watchdog', 'event-loop-lag', { maxMs: Math.round(maxMs) });
        }
    }, LAG_SAMPLE_MS);
    // Observability only — must not by itself keep the process alive.
    timer.unref();
}

function startWatchdog() {
    try {
        startSystemdHeartbeat();
        startLagMonitor();
    } catch (err) {
        // The watchdog must never take down the app it is meant to protect.
        logError('watchdog', 'start-failed', { message: err.message });
    }
}

module.exports = { startWatchdog };

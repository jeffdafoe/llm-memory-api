// Tests for the attempt limiter (LLM-670) — the throttle on the account-link
// route. Run with: node --test (from node/api).

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { createAttemptLimiter, acquireAttempt } = require('./attempt-limiter');

function fakeClock(start) {
    let t = start;
    return { now: () => t, advance: (ms) => { t += ms; } };
}

function limiter(clock, maxAttempts, extra) {
    return createAttemptLimiter(Object.assign({ maxAttempts, windowMs: 60000, now: clock.now }, extra || {}));
}

test('a burst of attempts started before any result is capped at maxAttempts', () => {
    // The route acquires, then awaits the password hash. A burst of parallel
    // requests all acquire before any of them finishes — only maxAttempts of
    // them may get through, however many arrive.
    const clock = fakeClock(1000);
    const caller = limiter(clock, 5);
    const target = limiter(clock, 10);
    let allowed = 0;
    for (let i = 0; i < 50; i++) {
        if (acquireAttempt([[caller, 7], [target, 'bob']]).allowed) {
            allowed += 1;
        }
    }
    assert.equal(allowed, 5);
});

test('the per-target cap holds across many callers', () => {
    const clock = fakeClock(1000);
    const caller = limiter(clock, 5);
    const target = limiter(clock, 10);
    let allowed = 0;
    for (let callerId = 1; callerId <= 30; callerId++) {
        if (acquireAttempt([[caller, callerId], [target, 'bob']]).allowed) {
            allowed += 1;
        }
    }
    assert.equal(allowed, 10);
});

test('a blocked acquire consumes nothing from the other limiter', () => {
    const clock = fakeClock(1000);
    const caller = limiter(clock, 5);
    const target = limiter(clock, 1);
    assert.equal(acquireAttempt([[caller, 7], [target, 'bob']]).allowed, true);
    // Target is now exhausted; these must not eat into caller 7's quota.
    for (let i = 0; i < 10; i++) {
        assert.equal(acquireAttempt([[caller, 7], [target, 'bob']]).allowed, false);
    }
    // A fresh target each time, so only caller 7's own quota can stop it.
    let allowed = 0;
    for (let i = 0; i < 10; i++) {
        if (acquireAttempt([[caller, 7], [target, 'other-' + i]]).allowed) {
            allowed += 1;
        }
    }
    assert.equal(allowed, 4, 'caller 7 used 1 of 5 before the target block');
});

test('the block lifts once the window has passed', () => {
    const clock = fakeClock(1000);
    const l = limiter(clock, 2);
    l.consume(7);
    l.consume(7);
    assert.equal(l.check(7).blocked, true);
    clock.advance(59999);
    assert.equal(l.check(7).retryAfterSeconds, 1);
    clock.advance(1);
    assert.equal(l.check(7).blocked, false);
});

test('reset (a success) clears the key', () => {
    const clock = fakeClock(1000);
    const l = limiter(clock, 2);
    l.consume(7);
    l.consume(7);
    l.reset(7);
    assert.equal(l.check(7).blocked, false);
});

test('expired one-off keys are swept once the map passes sweepAt', () => {
    const clock = fakeClock(1000);
    const l = limiter(clock, 5, { sweepAt: 10, maxKeys: 1000 });
    for (let i = 0; i < 10; i++) {
        l.consume('guess-' + i);
    }
    assert.equal(l.size(), 10);
    clock.advance(60000);
    // A check for a new key at the threshold sweeps the expired ones.
    assert.equal(l.check('new').blocked, false);
    assert.equal(l.size(), 0);
});

test('a full map refuses new keys but still serves existing ones', () => {
    const clock = fakeClock(1000);
    const l = limiter(clock, 5, { sweepAt: 3, maxKeys: 3 });
    l.consume('a');
    l.consume('b');
    l.consume('c');
    assert.equal(l.check('d').blocked, true, 'no room and nothing expired');
    assert.equal(l.check('a').blocked, false, 'existing key unaffected');
    clock.advance(60000);
    assert.equal(l.check('d').blocked, false, 'room again after the window');
});

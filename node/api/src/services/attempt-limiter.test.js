// Tests for createAttemptLimiter (LLM-670) — the failed-attempt throttle on
// the account-link route. Run with: node --test (from node/api).

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { createAttemptLimiter } = require('./attempt-limiter');

function fakeClock(start) {
    let t = start;
    return { now: () => t, advance: (ms) => { t += ms; } };
}

test('allows attempts until maxFailures failures, then blocks', () => {
    const clock = fakeClock(1000);
    const limiter = createAttemptLimiter({ maxFailures: 3, windowMs: 60000, now: clock.now });
    for (let i = 0; i < 3; i++) {
        assert.equal(limiter.check(7).blocked, false, 'attempt ' + (i + 1) + ' should be allowed');
        limiter.recordFailure(7);
    }
    const result = limiter.check(7);
    assert.equal(result.blocked, true);
    assert.equal(result.retryAfterSeconds, 60);
});

test('the block lifts once the window has passed', () => {
    const clock = fakeClock(1000);
    const limiter = createAttemptLimiter({ maxFailures: 2, windowMs: 60000, now: clock.now });
    limiter.recordFailure(7);
    limiter.recordFailure(7);
    assert.equal(limiter.check(7).blocked, true);
    clock.advance(59999);
    assert.equal(limiter.check(7).blocked, true, 'still inside the window');
    assert.equal(limiter.check(7).retryAfterSeconds, 1);
    clock.advance(1);
    assert.equal(limiter.check(7).blocked, false, 'window over');
});

test('a success clears the failures for that key', () => {
    const clock = fakeClock(1000);
    const limiter = createAttemptLimiter({ maxFailures: 2, windowMs: 60000, now: clock.now });
    limiter.recordFailure(7);
    limiter.recordSuccess(7);
    limiter.recordFailure(7);
    assert.equal(limiter.check(7).blocked, false, 'only one failure since the success');
});

test('keys are independent', () => {
    const clock = fakeClock(1000);
    const limiter = createAttemptLimiter({ maxFailures: 1, windowMs: 60000, now: clock.now });
    limiter.recordFailure(7);
    assert.equal(limiter.check(7).blocked, true);
    assert.equal(limiter.check(8).blocked, false);
});

test('a failure after the window expires starts a fresh window', () => {
    const clock = fakeClock(1000);
    const limiter = createAttemptLimiter({ maxFailures: 2, windowMs: 60000, now: clock.now });
    limiter.recordFailure(7);
    clock.advance(60000);
    limiter.recordFailure(7);
    assert.equal(limiter.check(7).blocked, false, 'the old failure no longer counts');
});

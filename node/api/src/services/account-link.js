// Account linking (LLM-670): let a user who controls two accounts make them
// see each other, by proving they know the OTHER account's dashboard password.
//
// Why: visibility is default-deny, and actors/visibility/save only lets you
// grant sight you already hold, so two self-registered accounts owned by one
// person could not be connected without a superadmin.
//
// The link is two ordinary actor_visibility_configuration rows (caller ->
// target and target -> caller), the same rows "Who They Can See" edits, so
// either side can later remove its half there. It bypasses the delegation
// bound on purpose: that bound stops a signup granting itself sight of
// accounts it cannot see; here the caller has proven it owns the target.
//
// The decision logic lives here with every side effect injected (DB, hashing,
// cache, log, limiter), so it can be tested without a database — the repo has
// no route/DB test harness. routes/admin.js wires in the real dependencies.

const INVALID_CREDENTIALS = {
    status: 400,
    body: { error: { code: 'INVALID_CREDENTIALS', message: 'Username or password is incorrect' } },
};

// Returns { status, body, headers? } for the route to send.
//
// Every credential failure (unknown account, no dashboard password, wrong
// password) returns the same INVALID_CREDENTIALS after the same hashing work,
// so the route cannot be used to discover which accounts exist. 400, not 401:
// the dashboard's api() helper treats any 401 as an expired session and logs
// the user out, and a typo in the OTHER account's password must not do that.
//
// deps:
//   acquireAttempt(callerId, username) -> { allowed } | { allowed: false, retryAfterSeconds }
//       MUST be synchronous: it reserves the attempt before anything is
//       awaited, so parallel requests cannot share one remaining quota.
//   findTarget(username)   -> { id, name, password_hash, password_salt } | null
//                             (null also when the account has no dashboard password)
//   verifyPassword(password, salt, hash) -> Promise<boolean>
//   dummyHash(password)    -> Promise   (equal-cost stand-in when there is no target)
//   sharesRealm(aId, bId)  -> Promise<boolean>
//   insertLinkRows(aId, bId) -> Promise<number>  rows actually added (0..2), one statement
//   clearVisibilityCache(actorId)
//   log(event, details)
async function linkAccount({ callerId, username, password }, deps) {
    if (!username || !password) {
        return {
            status: 400,
            body: { error: { code: 'BAD_REQUEST', message: 'Required fields: username, password' } },
        };
    }

    // No await before this line. Every attempt counts for the window — a
    // correct password refunds nothing, or a caller could guess, link an
    // account it controls to clear its count, and repeat.
    const attempt = deps.acquireAttempt(callerId, username);
    if (!attempt.allowed) {
        return {
            status: 429,
            headers: { 'Retry-After': String(attempt.retryAfterSeconds) },
            body: { error: { code: 'RATE_LIMITED', message: 'Too many attempts. Try again in ' + Math.ceil(attempt.retryAfterSeconds / 60) + ' minutes.' } },
        };
    }

    const target = await deps.findTarget(username);
    let valid = false;
    if (target) {
        valid = await deps.verifyPassword(password, target.password_salt, target.password_hash);
    } else {
        await deps.dummyHash(password);
    }
    if (!valid) {
        deps.log('account_link_failed', { user_id: callerId, username });
        return INVALID_CREDENTIALS;
    }

    if (target.id === callerId) {
        return {
            status: 400,
            body: { error: { code: 'BAD_REQUEST', message: 'That is the account you are logged in as' } },
        };
    }

    // Visibility grants only take effect between actors that share a realm
    // (services/actor-visibility.js), so a link across realms would be saved
    // but show nothing. Say so instead of reporting success.
    if (!(await deps.sharesRealm(callerId, target.id))) {
        return {
            status: 409,
            body: { error: { code: 'NO_SHARED_REALM', message: 'These accounts are in different realms, so a link would not let them see each other. Ask an administrator to add them to a shared realm.' } },
        };
    }

    const rowsAdded = await deps.insertLinkRows(callerId, target.id);
    deps.clearVisibilityCache(callerId);
    deps.clearVisibilityCache(target.id);
    deps.log('account_link', { user_id: callerId, target_actor_id: target.id, rows_added: rowsAdded });
    return { status: 200, body: { linked: target.name, already_linked: rowsAdded === 0 } };
}

module.exports = { linkAccount };

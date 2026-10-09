// Agent name + password check, shared by the dashboard login (/admin/login)
// and the claude.ai connector login on /authorize (LLM-733). The password is
// the one the user chose at signup (actors.password_hash / password_salt);
// accounts without one (virtual agents, sim NPCs) can never log in.

const pool = require('../db');
const { hash: hashToken, generateSalt, verify } = require('./hashing');

// Hashing against a throwaway salt when the name is unknown makes a miss take
// as long as a wrong password, so response time does not reveal which agent
// names exist.
const DUMMY_SALT = generateSalt();

// Returns { id, name } on a match, null otherwise. `username` must already be
// normalized by the caller (sanitize.agentName).
async function verifyPasswordLogin(username, password) {
    const result = await pool.query(
        'SELECT id, name, password_hash, password_salt FROM actors WHERE name = $1 AND password_hash IS NOT NULL',
        [username]
    );
    const row = result.rows[0];
    if (!row) {
        await hashToken(password, DUMMY_SALT);
        return null;
    }
    if (!(await verify(password, row.password_salt, row.password_hash))) {
        return null;
    }
    return { id: row.id, name: row.name };
}

module.exports = { verifyPasswordLogin };

function auth(req, res, next) {
    const header = req.headers.authorization;

    if (!header) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Missing Authorization header' }
        });
    }

    const token = header.replace('Bearer ', '');

    if (token !== process.env.MEMORY_API_KEY) {
        return res.status(403).json({
            error: { code: 'FORBIDDEN', message: 'Invalid API key' }
        });
    }

    next();
}

module.exports = auth;

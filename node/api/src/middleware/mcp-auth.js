// Validates JWT bearer tokens on the /mcp endpoint.
// Tokens are issued by the /oauth/token endpoint (client_credentials grant).
// Sets req.mcpAgent and req.mcpPermissions for downstream handlers.

const jwt = require('jsonwebtoken');

const JWT_SECRET = process.env.JWT_SECRET;

function getResourceMetadataUrl(req) {
    if (process.env.BASE_URL) {
        return `${process.env.BASE_URL}/.well-known/oauth-protected-resource`;
    }
    return `${req.protocol}://${req.get('host')}/.well-known/oauth-protected-resource`;
}

function mcpAuth(req, res, next) {
    const header = req.headers.authorization;

    if (!header || !header.startsWith('Bearer ')) {
        res.set('WWW-Authenticate', `Bearer resource_metadata="${getResourceMetadataUrl(req)}"`);
        return res.status(401).json({
            error: 'unauthorized',
            error_description: 'Missing or invalid Authorization header'
        });
    }

    const token = header.slice(7);

    try {
        const decoded = jwt.verify(token, JWT_SECRET);
        req.mcpAgent = decoded.agent;
        req.mcpPermissions = decoded.permissions || [];
        next();
    } catch (err) {
        res.set('WWW-Authenticate', `Bearer resource_metadata="${getResourceMetadataUrl(req)}"`);

        if (err.name === 'TokenExpiredError') {
            return res.status(401).json({
                error: 'invalid_token',
                error_description: 'Token has expired'
            });
        }
        return res.status(401).json({
            error: 'invalid_token',
            error_description: 'Invalid token'
        });
    }
}

module.exports = mcpAuth;

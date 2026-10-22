// Content-Security-Policy for the admin SPA (LLM-27). A structural backstop
// for XSS: even if a v-html sink (the mermaid render, rendered markdown) lets
// markup through its sanitizer, the browser refuses to run any script that is
// not a file served from this origin. That blocks <script> tags, inline
// handlers such as onerror=, javascript: URLs and eval-style code.
//
// The policy holds only because the admin's Vue templates are compiled at
// build time (vite.config.js, vueTemplateCompiler). Vue's in-browser template
// compiler needs Function(), which script-src 'self' forbids; switching back
// to runtime templates would blank the admin.
//
// - script-src 'self'      only same-origin script files; no inline, no eval
// - object-src 'none'      no <object>/<embed> plugin content
// - base-uri 'none'        an injected <base> cannot re-point relative URLs;
//                          the admin uses no <base> of its own
// - form-action 'self'     injected markup cannot post a form to another site
// - frame-ancestors 'none' the login form cannot be framed (clickjacking);
//                          same rule as the OAuth pages in routes/oauth.js
//
// Styles, images, fonts and connections are left unrestricted: mermaid writes
// inline <style>, and the templates use style attributes.
const ADMIN_CSP = [
    "script-src 'self'",
    "object-src 'none'",
    "base-uri 'none'",
    "form-action 'self'",
    "frame-ancestors 'none'"
].join('; ');

function adminCsp(req, res, next) {
    res.setHeader('Content-Security-Policy', ADMIN_CSP);
    next();
}

module.exports = { adminCsp, ADMIN_CSP };

#!/usr/bin/env node
// check-admin-templates.js: fail when an admin UI component would need Vue's
// runtime template compiler.
//
// nginx serves the admin with `script-src 'self'` (admin_csp in
// infrastructure/roles/nginx/templates/site-production.conf.j2). The admin
// imports 'vue', which bundlers resolve to the runtime-only build, and
// templates are compiled at build time: a component imports
// `./x.html?render` and the vue-template-compiler plugin in vite.config.js
// turns the file into a render function. A component that declares a
// `template` option instead (a string, or an `x.html?raw` import) has no
// render function, and compiling it in the browser would need Function(),
// which the policy blocks. Either way the view renders blank, and vite build
// does not fail. This check catches the two shapes in the source.
//
// The template pattern matches a line that starts with `template:` or
// `template,` (an option written in full, or in shorthand). The admin writes
// one option per line; an option on the same line as its `{` is not matched,
// because a wider pattern would also match request bodies such as
// `{ name, template }` (the admin edits stored templates). Plain text that
// starts a line with `template:`, inside a template literal, also matches:
// reword it.
//
// Run from anywhere: node node/api/scripts/check-admin-templates.js
// Exit 0 when clean, 1 with the offending files listed. CI runs it in the
// Admin UI Templates job. It uses only Node's built-in modules, so it needs
// no npm install.

'use strict';

const fs = require('node:fs');
const path = require('node:path');

// __dirname is the folder of this script, so the path does not depend on
// the working directory.
const ADMIN_SRC = path.join(__dirname, '..', 'public', 'admin');
const SOURCE_EXT = /\.(js|mjs|ts|vue)$/;
// The `m` flag makes ^ match at the start of every line, not only the first.
const TEMPLATE_OPTION = /^\s*template\s*[,:]/m;
const RAW_TEMPLATE_IMPORT = '.html?raw';

const offenders = [];

function walk(dir) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) {
            // dist is vite's output, not source.
            if (entry.name !== 'dist') {
                walk(full);
            }
            continue;
        }
        if (!SOURCE_EXT.test(entry.name)) {
            continue;
        }
        const src = fs.readFileSync(full, 'utf8');
        const rel = path.relative(ADMIN_SRC, full);
        if (TEMPLATE_OPTION.test(src)) {
            offenders.push(rel + ': template option');
        }
        if (src.includes(RAW_TEMPLATE_IMPORT)) {
            offenders.push(rel + ': ' + RAW_TEMPLATE_IMPORT + ' import');
        }
    }
}

walk(ADMIN_SRC);

if (offenders.length > 0) {
    console.error('Admin components that need the runtime template compiler (blank under the admin CSP):');
    for (const line of offenders) {
        console.error('  ' + line);
    }
    console.error('Move the template into an .html file and import it with ?render (see vite.config.js).');
    process.exit(1);
}
console.log('Admin templates: no runtime-compiled component.');

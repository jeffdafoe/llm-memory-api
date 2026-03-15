import { defineConfig } from 'vite';
import { readFileSync } from 'fs';

// The admin uses in-DOM templates (Vue directives in index.html), which requires
// the template compiler at runtime. vue/dist/vue.esm-bundler.js includes it and
// registers it, but @vue/* packages declare sideEffects:false so Rollup
// tree-shakes the compiler out. Fix: alias 'vue' to the full build and mark it
// as having side effects so the compiler registration survives.

// Validate HTML templates imported via ?raw for balanced tags.
// Catches mismatched open/close tags at build time instead of at runtime.
function htmlTemplateValidator() {
    // Self-closing HTML tags that don't need a closing tag
    const VOID_TAGS = new Set([
        'area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input',
        'link', 'meta', 'param', 'source', 'track', 'wbr'
    ]);

    return {
        name: 'html-template-validator',
        enforce: 'pre',
        transform(code, id) {
            if (!id.endsWith('.html?raw')) return;

            const filePath = id.replace('?raw', '');
            let html;
            try {
                html = readFileSync(filePath, 'utf8');
            } catch { return; }

            // Match all opening and closing tags
            const tagRe = /<\/?([a-zA-Z][a-zA-Z0-9-]*)\b[^>]*\/?>/g;
            const stack = [];
            let match;

            while ((match = tagRe.exec(html)) !== null) {
                const full = match[0];
                const tag = match[1].toLowerCase();

                if (VOID_TAGS.has(tag)) continue;
                if (full.endsWith('/>')) continue;         // self-closed
                if (full.startsWith('</')) {
                    // Closing tag
                    if (stack.length === 0 || stack[stack.length - 1] !== tag) {
                        const expected = stack.length ? stack[stack.length - 1] : '(none)';
                        this.error(
                            `Template tag mismatch in ${filePath}: ` +
                            `found </${tag}> but expected </${expected}>`
                        );
                    }
                    stack.pop();
                } else {
                    // Opening tag
                    stack.push(tag);
                }
            }

            if (stack.length > 0) {
                this.error(
                    `Template has unclosed tags in ${filePath}: ` +
                    `<${stack.join('>, <')}>`
                );
            }
        }
    };
}

export default defineConfig({
    root: 'public/admin',
    base: '/admin/',
    resolve: {
        alias: {
            vue: 'vue/dist/vue.esm-bundler.js'
        }
    },
    plugins: [
        htmlTemplateValidator(),
        {
            name: 'vue-compiler-side-effects',
            enforce: 'pre',
            async resolveId(source, importer, options) {
                if (source === 'vue/dist/vue.esm-bundler.js') {
                    const resolved = await this.resolve(source, importer, { ...options, skipSelf: true });
                    if (resolved) {
                        return { ...resolved, moduleSideEffects: true };
                    }
                }
            }
        }
    ],
    define: {
        __VUE_OPTIONS_API__: true,
        __VUE_PROD_DEVTOOLS__: false,
        __VUE_PROD_HYDRATION_MISMATCH_DETAILS__: false
    },
    build: {
        outDir: 'dist',
        emptyOutDir: true,
        rollupOptions: {
            input: 'public/admin/index.html'
        }
    }
});

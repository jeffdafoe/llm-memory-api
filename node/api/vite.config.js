import { defineConfig } from 'vite';
import { readFileSync } from 'fs';
import { compile } from '@vue/compiler-dom';

// The admin uses in-DOM templates (Vue directives in index.html), which requires
// the template compiler at runtime. vue/dist/vue.esm-bundler.js includes it and
// registers it, but @vue/* packages declare sideEffects:false so Rollup
// tree-shakes the compiler out. Fix: alias 'vue' to the full build and mark it
// as having side effects so the compiler registration survives.

// Validate Vue templates imported via ?raw using Vue's own compiler.
// Catches syntax errors at build time (and dev serve) instead of at runtime.
function htmlTemplateValidator() {
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

            const errors = [];
            compile(html, {
                onError(err) { errors.push(err); }
            });

            if (errors.length > 0) {
                const messages = errors.map(e => e.message).join('\n  ');
                this.error(`Vue template errors in ${filePath}:\n  ${messages}`);
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

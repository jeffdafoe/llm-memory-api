import { defineConfig } from 'vite';
import { readFileSync } from 'fs';
import { compile } from '@vue/compiler-dom';

// Compile Vue templates at build time (LLM-27). `import { render } from
// './x.html?render'` loads x.html and returns the ES module Vue's compiler
// emits: an exported render function that imports its helpers from 'vue'.
// Nothing is compiled in the browser, so the app runs on Vue's runtime-only
// build. The full build compiles templates with Function(), which a
// Content-Security-Policy without 'unsafe-eval' blocks (see src/server.js).
// A template syntax error fails the build instead of showing up at runtime.
function vueTemplateCompiler() {
    return {
        name: 'vue-template-compiler',
        enforce: 'pre',
        load(id) {
            if (!id.endsWith('.html?render')) return;

            const filePath = id.slice(0, -'?render'.length);
            this.addWatchFile(filePath);
            const html = readFileSync(filePath, 'utf8');

            const errors = [];
            // prefixIdentifiers rewrites `foo` to `_ctx.foo`, the module-mode
            // equivalent of the runtime compiler's with(_ctx) scope. Globals
            // Vue allows in templates (Math, Date, JSON, ...) are left alone,
            // same as at runtime. hoistStatic matches the runtime compiler's
            // setting.
            const { code } = compile(html, {
                mode: 'module',
                prefixIdentifiers: true,
                hoistStatic: true,
                onError(err) { errors.push(err); }
            });

            if (errors.length > 0) {
                const messages = errors.map(e => {
                    if (e.loc) {
                        return `line ${e.loc.start.line}: ${e.message}`;
                    }
                    return e.message;
                }).join('\n  ');
                this.error(`Vue template errors in ${filePath}:\n  ${messages}`);
            }
            return code;
        }
    };
}

export default defineConfig({
    root: 'public/admin',
    base: '/admin/',
    plugins: [
        vueTemplateCompiler()
    ],
    define: {
        __VUE_OPTIONS_API__: true,
        __VUE_PROD_DEVTOOLS__: false,
        __VUE_PROD_HYDRATION_MISMATCH_DETAILS__: false
    },
    build: {
        outDir: 'dist',
        emptyOutDir: true,
        chunkSizeWarningLimit: 600,
        rollupOptions: {
            input: 'public/admin/index.html',
            output: {
                manualChunks(id) {
                    if (id.includes('node_modules/vue/')) return 'vue';
                    if (id.includes('node_modules/mermaid/')) return 'mermaid';
                    if (id.includes('node_modules/marked/') || id.includes('node_modules/dompurify/')) return 'markdown';
                    if (id.includes('node_modules/d3-')) return 'd3';
                }
            }
        }
    }
});

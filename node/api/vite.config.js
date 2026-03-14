import { defineConfig } from 'vite';

// The admin uses in-DOM templates (Vue directives in index.html), which requires
// the template compiler at runtime. vue/dist/vue.esm-bundler.js includes it and
// registers it, but @vue/* packages declare sideEffects:false so Rollup
// tree-shakes the compiler out. Fix: alias 'vue' to the full build and mark it
// as having side effects so the compiler registration survives.

export default defineConfig({
    root: 'public/admin',
    base: '/admin/',
    resolve: {
        alias: {
            vue: 'vue/dist/vue.esm-bundler.js'
        }
    },
    plugins: [{
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
    }],
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

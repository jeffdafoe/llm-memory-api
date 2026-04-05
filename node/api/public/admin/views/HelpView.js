import { inject, computed } from 'vue';
import template from './help.html?raw';

export default {
    name: 'HelpView',
    template,
    setup() {
        const app = inject('app');
        const mcpUrl = computed(() => location.origin + '/mcp');
        return { ...app, mcpUrl };
    }
};

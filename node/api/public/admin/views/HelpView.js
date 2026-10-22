import { inject, computed } from 'vue';
import { render } from './help.html?render';

export default {
    name: 'HelpView',
    render,
    setup() {
        const app = inject('app');
        const mcpUrl = computed(() => location.origin + '/mcp');
        return { ...app, mcpUrl };
    }
};

import { inject } from 'vue';
import template from './agent-dialog.html?raw';

export default {
    name: 'AgentDialog',
    template,
    setup() {
        return inject('app');
    }
};

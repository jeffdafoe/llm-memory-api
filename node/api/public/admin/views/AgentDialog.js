import { inject } from 'vue';
import { render } from './agent-dialog.html?render';

export default {
    name: 'AgentDialog',
    render,
    setup() {
        return inject('app');
    }
};

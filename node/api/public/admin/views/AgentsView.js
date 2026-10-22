import { inject } from 'vue';
import { render } from './agents.html?render';

export default {
    name: 'AgentsView',
    render,
    setup() {
        return inject('app');
    }
};

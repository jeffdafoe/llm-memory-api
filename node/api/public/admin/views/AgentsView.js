import { inject } from 'vue';
import template from './agents.html?raw';

export default {
    name: 'AgentsView',
    template,
    setup() {
        return inject('app');
    }
};

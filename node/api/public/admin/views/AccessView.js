import { inject } from 'vue';
import template from './access.html?raw';

export default {
    name: 'AccessView',
    template,
    setup() {
        return inject('app');
    }
};

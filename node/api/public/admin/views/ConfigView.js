import { inject } from 'vue';
import template from './config.html?raw';

export default {
    name: 'ConfigView',
    template,
    setup() {
        return inject('app');
    }
};

import { inject } from 'vue';
import template from './dashboard.html?raw';

export default {
    name: 'DashboardView',
    template,
    setup() {
        return inject('app');
    }
};

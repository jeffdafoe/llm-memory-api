import { inject } from 'vue';
import { render } from './dashboard.html?render';

export default {
    name: 'DashboardView',
    render,
    setup() {
        return inject('app');
    }
};

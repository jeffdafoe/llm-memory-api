import { inject } from 'vue';
import { render } from './config.html?render';

export default {
    name: 'ConfigView',
    render,
    setup() {
        return inject('app');
    }
};

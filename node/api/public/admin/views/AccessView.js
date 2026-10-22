import { inject } from 'vue';
import { render } from './access.html?render';

export default {
    name: 'AccessView',
    render,
    setup() {
        return inject('app');
    }
};

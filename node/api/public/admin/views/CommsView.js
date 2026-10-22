import { inject } from 'vue';
import { render } from './comms.html?render';

export default {
    name: 'CommsView',
    render,
    setup() {
        return inject('app');
    }
};

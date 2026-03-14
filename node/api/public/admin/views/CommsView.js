import { inject } from 'vue';
import template from './comms.html?raw';

export default {
    name: 'CommsView',
    template,
    setup() {
        return inject('app');
    }
};

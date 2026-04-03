import { inject } from 'vue';
import template from './help.html?raw';

export default {
    name: 'HelpView',
    template,
    setup() {
        return inject('app');
    }
};

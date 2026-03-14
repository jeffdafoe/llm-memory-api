import { inject } from 'vue';
import template from './misc-dialogs.html?raw';

export default {
    name: 'MiscDialogs',
    template,
    setup() {
        return inject('app');
    }
};

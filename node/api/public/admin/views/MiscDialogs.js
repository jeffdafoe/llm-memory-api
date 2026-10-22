import { inject } from 'vue';
import { render } from './misc-dialogs.html?render';

export default {
    name: 'MiscDialogs',
    render,
    setup() {
        return inject('app');
    }
};

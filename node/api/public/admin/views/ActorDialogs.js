import { inject } from 'vue';
import template from './actor-dialogs.html?raw';

export default {
    name: 'ActorDialogs',
    template,
    setup() {
        return inject('app');
    }
};

import { inject } from 'vue';
import { render } from './actor-dialogs.html?render';

export default {
    name: 'ActorDialogs',
    render,
    setup() {
        return inject('app');
    }
};

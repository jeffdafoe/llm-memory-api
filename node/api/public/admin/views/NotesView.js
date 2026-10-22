import { inject } from 'vue';
import { render } from './memory.html?render';

export default {
    name: 'NotesView',
    render,
    setup() {
        return inject('app');
    }
};

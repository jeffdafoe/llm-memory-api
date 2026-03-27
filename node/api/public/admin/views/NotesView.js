import { inject } from 'vue';
import template from './memory.html?raw';

export default {
    name: 'NotesView',
    template,
    setup() {
        return inject('app');
    }
};

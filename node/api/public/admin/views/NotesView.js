import { inject } from 'vue';
import template from './notes.html?raw';

export default {
    name: 'NotesView',
    template,
    setup() {
        return inject('app');
    }
};

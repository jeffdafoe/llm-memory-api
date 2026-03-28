// CustomSelect — styled dropdown replacement for native <select>.
// Usage:
//   <custom-select v-model="value" :options="options" placeholder="Choose..." />
//
// Options format: array of { value, label } objects, or simple strings.
// Supports keyboard navigation (arrows, Enter, Escape, type-to-filter).

import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue';

const template = `
<div class="custom-select" :class="{ 'custom-select--open': open, 'custom-select--disabled': disabled }" ref="root">
    <button type="button" class="custom-select__trigger" @click="toggle" @keydown="onTriggerKeydown" :disabled="disabled">
        <span class="custom-select__value" :class="{ 'custom-select__placeholder': !hasSelection }">
            {{ displayLabel }}
        </span>
        <i class="icon-chevron-down custom-select__chevron"></i>
    </button>
    <div v-if="open" class="custom-select__dropdown" ref="dropdown">
        <div v-if="filterable" class="custom-select__search">
            <input ref="searchInput" type="text" v-model="search" @keydown="onSearchKeydown"
                placeholder="Filter..." class="custom-select__search-input">
        </div>
        <div class="custom-select__options" ref="optionsList">
            <div v-for="(opt, idx) in filteredOptions" :key="opt.value"
                class="custom-select__option"
                :class="{
                    'custom-select__option--selected': opt.value === modelValue,
                    'custom-select__option--focused': idx === focusedIndex
                }"
                @click="select(opt.value)"
                @mouseenter="focusedIndex = idx"
                :ref="el => { if (idx === focusedIndex) focusedEl = el; }">
                {{ opt.label }}
                <i v-if="opt.value === modelValue" class="icon-check custom-select__check"></i>
            </div>
            <div v-if="filteredOptions.length === 0" class="custom-select__empty">
                No matches
            </div>
        </div>
    </div>
</div>
`;

export default {
    name: 'CustomSelect',
    template,
    props: {
        modelValue: { default: '' },
        options: { type: Array, default: () => [] },
        placeholder: { type: String, default: 'Select...' },
        disabled: { type: Boolean, default: false },
        // Show search/filter input when there are this many or more options
        filterThreshold: { type: Number, default: 8 }
    },
    emits: ['update:modelValue'],
    setup(props, { emit }) {
        const open = ref(false);
        const search = ref('');
        const focusedIndex = ref(-1);
        const focusedEl = ref(null);
        const root = ref(null);
        const dropdown = ref(null);
        const searchInput = ref(null);
        const optionsList = ref(null);

        // Normalize options to { value, label } format
        const normalizedOptions = computed(() => {
            return props.options.map(opt => {
                if (typeof opt === 'string') {
                    return { value: opt, label: opt };
                }
                return { value: opt.value, label: opt.label || String(opt.value) };
            });
        });

        // Whether to show the filter input
        const filterable = computed(() => {
            return normalizedOptions.value.length >= props.filterThreshold;
        });

        // Filter options by search text
        const filteredOptions = computed(() => {
            if (!search.value) return normalizedOptions.value;
            const q = search.value.toLowerCase();
            return normalizedOptions.value.filter(opt =>
                opt.label.toLowerCase().includes(q)
            );
        });

        // Whether a value is currently selected
        const hasSelection = computed(() => {
            return normalizedOptions.value.some(opt => opt.value === props.modelValue);
        });

        // Display text for the trigger button
        const displayLabel = computed(() => {
            const found = normalizedOptions.value.find(opt => opt.value === props.modelValue);
            return found ? found.label : props.placeholder;
        });

        function toggle() {
            if (props.disabled) return;
            if (open.value) {
                close();
            } else {
                openDropdown();
            }
        }

        function openDropdown() {
            open.value = true;
            search.value = '';
            // Set focus to the currently selected item
            const selectedIdx = filteredOptions.value.findIndex(opt => opt.value === props.modelValue);
            focusedIndex.value = selectedIdx >= 0 ? selectedIdx : 0;
            nextTick(() => {
                if (filterable.value && searchInput.value) {
                    searchInput.value.focus();
                }
                scrollToFocused();
                positionDropdown();
            });
        }

        function close() {
            open.value = false;
            search.value = '';
            focusedIndex.value = -1;
        }

        function select(value) {
            emit('update:modelValue', value);
            close();
        }

        // Position dropdown above or below based on available space
        function positionDropdown() {
            if (!dropdown.value || !root.value) return;
            const triggerRect = root.value.getBoundingClientRect();
            const dropdownEl = dropdown.value;
            const spaceBelow = window.innerHeight - triggerRect.bottom;
            const spaceAbove = triggerRect.top;

            // Reset positioning
            dropdownEl.style.top = '';
            dropdownEl.style.bottom = '';

            if (spaceBelow < 240 && spaceAbove > spaceBelow) {
                // Position above
                dropdownEl.style.bottom = '100%';
                dropdownEl.style.marginBottom = '4px';
                dropdownEl.style.marginTop = '';
            } else {
                // Position below (default)
                dropdownEl.style.top = '100%';
                dropdownEl.style.marginTop = '4px';
                dropdownEl.style.marginBottom = '';
            }
        }

        function scrollToFocused() {
            nextTick(() => {
                if (focusedEl.value && optionsList.value) {
                    focusedEl.value.scrollIntoView({ block: 'nearest' });
                }
            });
        }

        function moveFocus(delta) {
            const max = filteredOptions.value.length - 1;
            if (max < 0) return;
            let next = focusedIndex.value + delta;
            if (next < 0) next = 0;
            if (next > max) next = max;
            focusedIndex.value = next;
            scrollToFocused();
        }

        function onTriggerKeydown(e) {
            if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
                e.preventDefault();
                if (!open.value) {
                    openDropdown();
                }
            } else if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                toggle();
            } else if (e.key === 'Escape') {
                close();
            }
        }

        function onSearchKeydown(e) {
            if (e.key === 'ArrowDown') {
                e.preventDefault();
                moveFocus(1);
            } else if (e.key === 'ArrowUp') {
                e.preventDefault();
                moveFocus(-1);
            } else if (e.key === 'Enter') {
                e.preventDefault();
                if (focusedIndex.value >= 0 && focusedIndex.value < filteredOptions.value.length) {
                    select(filteredOptions.value[focusedIndex.value].value);
                }
            } else if (e.key === 'Escape') {
                close();
            }
        }

        // Close on outside click
        function onClickOutside(e) {
            if (root.value && !root.value.contains(e.target)) {
                close();
            }
        }

        onMounted(() => {
            document.addEventListener('mousedown', onClickOutside);
        });

        onUnmounted(() => {
            document.removeEventListener('mousedown', onClickOutside);
        });

        // Reset focused index when filter changes
        watch(search, () => {
            focusedIndex.value = filteredOptions.value.length > 0 ? 0 : -1;
        });

        return {
            open, search, focusedIndex, focusedEl, root, dropdown, searchInput, optionsList,
            normalizedOptions, filterable, filteredOptions, hasSelection, displayLabel,
            toggle, close, select, onTriggerKeydown, onSearchKeydown
        };
    }
};

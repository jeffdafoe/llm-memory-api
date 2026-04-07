// CustomSelect — styled dropdown replacement for native <select>.
// Usage:
//   <custom-select v-model="value" :options="options" placeholder="Choose..." />
//   <custom-select v-model="values" :options="options" multiple placeholder="Choose agents..." />
//
// Options format: array of { value, label } objects, or simple strings.
// Supports keyboard navigation (arrows, Enter, Escape, type-to-filter).
//
// multiple prop: when true, modelValue is an array and clicking toggles selection.
// The dropdown stays open until clicked outside or Escape is pressed.
//
// allowCustom prop: when true, the user can type an arbitrary value that isn't
// in the options list. A "Use ..." option appears at the top of the dropdown.
// Useful for providers like OpenRouter where the model list is open-ended.

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
        <div v-if="filterable || allowCustom" class="custom-select__search">
            <input ref="searchInput" type="text" v-model="search" @keydown="onSearchKeydown"
                :placeholder="allowCustom ? 'Filter or type model ID...' : 'Filter...'" class="custom-select__search-input">
        </div>
        <div class="custom-select__options" ref="optionsList">
            <div v-if="allowCustom && search && !exactMatch" class="custom-select__option custom-select__option--custom"
                :class="{ 'custom-select__option--focused': focusedIndex === -2 }"
                @click="select(search)"
                @mouseenter="focusedIndex = -2">
                Use "{{ search }}"
            </div>
            <div v-for="(opt, idx) in filteredOptions" :key="opt.value"
                class="custom-select__option"
                :class="{
                    'custom-select__option--selected': isSelected(opt.value),
                    'custom-select__option--focused': idx === focusedIndex
                }"
                @click="select(opt.value)"
                @mouseenter="focusedIndex = idx"
                :ref="el => { if (idx === focusedIndex) focusedEl = el; }">
                <i v-if="multiple" :class="isSelected(opt.value) ? 'icon-check-square' : 'icon-square'" class="custom-select__multi-icon"></i>
                {{ opt.label }}
                <span v-if="opt.sublabel" class="custom-select__sublabel">{{ opt.sublabel }}</span>
                <i v-if="!multiple && isSelected(opt.value)" class="icon-check custom-select__check"></i>
            </div>
            <div v-if="filteredOptions.length === 0 && !(allowCustom && search)" class="custom-select__empty">
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
        filterThreshold: { type: Number, default: 8 },
        // Allow typing arbitrary values not in the options list
        allowCustom: { type: Boolean, default: false },
        // Multiple selection mode — modelValue is an array
        multiple: { type: Boolean, default: false }
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

        // Normalize options to { value, label, sublabel } format
        const normalizedOptions = computed(() => {
            return props.options.map(opt => {
                if (typeof opt === 'string') {
                    return { value: opt, label: opt, sublabel: '' };
                }
                return { value: opt.value, label: opt.label || String(opt.value), sublabel: opt.sublabel || '' };
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
                opt.label.toLowerCase().includes(q) || opt.value.toLowerCase().includes(q)
            );
        });

        // Whether the search text exactly matches an existing option value
        const exactMatch = computed(() => {
            if (!search.value) return false;
            const q = search.value.toLowerCase();
            return normalizedOptions.value.some(opt => opt.value.toLowerCase() === q);
        });

        // Check if a value is currently selected
        function isSelected(value) {
            if (props.multiple) {
                return Array.isArray(props.modelValue) && props.modelValue.includes(value);
            }
            return value === props.modelValue;
        }

        // Whether a value is currently selected (in options list OR custom)
        const hasSelection = computed(() => {
            if (props.multiple) {
                return Array.isArray(props.modelValue) && props.modelValue.length > 0;
            }
            if (!props.modelValue) return false;
            if (normalizedOptions.value.some(opt => opt.value === props.modelValue)) return true;
            // For allowCustom, any non-empty value counts as selected
            if (props.allowCustom) return true;
            return false;
        });

        // Display text for the trigger button
        const displayLabel = computed(() => {
            if (props.multiple) {
                if (!Array.isArray(props.modelValue) || props.modelValue.length === 0) {
                    return props.placeholder;
                }
                // Show names for up to 3 selections, then "N selected"
                if (props.modelValue.length <= 3) {
                    return props.modelValue.map(v => {
                        const found = normalizedOptions.value.find(opt => opt.value === v);
                        return found ? found.label : v;
                    }).join(', ');
                }
                return props.modelValue.length + ' selected';
            }
            const found = normalizedOptions.value.find(opt => opt.value === props.modelValue);
            if (found) return found.label;
            // For custom values, show the raw value
            if (props.allowCustom && props.modelValue) return props.modelValue;
            return props.placeholder;
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
            var selectedIdx;
            if (props.multiple) {
                selectedIdx = -1;
            } else {
                selectedIdx = filteredOptions.value.findIndex(opt => opt.value === props.modelValue);
            }
            focusedIndex.value = selectedIdx >= 0 ? selectedIdx : 0;
            nextTick(() => {
                if ((filterable.value || props.allowCustom) && searchInput.value) {
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
            if (props.multiple) {
                // Toggle the value in the array
                var current = Array.isArray(props.modelValue) ? [...props.modelValue] : [];
                var idx = current.indexOf(value);
                if (idx >= 0) {
                    current.splice(idx, 1);
                } else {
                    current.push(value);
                }
                emit('update:modelValue', current);
                // Don't close — let the user select more
            } else {
                emit('update:modelValue', value);
                close();
            }
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
                // If the custom "Use ..." option is focused, select the raw search text
                if (props.allowCustom && focusedIndex.value === -2 && search.value) {
                    select(search.value);
                } else if (focusedIndex.value >= 0 && focusedIndex.value < filteredOptions.value.length) {
                    select(filteredOptions.value[focusedIndex.value].value);
                } else if (props.allowCustom && search.value && !exactMatch.value) {
                    // No focused item but custom is allowed — use the search text
                    select(search.value);
                }
            } else if (e.key === 'Escape') {
                close();
            }
        }

        // Close on outside click or when focus leaves the component.
        // mousedown catches clicks on non-focusable elements (empty space, other divs).
        function onClickOutside(e) {
            if (open.value && root.value && !root.value.contains(e.target)) {
                close();
            }
        }

        // focusin catches when focus moves to another input/button.
        // Must also check that the focus target isn't an ancestor of the
        // component (e.g. a <dialog>). When clicking a non-focusable option
        // div, the browser moves focus to the nearest focusable ancestor,
        // which is outside root but NOT an outside click.
        function onFocusIn(e) {
            if (open.value && root.value && !root.value.contains(e.target) && !e.target.contains(root.value)) {
                close();
            }
        }

        onMounted(() => {
            document.addEventListener('mousedown', onClickOutside, true);
            document.addEventListener('focusin', onFocusIn, true);
        });

        onUnmounted(() => {
            document.removeEventListener('mousedown', onClickOutside, true);
            document.removeEventListener('focusin', onFocusIn, true);
        });

        // Reset focused index when filter changes
        watch(search, () => {
            focusedIndex.value = filteredOptions.value.length > 0 ? 0 : -1;
        });

        return {
            open, search, focusedIndex, focusedEl, root, dropdown, searchInput, optionsList,
            normalizedOptions, filterable, filteredOptions, exactMatch, isSelected, hasSelection, displayLabel,
            toggle, close, select, onTriggerKeydown, onSearchKeydown
        };
    }
};

// notes.js — Notes browser (tree view, editor, search, reindex)

function useNotes({ api, showToast, showConfirm }) {
    const notesNamespaces = ref([]);
    const notesTreesRaw = ref({});
    const expandedNamespaces = ref({});
    const expandedFolders = ref({});
    const selectedNote = ref(null);
    const notesEditing = ref(false);
    const notesEditTitle = ref('');
    const notesEditContent = ref('');
    const notesSaving = ref(false);
    const notesSearchQuery = ref('');
    const notesSearchResults = ref(null);
    const notesReindexing = ref(false);
    const reindexStatus = ref(null);
    let reindexPollTimer = null;

    // Build a flat tree structure from a list of slugs
    function buildTree(notes) {
        const tree = [];
        const folders = new Set();
        const sorted = [...notes].sort((a, b) => a.slug.localeCompare(b.slug));

        for (const note of sorted) {
            const parts = note.slug.split('/');
            let path = '';
            for (let i = 0; i < parts.length - 1; i++) {
                path = path ? path + '/' + parts[i] : parts[i];
                if (!folders.has(path)) {
                    folders.add(path);
                    tree.push({
                        type: 'folder',
                        name: parts[i],
                        path: path,
                        depth: i + 1
                    });
                }
            }
            tree.push({
                type: 'file',
                name: parts[parts.length - 1],
                slug: note.slug,
                title: note.title,
                depth: parts.length,
                updated_at: note.updated_at
            });
        }
        return tree;
    }

    // Return visible tree nodes — only show children if their parent folder is expanded
    function visibleTree(namespace) {
        const allNodes = notesTreesRaw.value[namespace];
        if (!allNodes) return [];

        const result = [];
        for (const node of allNodes) {
            if (node.depth === 1) {
                result.push(node);
                continue;
            }
            let parentPath;
            if (node.type === 'folder') {
                const lastSlash = node.path.lastIndexOf('/');
                parentPath = lastSlash > 0 ? node.path.substring(0, lastSlash) : null;
            } else {
                const lastSlash = node.slug.lastIndexOf('/');
                parentPath = lastSlash > 0 ? node.slug.substring(0, lastSlash) : null;
            }
            if (!parentPath) {
                result.push(node);
            } else if (expandedFolders.value[namespace + '/' + parentPath]) {
                result.push(node);
            }
        }
        return result;
    }

    const notesTrees = computed(() => {
        const result = {};
        for (const ns of notesNamespaces.value) {
            result[ns.namespace] = visibleTree(ns.namespace);
        }
        return result;
    });

    async function loadNotes() {
        try {
            const data = await api('/admin/notes/namespaces');
            notesNamespaces.value = data.namespaces;
            for (const ns of data.namespaces) {
                const notesData = await api('/admin/notes/list', { namespace: ns.namespace, limit: 500 });
                notesTreesRaw.value[ns.namespace] = buildTree(notesData.notes);
            }
        } catch (err) {
            console.error('Failed to load notes:', err);
        }
    }

    function toggleNamespace(namespace) {
        expandedNamespaces.value[namespace] = !expandedNamespaces.value[namespace];
    }

    function toggleFolder(namespace, path) {
        const key = namespace + '/' + path;
        expandedFolders.value[key] = !expandedFolders.value[key];
    }

    async function openNote(namespace, slug) {
        notesEditing.value = false;
        try {
            const data = await api('/admin/notes/read', { namespace, slug });
            selectedNote.value = { ...data.note, namespace };
        } catch (err) {
            console.error('Failed to open note:', err);
        }
    }

    async function openNoteFromSearch(result) {
        await openNote(result.namespace, result.source_file);
    }

    function startEditNote() {
        notesEditing.value = true;
        notesEditTitle.value = selectedNote.value.title;
        notesEditContent.value = selectedNote.value.content;
    }

    function cancelEditNote() {
        notesEditing.value = false;
    }

    async function saveEditedNote() {
        notesSaving.value = true;
        try {
            await api('/admin/notes/save', {
                namespace: selectedNote.value.namespace,
                slug: selectedNote.value.slug,
                title: notesEditTitle.value,
                content: notesEditContent.value
            });
            selectedNote.value.title = notesEditTitle.value;
            selectedNote.value.content = notesEditContent.value;
            notesEditing.value = false;
        } catch (err) {
            console.error('Failed to save note:', err);
            showToast('Failed to save: ' + err.message, 'error');
        } finally {
            notesSaving.value = false;
        }
    }

    function confirmDeleteNote() {
        const ns = selectedNote.value.namespace;
        const slug = selectedNote.value.slug;
        showConfirm('Delete "' + slug + '"?', async () => {
            try {
                await api('/admin/notes/delete', { namespace: ns, slug });
                selectedNote.value = null;
                await loadNotes();
                showToast('Note deleted', 'success');
            } catch (err) {
                console.error('Failed to delete note:', err);
                showToast('Failed to delete: ' + err.message, 'error');
            }
        });
    }

    async function searchNotes() {
        if (!notesSearchQuery.value.trim()) return;
        try {
            const data = await api('/admin/notes/search', {
                query: notesSearchQuery.value,
                namespace: '*',
                limit: 15
            });
            notesSearchResults.value = data.results;
        } catch (err) {
            console.error('Search failed:', err);
        }
    }

    // Reindex
    function reindexNotes() {
        showConfirm('Delete ALL vector chunks and re-ingest every note? This may take a while.', async () => {
            try {
                await api('/admin/notes/reindex');
                startReindexPolling();
            } catch (err) {
                console.error('Reindex failed:', err);
                showToast('Reindex failed: ' + err.message, 'error');
            }
        });
    }

    async function pollReindexStatus() {
        try {
            const data = await api('/admin/notes/reindex-status');
            reindexStatus.value = data;
            notesReindexing.value = data.running;
            if (!data.running) {
                stopReindexPolling();
                if (data.result && !data.result.error) {
                    let msg = 'Reindex complete: ' + data.result.docs_indexed + ' docs, ' + data.result.chunks_created + ' chunks';
                    if (data.result.errors && data.result.errors.length > 0) {
                        msg += ' (' + data.result.errors.length + ' errors)';
                    }
                    showToast(msg, (data.result.errors && data.result.errors.length > 0) ? 'error' : 'success', 8000);
                } else if (data.result && data.result.error) {
                    showToast('Reindex failed: ' + data.result.error, 'error');
                }
                api('/admin/notes/reindex-clear').catch(() => {});
                reindexStatus.value = null;
            }
        } catch (err) {
            console.error('Reindex status poll failed:', err);
        }
    }

    function startReindexPolling() {
        pollReindexStatus();
        if (!reindexPollTimer) {
            reindexPollTimer = setInterval(pollReindexStatus, 2000);
        }
    }

    function stopReindexPolling() {
        if (reindexPollTimer) {
            clearInterval(reindexPollTimer);
            reindexPollTimer = null;
        }
    }

    return {
        notesNamespaces, notesTrees, expandedNamespaces, expandedFolders,
        selectedNote, notesEditing, notesEditTitle, notesEditContent, notesSaving,
        notesSearchQuery, notesSearchResults,
        notesReindexing, reindexStatus,
        loadNotes, toggleNamespace, toggleFolder,
        openNote, openNoteFromSearch,
        startEditNote, cancelEditNote, saveEditedNote, confirmDeleteNote,
        searchNotes, reindexNotes, pollReindexStatus
    };
}

window.useNotes = useNotes;

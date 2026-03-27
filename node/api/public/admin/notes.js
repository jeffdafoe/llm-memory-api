// notes.js — Notes browser (tree view, editor, search, reindex)
import { ref, computed, watch, nextTick } from 'vue';
import { marked } from 'marked';
import DOMPurify from 'dompurify';
import mermaid from 'mermaid';
import svgPanZoom from 'svg-pan-zoom';

// Initialize mermaid — startOnLoad false since we render manually
mermaid.initialize({
    startOnLoad: false,
    theme: 'base',
    themeVariables: {
        background: '#1c1c30',
        lineColor: '#e4e4e7',
        textColor: '#e4e4e7',
        primaryColor: '#2f2f52',
        primaryBorderColor: '#4a4a6a',
        primaryTextColor: '#e4e4e7',
        secondaryColor: '#252542',
        secondaryBorderColor: '#4a4a6a',
        secondaryTextColor: '#e4e4e7',
        tertiaryColor: '#1c1c30',
        signalColor: '#e4e4e7',
        signalTextColor: '#e4e4e7',
        noteBkgColor: '#fff3cd',
        noteBorderColor: '#997a00',
        noteTextColor: '#1a1a1a',
        actorLineColor: '#3a3a55',
        actorBorder: '#4a4a6a',
        actorBkg: '#2f2f52',
        actorTextColor: '#e4e4e7',
        labelBoxBkgColor: '#2f2f52',
        labelBoxBorderColor: '#4a4a6a',
        labelTextColor: '#e4e4e7'
    }
});

function useNotes({ api, showToast, showConfirm, onEvent }) {
    const notesNamespaces = ref([]);
    const notesTreesRaw = ref({});
    const expandedNamespaces = ref({});
    const expandedFolders = ref({});
    const selectedNote = ref(null);

    // ---- Sync mappings ----
    const allSyncMappings = ref([]);         // all mappings for sync indicator icons
    const syncContextMenu = ref(null);       // { x, y, namespace, slug }
    const syncDialog = ref(null);            // { namespace, slug, localPath, actorId }
    const syncAgents = ref([]);              // agents list for dropdown
    const syncMappings = ref([]);            // current mappings for dialog display
    const syncSaving = ref(false);
    const isMermaid = computed(() => {
        if (!selectedNote.value) return false;
        if (selectedNote.value.slug.endsWith('.mmd')) return true;
        // Detect content wrapped in a ```mermaid fenced block
        const content = (selectedNote.value.content || '').trim();
        return content.startsWith('```mermaid') && content.endsWith('```');
    });
    // ---- Inline rename ----
    const renameTarget = ref(null);          // { namespace, slug, type: 'file'|'folder', originalName }
    const renameValue = ref('');
    const renameConflict = ref(null);        // { namespace, old_prefix, new_prefix, would_move, conflicts: [{slug, title, action}] }
    const renameConflictExecuting = ref(false);

    const notesSidebarCollapsed = ref(false);
    const notesFullscreen = ref(false);
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
        // Sort notes so folders appear before files at each level:
        // notes with deeper paths (containing more /) sort first within their parent,
        // then alphabetically within the same depth
        const sorted = [...notes].sort((a, b) => {
            var aParts = a.slug.split('/');
            var bParts = b.slug.split('/');
            // Compare common path segments
            var minLen = Math.min(aParts.length, bParts.length);
            for (var i = 0; i < minLen - 1; i++) {
                var cmp = aParts[i].localeCompare(bParts[i]);
                if (cmp !== 0) return cmp;
            }
            // At the divergence point: deeper paths (folders) come first
            if (aParts.length !== bParts.length) return bParts.length - aParts.length;
            // Same depth: alphabetical
            return a.slug.localeCompare(b.slug);
        });

        const tree = [];
        const folders = new Set();

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

    // Rendered HTML for the note body — markdown or mermaid SVG
    const renderedNoteContent = ref('');
    const mermaidContainer = ref(null); // template ref for the mermaid wrapper div
    let panZoomInstance = null;

    // Clean up any existing pan-zoom instance
    function destroyPanZoom() {
        if (panZoomInstance) {
            panZoomInstance.destroy();
            panZoomInstance = null;
        }
    }

    // Attach pan+zoom to the SVG inside the mermaid container
    function initPanZoom() {
        destroyPanZoom();
        if (!mermaidContainer.value) return;
        const svg = mermaidContainer.value.querySelector('svg');
        if (!svg) return;
        // Make SVG fill its container so pan-zoom has room to work
        svg.style.width = '100%';
        svg.style.height = '100%';
        svg.style.maxWidth = 'none';
        panZoomInstance = svgPanZoom(svg, {
            zoomEnabled: true,
            panEnabled: true,
            controlIconsEnabled: true,
            fit: true,
            center: true,
            minZoom: 0.25,
            maxZoom: 10,
            zoomScaleSensitivity: 0.3
        });
    }

    // Watch note identity AND content — property mutations (e.g. after save) must re-render
    watch([selectedNote, isMermaid, () => selectedNote.value?.content], async () => {
        destroyPanZoom();
        if (!selectedNote.value || !selectedNote.value.content) {
            renderedNoteContent.value = '';
            return;
        }
        if (isMermaid.value) {
            try {
                // Strip ```mermaid fences if present
                let diagram = selectedNote.value.content.trim();
                if (diagram.startsWith('```mermaid')) {
                    diagram = diagram.replace(/^```mermaid\s*\n?/, '').replace(/\n?```\s*$/, '');
                }
                // mermaid.render needs a unique ID per call
                const id = 'mermaid-' + Date.now();
                const { svg } = await mermaid.render(id, diagram);
                renderedNoteContent.value = svg;
                // Wait for DOM update, then attach pan+zoom
                await nextTick();
                initPanZoom();
            } catch (err) {
                // Show the parse error + raw content as fallback
                renderedNoteContent.value = '<pre class="mermaid-error">Mermaid error: '
                    + DOMPurify.sanitize(err.message) + '</pre>'
                    + '<pre>' + DOMPurify.sanitize(selectedNote.value.content) + '</pre>';
            }
        } else {
            renderedNoteContent.value = DOMPurify.sanitize(marked.parse(selectedNote.value.content));
        }
    }, { immediate: true });

    function toggleFullscreen() {
        notesFullscreen.value = !notesFullscreen.value;
        // Re-init pan-zoom when entering/exiting fullscreen since container size changes
        if (isMermaid.value) {
            nextTick(() => initPanZoom());
        }
    }

    // Build a flat list of all file nodes for arrow key navigation
    function getAllFileNodes() {
        const files = [];
        for (const ns of notesNamespaces.value) {
            const tree = notesTreesRaw.value[ns.namespace];
            if (!tree) continue;
            for (const node of tree) {
                if (node.type === 'file') {
                    files.push({ namespace: ns.namespace, slug: node.slug });
                }
            }
        }
        return files;
    }

    // Keyboard shortcuts: Esc exits fullscreen, Left/Right navigate notes
    function onKeydown(e) {
        // Don't capture when typing in inputs/textareas
        if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
        if (e.key === 'Escape' && notesFullscreen.value) {
            toggleFullscreen();
            return;
        }
        if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
            if (!selectedNote.value) return;
            const files = getAllFileNodes();
            const idx = files.findIndex(f => f.namespace === selectedNote.value.namespace && f.slug === selectedNote.value.slug);
            if (idx === -1) return;
            const next = e.key === 'ArrowLeft' ? idx - 1 : idx + 1;
            if (next < 0 || next >= files.length) return;
            openNote(files[next].namespace, files[next].slug);
        }
    }
    document.addEventListener('keydown', onKeydown);

    // Live note update notifications via WebSocket
    if (onEvent) {
        const opMessages = {
            saved: 'This memory was updated externally',
            edited: 'This memory was edited externally',
            deleted: 'This memory was deleted',
            restored: 'This memory was restored',
            moved: 'This memory was moved'
        };
        onEvent('note_updated', (data) => {
            if (selectedNote.value &&
                selectedNote.value.namespace === data.namespace &&
                selectedNote.value.slug === data.slug) {
                const msg = opMessages[data.operation] || 'This note changed';
                const type = data.operation === 'deleted' ? 'error' : 'info';
                showToast(msg, type, 15000, () => {
                    openNote(data.namespace, data.slug);
                });
            }
        });
    }

    async function loadNotes() {
        try {
            const data = await api('/admin/notes/namespaces');
            notesNamespaces.value = data.namespaces;
            const results = await Promise.all(
                data.namespaces.map(ns =>
                    api('/admin/notes/list', { namespace: ns.namespace, limit: 500 })
                        .then(notesData => ({ namespace: ns.namespace, notes: notesData.notes }))
                )
            );
            for (const { namespace, notes } of results) {
                notesTreesRaw.value[namespace] = buildTree(notes);
            }
            // Load sync mappings for tree indicators
            await loadAllSyncMappings();
        } catch (err) {
            console.error('Failed to load notes:', err);
        }
    }

    // Load all sync mappings for displaying sync indicators in the tree
    async function loadAllSyncMappings() {
        try {
            const data = await api('/admin/notes/sync/list');
            allSyncMappings.value = data.mappings || [];
        } catch (err) {
            console.error('Failed to load sync mappings:', err);
            allSyncMappings.value = [];
        }
    }

    // Check if a note or folder has a sync mapping configured.
    // For folders (slug ends with '/'), checks exact match on the folder prefix.
    // For notes, checks exact match on the slug.
    function isSynced(namespace, slug) {
        return allSyncMappings.value.some(m => m.namespace === namespace && m.slug === slug);
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
                showToast('Memory deleted', 'success');
            } catch (err) {
                console.error('Failed to delete memory:', err);
                showToast('Failed to delete: ' + err.message, 'error');
            }
        });
    }

    // Start inline rename for a file or folder in the tree
    function startRename(namespace, slug, type, name) {
        renameTarget.value = { namespace, slug, type, originalName: name };
        renameValue.value = name;
    }

    function cancelRename() {
        renameTarget.value = null;
        renameValue.value = '';
    }

    async function commitRename() {
        const target = renameTarget.value;
        if (!target) return;

        // Clear immediately to prevent double-fire from blur after Enter
        renameTarget.value = null;

        const newName = renameValue.value.trim();
        if (newName === target.originalName) {
            renameValue.value = '';
            return;
        }
        // Empty name on a file is invalid; on a folder it means "collapse into parent"
        if (!newName && target.type === 'file') {
            renameValue.value = '';
            return;
        }

        try {
            if (target.type === 'file') {
                // Replace the last segment of the slug with the new name
                const parts = target.slug.split('/');
                parts[parts.length - 1] = newName;
                const newSlug = parts.join('/');

                await api('/admin/notes/move', {
                    namespace: target.namespace,
                    slug: target.slug,
                    new_slug: newSlug
                });
                showToast('Memory renamed', 'success');

                // Update selected note if it was the renamed one
                if (selectedNote.value && selectedNote.value.namespace === target.namespace && selectedNote.value.slug === target.slug) {
                    selectedNote.value.slug = newSlug;
                }
                await loadNotes();
            } else {
                // Folder rename: dry-run first to check for conflicts.
                // Empty name means "collapse into parent" (move contents up one level).
                const oldPrefix = target.slug;
                let newPrefix;
                if (!newName) {
                    // Collapse: parent prefix is everything before the last segment
                    // e.g. "notes/archive/" -> "notes/"
                    const trimmed = oldPrefix.slice(0, -1);
                    const lastSlash = trimmed.lastIndexOf('/');
                    newPrefix = lastSlash >= 0 ? trimmed.substring(0, lastSlash + 1) : '';
                } else {
                    const parts = oldPrefix.slice(0, -1).split('/');
                    parts[parts.length - 1] = newName;
                    newPrefix = parts.join('/') + '/';
                }

                const result = await api('/admin/notes/move-prefix', {
                    namespace: target.namespace,
                    old_prefix: oldPrefix,
                    new_prefix: newPrefix,
                    dry_run: true
                });

                if (result.conflicts.length > 0) {
                    // Show conflict resolution dialog
                    renameConflict.value = {
                        namespace: target.namespace,
                        old_prefix: oldPrefix,
                        new_prefix: newPrefix,
                        would_move: result.would_move,
                        conflicts: result.conflicts.map(c => ({ ...c, action: 'skip' }))
                    };
                } else {
                    // No conflicts — execute directly
                    await api('/admin/notes/move-prefix', {
                        namespace: target.namespace,
                        old_prefix: oldPrefix,
                        new_prefix: newPrefix
                    });
                    showToast('Folder renamed: ' + result.would_move + ' notes moved', 'success');
                    await loadNotes();
                }
            }
        } catch (err) {
            console.error('Failed to rename:', err);
            showToast('Failed to rename: ' + err.message, 'error');
        } finally {
            renameValue.value = '';
        }
    }

    // Set all conflict actions at once (skip all / overwrite all)
    function setBulkConflictAction(action) {
        if (!renameConflict.value) return;
        for (const c of renameConflict.value.conflicts) {
            c.action = action;
        }
    }

    function cancelRenameConflict() {
        renameConflict.value = null;
        renameConflictBulkAction.value = '';
        renameConflictExecuting.value = false;
    }

    // Execute the folder rename with the user's conflict resolution choices
    async function executeRenameWithConflicts() {
        const data = renameConflict.value;
        if (!data) return;
        renameConflictExecuting.value = true;

        try {
            const overwriteSlugs = data.conflicts
                .filter(c => c.action === 'overwrite')
                .map(c => c.slug);

            const result = await api('/admin/notes/move-prefix', {
                namespace: data.namespace,
                old_prefix: data.old_prefix,
                new_prefix: data.new_prefix,
                overwrite_slugs: overwriteSlugs
            });

            let msg = result.moved + ' notes moved';
            if (result.overwritten > 0) {
                msg += ', ' + result.overwritten + ' overwritten';
            }
            if (result.skipped > 0) {
                msg += ', ' + result.skipped + ' skipped';
            }
            showToast(msg, 'success');
            cancelRenameConflict();
            await loadNotes();
        } catch (err) {
            console.error('Failed to rename folder:', err);
            showToast('Failed to rename: ' + err.message, 'error');
            renameConflictExecuting.value = false;
        }
    }

    // Handle keydown in rename input
    function renameKeydown(event) {
        if (event.key === 'Enter') {
            event.preventDefault();
            commitRename();
        } else if (event.key === 'Escape') {
            event.preventDefault();
            cancelRename();
        }
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

    function inferExtension(note) {
        // Use stored extension if available
        if (note.extension) return note.extension;
        // Infer from content
        const content = (note.content || '').trim();
        if (content.startsWith('```mermaid') || note.slug.endsWith('.mmd')) return '.mmd';
        return '.md';
    }

    function downloadNote() {
        if (!selectedNote.value) return;
        const slug = selectedNote.value.slug;
        const baseName = slug.substring(slug.lastIndexOf('/') + 1);
        const ext = inferExtension(selectedNote.value);
        const filename = baseName + ext;
        const blob = new Blob([selectedNote.value.content], { type: 'text/plain' });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = filename;
        a.click();
        URL.revokeObjectURL(url);
    }

    async function uploadNote(event) {
        const file = event.target.files[0];
        if (!file || !selectedNote.value) return;
        event.target.value = ''; // reset input so same file can be re-uploaded
        const content = await file.text();
        // Extract extension from uploaded filename
        const dotIndex = file.name.lastIndexOf('.');
        const extension = dotIndex > 0 ? file.name.substring(dotIndex) : null;
        try {
            await api('/admin/notes/save', {
                namespace: selectedNote.value.namespace,
                slug: selectedNote.value.slug,
                title: selectedNote.value.title,
                content,
                extension
            });
            selectedNote.value.content = content;
            if (extension) selectedNote.value.extension = extension;
            showToast('Uploaded: ' + file.name, 'success');
        } catch (err) {
            showToast('Upload failed: ' + err.message, 'error');
        }
    }

    // Reindex
    function reindexNotes() {
        showConfirm('Delete ALL vector chunks and re-ingest every memory? This may take a while.', async () => {
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

    // ---- Sync mapping context menu and dialog ----

    // Show context menu on right-click of a tree node
    function showSyncContextMenu(event, namespace, slug) {
        event.preventDefault();
        syncContextMenu.value = { x: event.clientX, y: event.clientY, namespace, slug };
    }

    // Close context menu (called on any click)
    function closeSyncContextMenu() {
        syncContextMenu.value = null;
    }

    // Start rename from context menu
    function startRenameFromContext() {
        const ctx = syncContextMenu.value;
        if (!ctx) return;
        syncContextMenu.value = null;

        // Determine the type and name from the slug
        if (ctx.slug.endsWith('/')) {
            // Folder: slug is like "path/to/folder/", name is last segment
            const trimmed = ctx.slug.slice(0, -1);
            const name = trimmed.split('/').pop();
            startRename(ctx.namespace, ctx.slug, 'folder', name);
        } else {
            // File: name is last segment of slug
            const name = ctx.slug.split('/').pop();
            startRename(ctx.namespace, ctx.slug, 'file', name);
        }
    }

    // Delete from context menu — handles both single notes and folders
    function deleteFromContext() {
        const ctx = syncContextMenu.value;
        if (!ctx) return;
        syncContextMenu.value = null;

        if (ctx.slug.endsWith('/')) {
            // Folder: list all notes under this prefix and confirm bulk delete
            api('/admin/notes/list', { namespace: ctx.namespace, prefix: ctx.slug }).then(data => {
                const notes = data.notes || [];
                if (notes.length === 0) {
                    showToast('Folder is empty', 'info');
                    return;
                }
                const slugList = notes.map(n => n.slug).join('\n  • ');
                showConfirm('Delete ' + notes.length + ' ' + (notes.length === 1 ? 'memory' : 'memories') + ' in "' + ctx.slug + '"?\n\n  • ' + slugList, async () => {
                    let deleted = 0;
                    for (const note of notes) {
                        try {
                            await api('/admin/notes/delete', { namespace: ctx.namespace, slug: note.slug });
                            deleted++;
                        } catch (err) {
                            console.error('Failed to delete ' + note.slug + ':', err);
                        }
                    }
                    if (selectedNote.value && selectedNote.value.namespace === ctx.namespace && selectedNote.value.slug.startsWith(ctx.slug)) {
                        selectedNote.value = null;
                    }
                    await loadNotes();
                    showToast(deleted + ' ' + (deleted === 1 ? 'memory' : 'memories') + ' deleted', 'success');
                });
            }).catch(err => {
                showToast('Failed to list folder contents: ' + err.message, 'error');
            });
        } else {
            // Single note
            showConfirm('Delete "' + ctx.slug + '"?', async () => {
                try {
                    await api('/admin/notes/delete', { namespace: ctx.namespace, slug: ctx.slug });
                    if (selectedNote.value && selectedNote.value.namespace === ctx.namespace && selectedNote.value.slug === ctx.slug) {
                        selectedNote.value = null;
                    }
                    await loadNotes();
                    showToast('Memory deleted', 'success');
                } catch (err) {
                    console.error('Failed to delete memory:', err);
                    showToast('Failed to delete: ' + err.message, 'error');
                }
            });
        }
    }

    // Open the sync dialog from the context menu
    async function openSyncDialog() {
        const ctx = syncContextMenu.value;
        if (!ctx) return;
        syncDialog.value = { namespace: ctx.namespace, slug: ctx.slug, localPath: '', actorId: null };
        syncContextMenu.value = null;

        // Load agents list for the dropdown (uses agents endpoint which respects visibility)
        if (syncAgents.value.length === 0) {
            try {
                const data = await api('/admin/agents');
                syncAgents.value = (data.agents || []).map(a => ({ id: a.actor_id, name: a.agent }));
            } catch (err) {
                console.error('Failed to load agents:', err);
            }
        }

        // Load existing mappings for this namespace+slug
        await loadSyncMappings(ctx.namespace, ctx.slug);
    }

    async function loadSyncMappings(namespace, slug) {
        try {
            const data = await api('/admin/notes/sync/list');
            // Filter to mappings that match this namespace+slug
            syncMappings.value = (data.mappings || []).filter(m => m.namespace === namespace && m.slug === slug);
        } catch (err) {
            console.error('Failed to load sync mappings:', err);
            syncMappings.value = [];
        }
    }

    async function saveSyncMapping() {
        const dlg = syncDialog.value;
        if (!dlg || !dlg.actorId || !dlg.localPath) return;
        syncSaving.value = true;
        try {
            await api('/admin/notes/sync/save', {
                actor_id: dlg.actorId,
                namespace: dlg.namespace,
                slug: dlg.slug,
                local_path: dlg.localPath
            });
            showToast('Sync mapping saved', 'success');
            // Reload mappings to show the new one + update tree indicators
            await loadSyncMappings(dlg.namespace, dlg.slug);
            await loadAllSyncMappings();
            // Reset form fields but keep dialog open
            dlg.localPath = '';
            dlg.actorId = null;
        } catch (err) {
            showToast('Failed to save: ' + err.message, 'error');
        } finally {
            syncSaving.value = false;
        }
    }

    async function deleteSyncMapping(id) {
        try {
            await api('/admin/notes/sync/delete', { id });
            showToast('Mapping removed', 'success');
            if (syncDialog.value) {
                await loadSyncMappings(syncDialog.value.namespace, syncDialog.value.slug);
            }
            await loadAllSyncMappings();
        } catch (err) {
            showToast('Failed to delete: ' + err.message, 'error');
        }
    }

    function closeSyncDialog() {
        syncDialog.value = null;
        syncMappings.value = [];
    }

    // Close context menu on any click anywhere
    document.addEventListener('click', closeSyncContextMenu);

    // Sidebar resize via drag handle
    function startSidebarResize(e) {
        e.preventDefault();
        const sidebar = e.target.previousElementSibling;
        const startX = e.clientX;
        const startWidth = sidebar.getBoundingClientRect().width;

        function onMouseMove(ev) {
            const newWidth = startWidth + (ev.clientX - startX);
            if (newWidth >= 150 && newWidth <= 800) {
                sidebar.style.width = newWidth + 'px';
                sidebar.style.minWidth = newWidth + 'px';
            }
        }

        function onMouseUp() {
            document.removeEventListener('mousemove', onMouseMove);
            document.removeEventListener('mouseup', onMouseUp);
            document.body.style.cursor = '';
            document.body.style.userSelect = '';
        }

        document.addEventListener('mousemove', onMouseMove);
        document.addEventListener('mouseup', onMouseUp);
        document.body.style.cursor = 'col-resize';
        document.body.style.userSelect = 'none';
    }

    return {
        notesNamespaces, notesTrees, expandedNamespaces, expandedFolders,
        selectedNote, renderedNoteContent, isMermaid, mermaidContainer, notesSidebarCollapsed, notesFullscreen, toggleFullscreen,
        notesEditing, notesEditTitle, notesEditContent, notesSaving,
        notesSearchQuery, notesSearchResults,
        notesReindexing, reindexStatus,
        isSynced, syncContextMenu, syncDialog, syncAgents, syncMappings, syncSaving,
        renameTarget, renameValue,
        renameConflict, renameConflictExecuting,
        loadNotes, toggleNamespace, toggleFolder,
        openNote, openNoteFromSearch,
        startEditNote, cancelEditNote, saveEditedNote, confirmDeleteNote,
        startRename, cancelRename, commitRename, renameKeydown, startRenameFromContext, deleteFromContext,
        setBulkConflictAction, cancelRenameConflict, executeRenameWithConflicts,
        downloadNote, uploadNote,
        searchNotes, reindexNotes, pollReindexStatus, stopReindexPolling,
        showSyncContextMenu, closeSyncContextMenu, openSyncDialog,
        saveSyncMapping, deleteSyncMapping, closeSyncDialog,
        startSidebarResize
    };
}

export { useNotes };

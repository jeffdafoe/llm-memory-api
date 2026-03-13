// permissions.js — Namespace permissions viewer/editor

function usePermissions({ api, showToast, showConfirm }) {
    const permissions = ref([]);
    const permissionsLoading = ref(false);

    // New permission form
    const newPermAgent = ref('');
    const newPermNamespace = ref('');
    const newPermRead = ref(true);
    const newPermWrite = ref(true);
    const newPermDelete = ref(false);
    const permSaving = ref(false);

    async function loadPermissions() {
        permissionsLoading.value = true;
        try {
            const data = await api('/admin/permissions/list');
            permissions.value = data.permissions;
        } catch (err) {
            console.error('Failed to load permissions:', err);
        } finally {
            permissionsLoading.value = false;
        }
    }

    async function togglePermission(perm, field) {
        if (perm.implicit) return;
        const updated = { ...perm, [field]: !perm[field] };
        try {
            await api('/admin/permissions/upsert', {
                agent: updated.agent,
                namespace: updated.namespace,
                can_read: updated.can_read,
                can_write: updated.can_write,
                can_delete: updated.can_delete
            });
            perm[field] = !perm[field];
        } catch (err) {
            showToast('Failed: ' + err.message, 'error');
        }
    }

    async function addPermission() {
        if (!newPermAgent.value || !newPermNamespace.value) {
            showToast('Agent and namespace are required', 'error');
            return;
        }
        permSaving.value = true;
        try {
            await api('/admin/permissions/upsert', {
                agent: newPermAgent.value,
                namespace: newPermNamespace.value,
                can_read: newPermRead.value,
                can_write: newPermWrite.value,
                can_delete: newPermDelete.value
            });
            newPermAgent.value = '';
            newPermNamespace.value = '';
            newPermRead.value = true;
            newPermWrite.value = true;
            newPermDelete.value = false;
            showToast('Permission added', 'success');
            await loadPermissions();
        } catch (err) {
            showToast('Failed: ' + err.message, 'error');
        } finally {
            permSaving.value = false;
        }
    }

    async function deletePermission(perm) {
        if (perm.implicit) return;
        const confirmed = await showConfirm(`Remove ${perm.agent} access to ${perm.namespace}?`);
        if (!confirmed) return;
        try {
            await api('/admin/permissions/delete', { id: perm.id });
            showToast('Permission removed', 'success');
            await loadPermissions();
        } catch (err) {
            showToast('Failed: ' + err.message, 'error');
        }
    }

    return {
        permissions, permissionsLoading,
        newPermAgent, newPermNamespace, newPermRead, newPermWrite, newPermDelete, permSaving,
        loadPermissions, togglePermission, addPermission, deletePermission
    };
}

window.usePermissions = usePermissions;

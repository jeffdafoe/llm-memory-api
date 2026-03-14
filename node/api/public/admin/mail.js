// mail.js — Mail view (list, detail, compose)
import { ref } from 'vue';

function useMail({ api, showToast }) {
    const mailMessages = ref([]);
    const selectedMail = ref(null);
    const mailComposing = ref(false);
    const mailTo = ref('');
    const mailSubject = ref('');
    const mailBody = ref('');
    const mailSending = ref(false);

    async function loadMail() {
        try {
            const data = await api('/admin/mail', { limit: 100 });
            mailMessages.value = data.messages;
        } catch (err) {
            console.error('Failed to load mail:', err);
        }
    }

    function viewMail(msg) {
        selectedMail.value = msg;
    }

    function startComposeMail() {
        mailComposing.value = true;
        mailTo.value = '';
        mailSubject.value = '';
        mailBody.value = '';
    }

    function cancelComposeMail() {
        mailComposing.value = false;
    }

    async function sendMail() {
        if (!mailTo.value || !mailSubject.value || !mailBody.value) return;
        mailSending.value = true;
        try {
            await api('/admin/mail/send', {
                to: mailTo.value,
                subject: mailSubject.value,
                body: mailBody.value
            });
            mailComposing.value = false;
            loadMail();
        } catch (err) {
            console.error('Failed to send mail:', err);
            showToast('Failed to send: ' + err.message, 'error');
        } finally {
            mailSending.value = false;
        }
    }

    async function deleteMail(msg) {
        try {
            await api('/admin/mail/delete', { id: msg.id });
            mailMessages.value = mailMessages.value.filter(m => m.id !== msg.id);
            selectedMail.value = null;
            showToast('Mail deleted');
        } catch (err) {
            console.error('Failed to delete mail:', err);
            showToast('Failed to delete: ' + err.message, 'error');
        }
    }

    function closeDialogs() {
        selectedMail.value = null;
    }

    return {
        mailMessages, selectedMail,
        mailComposing, mailTo, mailSubject, mailBody, mailSending,
        loadMail, viewMail, startComposeMail, cancelComposeMail, sendMail, deleteMail,
        closeDialogs
    };
}

export { useMail };

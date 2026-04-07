// Edit mode functionality
let originalMarkdown = '';

function getCurrentFilePath() {
    // Prefer data-file attribute (set for / and /memory default file views)
    const content = document.getElementById('content');
    if (content && content.dataset.file) {
        return '/' + content.dataset.file;
    }
    // Fall back to URL for /view/<path> routes
    const pathname = window.location.pathname.startsWith('/view/')
        ? window.location.pathname.replace('/view/', '/')
        : window.location.pathname;
    return decodeURIComponent(pathname);
}

async function toggleEditMode() {
    const editor = document.getElementById('markdown-editor');
    const editorContainer = document.getElementById('editor-container');

    if (!editor || !editorContainer) {
        console.error('Editor elements not found');
        return;
    }

    if (!originalMarkdown) {
        try {
            const filePath = getCurrentFilePath();
            const response = await fetch(`/raw${filePath}`);
            if (!response.ok) throw new Error('Failed to load file');
            originalMarkdown = await response.text();
            editor.value = originalMarkdown;
        } catch (err) {
            showErrorToast('Failed to load file for editing: ' + err.message);
            return;
        }
    }

    editorContainer.classList.add('active');
    editor.focus();
}

function cancelEdit() {
    const editor = document.getElementById('markdown-editor');
    const editorContainer = document.getElementById('editor-container');

    if (editor && editorContainer) {
        editor.value = originalMarkdown;
        editorContainer.classList.remove('active');
    }
}

async function saveMarkdown() {
    const editor = document.getElementById('markdown-editor');
    const content = editor.value;
    const filePath = getCurrentFilePath();

    if (content === originalMarkdown) {
        const editorContainer = document.getElementById('editor-container');
        if (editorContainer) editorContainer.classList.remove('active');
        return;
    }

    try {
        const response = await fetch('/save', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/x-www-form-urlencoded',
            },
            body: `file=${encodeURIComponent(filePath)}&content=${encodeURIComponent(content)}`
        });

        if (!response.ok) {
            const errorText = await response.text();
            throw new Error(errorText || 'Save failed');
        }

        originalMarkdown = content;
        const editorContainer = document.getElementById('editor-container');
        if (editorContainer) {
            editorContainer.classList.remove('active');
        }

        console.log('[Editor] File saved, waiting for SSE update...');
    } catch (err) {
        showErrorToast('Failed to save: ' + err.message);
    }
}

// Ctrl+S to save
document.addEventListener('keydown', function(e) {
    if ((e.ctrlKey || e.metaKey) && e.key === 's') {
        e.preventDefault();
        const editorContainer = document.getElementById('editor-container');
        if (editorContainer && editorContainer.classList.contains('active')) {
            saveMarkdown();
        }
    }
});

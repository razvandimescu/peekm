// Edit mode functionality
let originalMarkdown = '';
let autoSaveTimeout = null;
const AUTO_SAVE_DEBOUNCE_MS = 300;

function getCurrentFilePath() {
    // For browser mode (SPA), get from window.location.pathname
    // For single-file mode, use window.location.pathname
    const pathname = window.location.pathname.startsWith('/view/')
        ? window.location.pathname.replace('/view/', '/')
        : window.location.pathname;

    // Decode the URL-encoded path (e.g., npm%2FREADME.md -> npm/README.md)
    return decodeURIComponent(pathname);
}

async function toggleEditMode() {
    const editor = document.getElementById('markdown-editor');
    const editorContainer = document.getElementById('editor-container');
    const editButton = document.querySelector('.edit-button');

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
            alert('Failed to load file for editing: ' + err.message);
            return;
        }
    }

    editorContainer.classList.add('active');
    editor.focus();

    // Setup debounced auto-save (only once per editor session)
    if (!editor.dataset.autoSaveEnabled) {
        editor.addEventListener('input', handleEditorInput);
        editor.dataset.autoSaveEnabled = 'true';
    }
}

function handleEditorInput() {
    // Clear existing timeout
    if (autoSaveTimeout) {
        clearTimeout(autoSaveTimeout);
    }

    // Schedule new auto-save after debounce period
    autoSaveTimeout = setTimeout(() => {
        autoSaveMarkdown();
    }, AUTO_SAVE_DEBOUNCE_MS);
}

async function autoSaveMarkdown() {
    const editor = document.getElementById('markdown-editor');
    const content = editor.value;
    const filePath = getCurrentFilePath();

    // Don't auto-save if content hasn't changed
    if (content === originalMarkdown) {
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
            console.error('[Editor] Auto-save failed:', errorText);
            return;
        }

        originalMarkdown = content;
        console.log('[Editor] Auto-saved');
    } catch (err) {
        console.error('[Editor] Auto-save error:', err.message);
    }
}

function cancelEdit() {
    const editor = document.getElementById('markdown-editor');
    const editorContainer = document.getElementById('editor-container');

    // Clear any pending auto-save
    if (autoSaveTimeout) {
        clearTimeout(autoSaveTimeout);
        autoSaveTimeout = null;
    }

    if (editor && editorContainer) {
        editor.value = originalMarkdown;
        editorContainer.classList.remove('active');
    }
}

async function saveMarkdown() {
    const editor = document.getElementById('markdown-editor');
    const content = editor.value;
    const filePath = getCurrentFilePath();

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

        // SSE will automatically trigger preview update - no reload needed
        console.log('[Editor] File saved, waiting for SSE update...');
    } catch (err) {
        alert('Failed to save: ' + err.message);
    }
}

// Ctrl+S to save
document.addEventListener('keydown', function(e) {
    if (e.ctrlKey && e.key === 's') {
        e.preventDefault();
        const editorContainer = document.getElementById('editor-container');
        if (editorContainer && editorContainer.classList.contains('active')) {
            saveMarkdown();
        }
    }
});

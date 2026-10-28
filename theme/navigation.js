// SPA Navigation - maintains persistent SSE connection across page transitions
// This module handles client-side routing and content swapping

// Global state
let eventSource = null;
let reconnectAttempts = 0;
const maxReconnectDelay = 30000; // 30 seconds max
let refreshTreeTimer = null; // For debouncing tree refreshes

// Expand/collapse for any control whose next sibling is its panel (timeline
// session cards, Recent-mode bulk-write groups). Carets rotate from CSS on
// [aria-expanded="true"].
function toggleExpander(control) {
    var panel = control.nextElementSibling;
    var expanded = control.getAttribute('aria-expanded') === 'true';
    control.setAttribute('aria-expanded', String(!expanded));
    panel.style.display = expanded ? 'none' : '';
}

// Timeline filter toggle (All / Edits only)
function setTimelineFilter(filter) {
    var container = document.querySelector('.container');
    if (!container) return;
    container.classList.toggle('timeline-filter-edits', filter === 'edits');
    document.querySelectorAll('.timeline-filter-btn').forEach(function(btn) {
        btn.classList.toggle('active', btn.getAttribute('data-filter') === filter);
    });
    sessionStorage.setItem('peekm_timeline_filter', filter);
}

// Transcript image lightbox (event delegation — safe to call multiple times)
var _lightboxInitialized = false;
function initTranscriptLightbox() {
    if (_lightboxInitialized) return;
    _lightboxInitialized = true;
    document.addEventListener('click', function(e) {
        var img = e.target.closest('.transcript-image img');
        if (img) {
            var lb = document.getElementById('transcript-lightbox');
            if (lb) { lb.querySelector('img').src = img.src; lb.hidden = false; }
        }
    });
    document.addEventListener('click', function(e) {
        var lb = e.target.closest('.transcript-lightbox');
        if (lb) lb.hidden = true;
    });
    document.addEventListener('keydown', function(e) {
        if (e.key === 'Escape') {
            var lb = document.getElementById('transcript-lightbox');
            if (lb && !lb.hidden) lb.hidden = true;
        }
    });
}

// Restore timeline filter from sessionStorage on page load
function restoreTimelineFilter() {
    var filter = sessionStorage.getItem('peekm_timeline_filter');
    if (filter === 'edits') setTimelineFilter('edits');
}

// Connect to SSE and maintain persistent connection
function connectSSE() {
    if (eventSource && eventSource.readyState !== EventSource.CLOSED) {
        console.log('[SSE] Already connected');
        return;
    }

    eventSource = new EventSource('/events');

    eventSource.onopen = function() {
        console.log('[SSE] Connected');
        reconnectAttempts = 0;

        // Show connected state immediately
        const dot = document.getElementById('connection-dot');
        if (dot) {
            dot.classList.add('connected');
        }
    };

    eventSource.onmessage = function(event) {
        console.log('[SSE] Received message:', event.data);

        // Try to parse as JSON for typed messages
        try {
            const data = JSON.parse(event.data);
            console.log('[SSE] Parsed data:', data);

            if (data.type === 'file_added') {
                console.log('[SSE] Handling file_added for:', data.path);
                showToast(`New file: ${data.path}`, data.path, data.session);
                // Debounced refresh from server (batches rapid updates)
                scheduleTreeRefresh();
            } else if (data.type === 'files_added') {
                // Bulk write: one toast; the debounced refresh brings the batch in.
                showToast(`${data.count} files added: ${data.path}`);
                scheduleTreeRefresh();
            } else if (data.type === 'file_removed') {
                console.log('[SSE] Handling file_removed for:', data.path);
                // Optimistic update: remove immediately
                removeFileFromTree(data.path);
                // Self-healing: debounced refresh from server
                scheduleTreeRefresh();
            } else if (data.type === 'file_modified') {
                console.log('[SSE] Handling file_modified for:', data.path);

                // Viewing this file (at /view/ or as the default file at /): refresh in place
                const content = document.getElementById('content');
                if (content && content.dataset.view === 'file' && getCurrentFilePath() === '/' + data.path) {
                    console.log('[SSE] Auto-refreshing current page');
                    reloadContent();

                    // Show notification if modified by Claude Code session
                    if (data.planTitle) {
                        showToast(`Plan updated: ${data.planTitle}`, data.path, data.session);
                    } else if (data.session) {
                        showToast(`Updated by Claude: ${data.path}`, data.path, data.session);
                    }
                } else {
                    fileModifiedToast(data);
                }
            } else if (data.type === 'day_modified') {
                scheduleTodayRefresh(data.path);
            } else if (data.type === 'session_activity') {
                scheduleTranscriptRefresh(data.session);
                scheduleTimelineRefresh();
            } else if (data.type === 'connection_status') {
                console.log('[SSE] Handling connection_status:', data.count);
                updateConnectionStatus(data.count);
            }
        } catch (e) {
            console.log('[SSE] Ignoring non-JSON message:', event.data);
        }
    };

    eventSource.onerror = function(error) {
        console.log('[SSE] Connection error, reconnecting...');
        eventSource.close();

        // Show reconnecting state
        const dot = document.getElementById('connection-dot');
        if (dot) {
            dot.classList.remove('connected');
            dot.classList.add('reconnecting');
        }
        const statusEl = document.getElementById('connection-status');
        if (statusEl) {
            statusEl.title = 'Reconnecting...';
            statusEl.classList.add('disconnected');
        }

        // Exponential backoff for reconnection
        reconnectAttempts++;
        const delay = Math.min(1000 * Math.pow(2, reconnectAttempts), maxReconnectDelay);

        setTimeout(connectSSE, delay);
    };
}

// Navigate to a new URL using fetch + content swap (SPA style)
async function navigate(url, addToHistory = true) {
    try {
        // Save tree state before navigation (for browser mode)
        saveTreeState();

        // Show loading state
        const loadingContent = document.getElementById('content');
        if (loadingContent) loadingContent.classList.add('loading');

        // Fetch partial content
        const response = await fetch(url, {
            headers: {
                'X-Requested-With': 'XMLHttpRequest'
            }
        });

        if (!response.ok) {
            throw new Error(`HTTP ${response.status}`);
        }

        const html = await response.text();

        // Parse the response to extract the main content
        const parser = new DOMParser();
        const doc = parser.parseFromString(html, 'text/html');
        const newContent = doc.getElementById('content');

        if (!newContent) {
            console.error('[Navigate] No #content element found in response');
            // Fallback to full page load
            window.location.href = url;
            return;
        }

        // Replace content
        const oldContent = document.getElementById('content');
        if (oldContent) {
            oldContent.replaceWith(newContent);
        }

        // Only update sidebar tree for root navigation (directory changes)
        // File navigation (/view/*) doesn't need tree update
        if (url === '/' || url === '/memory') {
            const newSidebarTree = doc.getElementById('sidebar-tree');
            const oldSidebarTree = document.getElementById('sidebar-tree');
            if (newSidebarTree && oldSidebarTree) {
                oldSidebarTree.innerHTML = newSidebarTree.innerHTML;
            }
        }

        // Track navigation origin for memory mode
        if (url === '/memory') {
            sessionStorage.setItem('peekm_memory_mode', 'true');
        } else if (url === '/' || url.startsWith('/timeline') || url.startsWith('/transcript')) {
            sessionStorage.removeItem('peekm_memory_mode');
        }

        // Update browser history
        if (addToHistory) {
            history.pushState({ url }, '', url);
        }

        // Reinitialize page-specific scripts
        reinitializeScripts();

        // Restore tree state after DOM update (for browser mode)
        restoreTreeState();

        // Auto-expand parent directories for file navigation
        if (url.startsWith('/view/')) {
            const filePath = url.replace('/view/', '');
            expandParentDirectories(filePath);
        }

        // On mobile, dismiss the overlay sidebar after navigation so the
        // tapped content becomes visible (standard mobile drawer pattern).
        collapseSidebarOnMobile();

        console.log('[Navigate] Navigated to:', url);
    } catch (error) {
        console.error('[Navigate] Error:', error);
        const errorContent = document.getElementById('content');
        if (errorContent) errorContent.classList.remove('loading');
        // Fallback to full page load
        window.location.href = url;
    }
}

// Reinitialize page-specific functionality after content swap
// Update active state on header nav buttons based on current route
function updateNavButtons() {
    const path = window.location.pathname;
    const inMemoryMode = sessionStorage.getItem('peekm_memory_mode') === 'true';
    const isView = path.startsWith('/view/');
    const filesBtn = document.getElementById('files-btn');
    const timelineBtn = document.getElementById('timeline-btn');
    const standupBtn = document.getElementById('standup-btn');
    const todayBtn = document.getElementById('today-btn');
    const memoryBtn = document.getElementById('memory-btn');
    if (filesBtn) filesBtn.classList.toggle('active', path === '/' || (isView && !inMemoryMode));
    if (timelineBtn) timelineBtn.classList.toggle('active', path.startsWith('/timeline') || path.startsWith('/transcript'));
    if (standupBtn) standupBtn.classList.toggle('active', path.startsWith('/standup'));
    if (todayBtn) todayBtn.classList.toggle('active', path.startsWith('/today'));
    if (memoryBtn) memoryBtn.classList.toggle('active', path.startsWith('/memory') || (isView && inMemoryMode));
}

// Standup: larger text for reading aloud, persisted alongside the theme preference.
function toggleStandupLarge() {
    const on = document.body.classList.toggle('standup-lg');
    try { localStorage.setItem('peekm_standup_lg', on ? '1' : '0'); } catch (e) {}
}

// Standup: copy a plain-text digest for pasting into a Slack/GitHub thread —
// the "hand a teammate who missed standup" path, without saving a file first.
function copyRecap(btn) {
    var lines = [];
    var label = document.querySelector('.standup-date-label');
    var summary = document.querySelector('.standup-summary');
    lines.push('*Recap — ' + (label ? label.textContent.trim() : '') + '*'
        + (summary ? '  ·  ' + summary.textContent.trim() : ''));
    lines.push('');
    document.querySelectorAll('.standup-project').forEach(function (p) {
        var name = p.querySelector('.standup-project-name');
        if (!name) return;
        var metrics = p.querySelector('.standup-metrics');
        var m = '';
        if (metrics) {
            // Token counts stay in the live view — never in the Slack paste.
            var clone = metrics.cloneNode(true);
            clone.querySelectorAll('.standup-tokens').forEach(function (n) { n.remove(); });
            m = clone.textContent.replace(/\s+/g, ' ').trim();
        }
        lines.push('• ' + name.textContent.trim() + (m ? ' — ' + m : ''));
    });
    var tailItems = document.querySelectorAll('.standup-tail .standup-tail-item');
    if (tailItems.length) {
        var parts = [].map.call(tailItems, function (n) { return n.textContent.replace(/\s+/g, ' ').trim(); });
        lines.push('');
        lines.push('Also touched: ' + parts.join(' · '));
    }
    var text = lines.join('\n');

    var done = function () {
        btn.classList.add('copied');
        var orig = btn.textContent;
        btn.textContent = 'Copied!';
        setTimeout(function () { btn.classList.remove('copied'); btn.textContent = orig; }, 1600);
    };
    copyText(text, done);
}
// navigator.clipboard only exists in secure contexts (HTTPS/localhost);
// over plain HTTP (e.g. a tailnet IP) fall back to execCommand.
function copyText(text, done) {
    done = done || function () {};
    if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done).catch(function () { copyFallback(text, done); });
    } else {
        copyFallback(text, done);
    }
}
function copyFallback(text, done) {
    var t = document.createElement('textarea');
    t.value = text; t.style.position = 'fixed'; t.style.opacity = '0';
    document.body.appendChild(t); t.select();
    try { document.execCommand('copy'); done(); } catch (e) {}
    t.remove();
}

// sendJSON sends body as JSON and resolves to the response; a non-2xx status
// rejects with the server's error text.
async function sendJSON(url, body, method = 'POST') {
    const resp = await fetch(url, {
        method: method,
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    });
    if (!resp.ok) throw new Error((await resp.text()).trim() || 'HTTP ' + resp.status);
    return resp;
}

// Standup: write standup-YYYY-MM-DD.md into the browse dir, then open it.
function saveStandup(date) {
    sendJSON('/standup/save', { date: date }).then(function (r) {
        return r.json();
    }).then(function (data) {
        showToast('Saved ' + data.path);
        navigate('/view/' + encodeURIComponent(data.path));
    }).catch(function () {
        showToast('Could not save standup');
    });
}

// Today: the add box takes focus on arrival, but not on live refreshes (they
// restore it themselves) or touch screens (it would pop the keyboard).
let todayRefreshing = false;
let todayRefreshTimer = null;

function focusTodayInput() {
    const input = document.getElementById('today-add-input');
    if (input && !todayRefreshing && !window.matchMedia('(hover: none)').matches) input.focus();
}

function scheduleTodayRefresh(date) {
    const content = document.getElementById('content');
    if (!content || content.dataset.view !== 'today' || content.dataset.date !== date) return;
    clearTimeout(todayRefreshTimer);
    todayRefreshTimer = setTimeout(refreshToday, 150);
}

// Re-render the current page in place, keeping the reader's scroll position.
async function reloadContent() {
    const scrollTop = document.getElementById('content').scrollTop;
    await navigate(window.location.pathname + window.location.search, false);
    const fresh = document.getElementById('content');
    if (fresh) fresh.scrollTop = scrollTop;
}

// Live refresh of Today that also keeps a half-typed task and its focus.
async function refreshToday() {
    const content = document.getElementById('content');
    if (!content || content.dataset.view !== 'today') return;
    const input = document.getElementById('today-add-input');
    const draft = input ? input.value : '';
    const focused = input !== null && document.activeElement === input;

    todayRefreshing = true;
    await reloadContent();
    todayRefreshing = false;

    const freshInput = document.getElementById('today-add-input');
    if (freshInput) {
        freshInput.value = draft;
        if (focused) freshInput.focus();
    }
}

function addTodayTask(e, form) {
    e.preventDefault();
    const input = form.elements.task;
    const task = input.value.trim();
    if (!task) return;
    const date = document.getElementById('content').dataset.date;
    sendJSON('/today/add', { date: date, items: [task] }).then(function () {
        input.value = '';
        scheduleTodayRefresh(date);
    }).catch(function (err) {
        showErrorToast('Could not add task: ' + err.message);
    });
}

// Carry-over copies the ticked leftovers into this day's plan; the old plan
// keeps them as written. Either button creates the file, which ends the offer.
function carryOver(btn, bringOver) {
    const banner = btn.closest('.today-carry');
    const items = bringOver
        ? Array.from(banner.querySelectorAll('input:checked'), function (box) { return box.value; })
        : [];
    const date = document.getElementById('content').dataset.date;
    sendJSON('/today/add', { date: date, items: items }).then(refreshToday).then(focusTodayInput).catch(function (err) {
        showErrorToast('Could not update the plan: ' + err.message);
    });
}

// Task-list checkboxes write their state back to the "[ ]" in the source file.
function initTaskCheckboxes() {
    document.querySelectorAll('#content input.task-list-item-checkbox').forEach(function (box, index) {
        box.disabled = false;
        box.addEventListener('change', function () {
            sendJSON('/task/toggle', {
                file: getCurrentFilePath(),
                index: index,
                task: box.dataset.task,
                checked: box.checked
            }).catch(function (err) {
                box.checked = !box.checked;
                showErrorToast('Could not update task: ' + err.message);
            });
        });
    });
}

function reinitializeScripts() {
    const content = document.getElementById('content');
    if (!content) return;

    const viewType = content.dataset.view;

    try {
        updateNavButtons();
        checkShareStatus();

        // A swapped-in #content mints a fresh .markdown-body with no data-theme,
        // so a forced theme must be re-stamped onto it (see applyThemeToContent).
        if (typeof applyThemeToContent === 'function') {
            applyThemeToContent(localStorage.getItem('theme') || 'auto');
        }

        // Initialize sidebar (Focus Mode) - works for both views
        if (typeof initializeSidebar === 'function') {
            initializeSidebar();
        }

        // Initialize session info timestamps (if present)
        if (typeof initializeSessionInfo === 'function') {
            initializeSessionInfo();
        }

        if (viewType === 'file' || viewType === 'today') {
            initTaskCheckboxes();
        }
        if (viewType === 'today') {
            focusTodayInput();
        }

        // Restore timeline filter on SPA navigation
        if (viewType === 'timeline') {
            restoreTimelineFilter();
        }

        // Initialize transcript lightbox on SPA navigation
        if (viewType === 'transcript') {
            initTranscriptLightbox();
            initReplyBox();
            revealHashTarget();
            if (typeof window.renderIdleReadout === 'function') {
                window.renderIdleReadout();
            }
        }

        // Re-render mermaid diagrams after SPA content swap
        if (typeof window.renderMermaid === 'function') {
            window.renderMermaid();
        }

        console.log('[Reinit] Scripts reinitialized for view:', viewType);
    } catch (error) {
        console.error('[Reinit] Error during script initialization:', error);
        // Don't crash - graceful degradation
    }
}

// Reveal the element addressed by location.hash even when it sits inside
// collapsed <details> (e.g. the recap's #tool-<id> transcript deep links):
// open the ancestor chain, scroll it into view, flash it. Lives here, not in
// the partial — inline scripts don't run on SPA content swaps.
function revealHashTarget() {
    if (!location.hash) return;
    const el = document.getElementById(location.hash.slice(1));
    if (!el) return;
    for (let d = el; d; d = d.parentElement) {
        if (d.tagName === 'DETAILS') d.open = true;
    }
    el.scrollIntoView({ behavior: 'smooth', block: 'center' });
    el.classList.add('anchor-flash');
    setTimeout(function() { el.classList.remove('anchor-flash'); }, 1800);
}

// Copy button on the recap's file rows (defined here for the same SPA reason).
function copyStandupPath(e, btn, path) {
    e.preventDefault();
    e.stopPropagation();
    var done = function() {
        btn.classList.add('copied');
        setTimeout(function() { btn.classList.remove('copied'); }, 1200);
    };
    copyText(path, done);
}

// Intercept link clicks for SPA navigation
function interceptLinks(e) {
    // Find the closest <a> element
    const link = e.target.closest('a');
    if (!link) return;

    // Let browser handle Cmd/Ctrl+Click naturally (opens new tab)
    if (e.metaKey || e.ctrlKey) {
        return; // Don't prevent default - let browser handle it
    }

    // Only intercept internal links
    const url = link.getAttribute('href');
    if (!url || url.startsWith('http') || url.startsWith('//')) {
        return;
    }

    // Same-page anchors (e.g. "Jump to end"): content scrolls inside .content-area,
    // not the document, so native hash navigation resets the overflow:hidden root to
    // the top instead of scrolling the pane. Scroll the container to the target.
    if (url.startsWith('#')) {
        const target = document.getElementById(url.slice(1));
        const scroller = document.querySelector('.content-area');
        if (target && scroller) {
            e.preventDefault();
            const top = scroller.scrollTop + target.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
            scroller.scrollTo({ top, behavior: 'smooth' });
        }
        return;
    }

    // Intercept all internal navigation links (root, file views, timeline)
    if (url === '/' || url.startsWith('/view/') || url.startsWith('/timeline') || url.startsWith('/transcript') || url.startsWith('/standup') || url.startsWith('/today') || url.startsWith('/memory')) {
        e.preventDefault();
        navigate(url);
    }
}

// Handle browser back/forward buttons
window.addEventListener('popstate', function(e) {
    if (e.state && e.state.url) {
        navigate(e.state.url, false);
    } else {
        // Fallback to current location (preserve query string)
        navigate(window.location.pathname + window.location.search, false);
    }
});

// Initialize on page load
document.addEventListener('DOMContentLoaded', function() {
    console.log('[SPA] Initializing...');

    // Restore standup large-text preference (body persists across SPA swaps)
    try {
        if (localStorage.getItem('peekm_standup_lg') === '1') document.body.classList.add('standup-lg');
    } catch (e) {}

    // Setup persistent SSE connection
    connectSSE();

    // Setup link interception
    document.body.addEventListener('click', interceptLinks);

    // Initialize current page scripts
    reinitializeScripts();

    // Restore tree state on initial page load
    restoreTreeState();

    // Add initial history state (preserve query string for transcript, timeline filters)
    var initialURL = window.location.pathname + window.location.search;
    history.replaceState({ url: initialURL }, '', initialURL);

    // Sidebar sort preference: the cookie normally lets the server ship the
    // right sidebar; re-sync it and refetch only if this payload predates the
    // cookie (the .tree-recent wrapper marks an already-Recent sidebar).
    updateTreeSortButton();
    if (treeSortMode() === 'recent') {
        setTreeSortCookie('recent');
        if (!document.querySelector('.sidebar-tree .tree-recent')) refreshTree();
    }

    console.log('[SPA] Initialization complete');
});

// Save state and cleanup on page unload
window.addEventListener('beforeunload', function() {
    saveTreeState();
    if (eventSource) {
        eventSource.close();
    }
});

// ===== Helper Functions (used by SSE handlers and tree operations) =====

// Escape HTML to prevent XSS
function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

// ===== Toast Notification Functions =====

let toastTimeout;
let toastFilePath = null;

// Batch notification state
let batchTimer = null;
let batchedFiles = new Map(); // Map<filePath, {message, timestamp}>

// Toast configuration constants
const TOAST_CONFIG = {
    BATCH_WINDOW: 800,        // ms to wait for batch
    MAX_BATCH_SIZE: 20,       // safety valve
    SINGLE_DURATION: 5000,    // ms for single file
    BATCH_DURATION: 6000,     // ms for batches
    TRANSITION_TIME: 300      // CSS transition duration
};

function fileModifiedToast(data) {
    const msg = data.planTitle
        ? `Plan updated: ${data.planTitle}`
        : `File updated: ${data.path}`;
    showToast(msg, data.path, data.session);
}

function showToast(message, filePath, session) {
    // Create file info object
    const fileInfo = {
        name: filePath ? filePath.split('/').pop() : null,
        path: filePath,
        message: message,
        session: session || null,
        timestamp: Date.now()
    };

    // Add to batch (deduplicate by file path)
    if (filePath) {
        batchedFiles.set(filePath, fileInfo);
    } else {
        // Non-file notifications (rare) - add with unique key
        batchedFiles.set(`non-file-${Date.now()}`, fileInfo);
    }

    // Clear existing timer
    if (batchTimer) {
        clearTimeout(batchTimer);
    }

    // Safety valve: show immediately if batch gets too large
    if (batchedFiles.size >= TOAST_CONFIG.MAX_BATCH_SIZE) {
        displayBatchedToast();
        return;
    }

    // Start/restart batch timer
    batchTimer = setTimeout(() => {
        displayBatchedToast();
    }, TOAST_CONFIG.BATCH_WINDOW);
}

// Format batch message based on file count (pure function)
function formatBatchMessage(files) {
    const count = files.length;

    if (count === 1) {
        // Single file - show full message with session if available
        const file = files[0];
        const primary = file.session ? `${file.message} (Session: ${file.session})` : file.message;
        return {
            primary: primary,
            secondary: null,
            icon: '<svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor"><path d="M2 1.75C2 .784 2.784 0 3.75 0h6.586c.464 0 .909.184 1.237.513l2.914 2.914c.329.328.513.773.513 1.237v9.586A1.75 1.75 0 0 1 13.25 16h-9.5A1.75 1.75 0 0 1 2 14.25Zm1.75-.25a.25.25 0 0 0-.25.25v12.5c0 .138.112.25.25.25h9.5a.25.25 0 0 0 .25-.25V6h-2.75A1.75 1.75 0 0 1 9 4.25V1.5Zm6.75.062V4.25c0 .138.112.25.25.25h2.688l-.011-.013-2.914-2.914-.013-.011Z"/></svg>',
            href: file.path ? `/view/${encodeURIComponent(file.path)}` : '#',
            clickAction: null
        };
    }

    // Batch formatting
    const names = files.map(f => f.name);
    const icon = '<svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor"><path d="M2 4.25A2.25 2.25 0 0 1 4.25 2h.5a.75.75 0 0 1 0 1.5h-.5a.75.75 0 0 0-.75.75v.5a.75.75 0 0 1-1.5 0Zm9 0A2.25 2.25 0 0 0 8.75 2h-.5a.75.75 0 0 0 0 1.5h.5a.75.75 0 0 1 .75.75v.5a.75.75 0 0 0 1.5 0ZM2 8.75A2.25 2.25 0 0 0 4.25 11h.5a.75.75 0 0 0 0-1.5h-.5a.75.75 0 0 1-.75-.75v-.5a.75.75 0 0 0-1.5 0Zm9 0A2.25 2.25 0 0 1 8.75 11h-.5a.75.75 0 0 1 0-1.5h.5a.75.75 0 0 0 .75-.75v-.5a.75.75 0 0 1 1.5 0ZM5.75 6a.75.75 0 0 0-.75.75v3.5c0 .414.336.75.75.75h4.5a.75.75 0 0 0 .75-.75v-3.5a.75.75 0 0 0-.75-.75Z"/></svg>';
    const href = '#';
    const clickAction = function(e) {
        if (e.target.classList.contains('toast-close')) return;
        e.preventDefault();
        navigate('/timeline');
        hideToast();
    };

    if (count === 2) {
        // Two files - show both names
        return {
            primary: `${count} files updated`,
            secondary: names.join(', '),
            icon,
            href,
            clickAction
        };
    }

    // 3+ files - show preview of first 2
    const preview = names.slice(0, 2).join(', ');
    return {
        primary: `${count} files updated`,
        secondary: `${preview}, and ${count - 2} more`,
        icon,
        href,
        clickAction
    };
}

// Update toast DOM elements with configuration
function updateToastDOM(config) {
    const elements = {
        toast: document.getElementById('toast'),
        message: document.getElementById('toast-message'),
        detail: document.getElementById('toast-detail'),
        icon: document.getElementById('toast-icon'),
        badge: document.getElementById('toast-badge')
    };

    // Early return if critical elements missing
    if (!elements.toast || !elements.message) {
        console.error('[Toast] Required DOM elements missing');
        return null;
    }

    // Update content
    elements.message.textContent = config.primary;

    // Set secondary text
    if (elements.detail) {
        elements.detail.textContent = config.secondary || '';
        elements.detail.style.display = config.secondary ? 'block' : 'none';
    }

    // Set icon
    if (elements.icon) {
        elements.icon.innerHTML = config.icon;
    }

    // Set badge for batches
    if (elements.badge) {
        const showBadge = config.count > 1;
        elements.badge.textContent = showBadge ? config.count : '';
        elements.badge.style.display = showBadge ? 'inline-block' : 'none';
        elements.toast.classList.toggle('batch', showBadge);
    }

    // Set navigation
    elements.toast.href = config.href;
    elements.toast.onclick = config.clickAction;

    return elements.toast;
}

// Display batched toast notification (orchestration)
function displayBatchedToast() {
    if (batchedFiles.size === 0) return;

    const files = Array.from(batchedFiles.values());
    const config = formatBatchMessage(files);
    config.count = files.length;

    const toast = updateToastDOM(config);
    if (!toast) return; // Error logged in helper

    // Store single file path for navigation
    toastFilePath = files.length === 1 ? files[0].path : null;

    // Show toast - remove any inline display style and use class
    toast.style.display = '';
    // Small delay to ensure DOM updates properly
    requestAnimationFrame(() => {
        toast.classList.add('show');
    });

    // Clear existing timeout
    if (toastTimeout) {
        clearTimeout(toastTimeout);
    }

    // Auto-hide after duration
    const duration = files.length > 1 ? TOAST_CONFIG.BATCH_DURATION : TOAST_CONFIG.SINGLE_DURATION;
    toastTimeout = setTimeout(hideToast, duration);

    // Clear batch state
    batchedFiles.clear();
    batchTimer = null;
}

function hideToast() {
    const toast = document.getElementById('toast');
    if (!toast) return;

    toast.classList.remove('show');
    toastFilePath = null;

    // Let CSS handle visibility through opacity and pointer-events
    // No need to set display:none as CSS handles it through pointer-events: none
}

// ===== Connection Status Functions =====

function updateConnectionStatus(count) {
    const dot = document.getElementById('connection-dot');
    const countEl = document.getElementById('connection-count');
    const statusEl = document.getElementById('connection-status');

    if (countEl) {
        countEl.textContent = count;
    }

    if (dot) {
        if (count > 0) {
            dot.classList.add('connected');
            dot.classList.remove('reconnecting');
        } else {
            dot.classList.remove('connected');
        }
    }

    if (statusEl) {
        const hasAI = !!document.querySelector('.connection-ai-label');
        const connectedTitle = hasAI ? 'Live reload + AI tracking active' : 'Live reload active';
        statusEl.title = count > 0 ? connectedTitle : 'Disconnected — will retry';
        statusEl.classList.toggle('disconnected', count === 0);
    }
}

// ===== Dynamic Tree Manipulation =====

// Optimistically drops a removed file's row; scheduleTreeRefresh() reconciles.
// Matches by href, not basename: names like README.md repeat across folders.
function removeFileFromTree(filePath) {
    const hrefs = [`/view/${filePath}`, `/view/${encodeURIComponent(filePath)}`];
    for (const link of document.querySelectorAll('.sidebar-tree .tree-file a')) {
        if (hrefs.includes(link.getAttribute('href'))) {
            // closest(), not the outer .tree-item: a directory's item contains its files.
            link.closest('.tree-item').remove();
            return;
        }
    }
}

// Expand all parent directories for a given file path
function expandParentDirectories(filePath) {
    if (!filePath) return false;

    // Decode URL encoding (handles spaces, unicode, etc.)
    const decoded = decodeURIComponent(filePath);

    // Parse parent paths: "a/b/c/file.md" → ["a", "a/b", "a/b/c"]
    const segments = decoded.split('/');
    if (segments.length <= 1) {
        // Root-level file, no parents to expand
        return true;
    }

    const parentPaths = [];
    for (let i = 1; i < segments.length; i++) {
        parentPaths.push(segments.slice(0, i).join('/'));
    }

    let allFound = true;
    parentPaths.forEach(path => {
        const selector = `.tree-directory[data-path="${CSS.escape(path)}"]`;
        const dir = document.querySelector(selector);

        if (!dir) {
            console.warn(`[expandParents] Parent directory not found: ${path}`);
            allFound = false;
            return;
        }

        // Only expand if currently collapsed
        if (dir.dataset.collapsed === 'true') {
            toggleDir(dir);
        }
    });

    console.log(`[expandParents] Expanded ${parentPaths.length} parent directories for: ${decoded}`);
    return allFound;
}

// Refresh tree from server (self-healing mechanism)
// Sidebar sort mode: 'name' (the tree) or 'recent' (flat, newest edit first).
// The cookie mirrors localStorage so the server ships the right sidebar on
// full-page loads instead of an A–Z tree the client would discard.
function treeSortMode() {
    try { return localStorage.getItem('peekm_tree_sort') === 'recent' ? 'recent' : 'name'; } catch (e) { return 'name'; }
}

function setTreeSortCookie(mode) {
    document.cookie = 'peekm_tree_sort=' + mode + '; path=/; max-age=31536000; SameSite=Lax';
}

// Whether Recent mode shows files beyond the server's day window; reset on
// toggle so re-entering the mode starts windowed again.
var treeShowAll = false;

function toggleTreeSort() {
    var next = treeSortMode() === 'recent' ? 'name' : 'recent';
    try { localStorage.setItem('peekm_tree_sort', next); } catch (e) {}
    setTreeSortCookie(next);
    treeShowAll = false;
    updateTreeSortButton();
    refreshTree();
}

function updateTreeSortButton() {
    var btn = document.getElementById('tree-sort-toggle');
    if (btn) btn.classList.toggle('active', treeSortMode() === 'recent');
}

// "+N older files" expander in Recent mode — the windowed payload only counts
// older files, so expanding refetches unwindowed. The flag lives here (not in
// the button) so the expansion survives SSE-driven refreshes.
function showOlderFiles(btn) {
    btn.disabled = true;
    treeShowAll = true;
    refreshTree();
}

async function refreshTree() {
    try {
        // The memory view owns its sidebar tree — never clobber it with the
        // workspace tree. Guarded here so every caller is safe by default.
        const view = document.getElementById('content');
        if (view && view.dataset.view === 'memory') return;

        const fileTree = document.querySelector('.sidebar-tree');
        if (!fileTree) {
            console.log('[refreshTree] No sidebar-tree element found, skipping');
            return;
        }

        // 1. Capture scroll position
        const sidebarContent = document.querySelector('.sidebar-content');
        const scrollPos = sidebarContent ? sidebarContent.scrollTop : 0;

        console.log('[refreshTree] Refreshing tree, scroll pos:', scrollPos);

        // 2. Fetch fresh tree HTML from server
        const recent = treeSortMode() === 'recent';
        const url = recent ? '/tree-html?sort=recent' + (treeShowAll ? '&all=1' : '') : '/tree-html';
        const response = await fetch(url, {
            headers: {
                'Cache-Control': 'no-cache'
            }
        });

        if (!response.ok) {
            console.error('[refreshTree] Server returned', response.status);
            return;
        }

        const html = await response.text();

        // 3. Replace tree DOM
        fileTree.innerHTML = html;

        // 4. Restore expanded state from localStorage (tree mode only — Recent
        // mode has no directories to restore)
        if (!recent) {
            restoreTreeState();
        }

        // 5. Restore scroll position
        if (sidebarContent) {
            sidebarContent.scrollTop = scrollPos;
        }

        console.log('[refreshTree] Tree refreshed successfully');
    } catch (error) {
        console.error('[refreshTree] Error:', error);
        // Don't crash - graceful degradation
    }
}

// Schedule tree refresh with debouncing (batches rapid updates)
function scheduleTreeRefresh() {
    // Clear any pending refresh
    if (refreshTreeTimer) {
        clearTimeout(refreshTreeTimer);
    }

    // Schedule new refresh after 800ms of inactivity
    refreshTreeTimer = setTimeout(() => {
        refreshTree();
        refreshTreeTimer = null;
    }, 800);

    console.log('[scheduleTreeRefresh] Tree refresh scheduled');
}

// Download HTML functionality
function downloadHTML() {
    const filePath = getCurrentFilePath();
    if (!filePath) {
        showErrorToast('No file currently open');
        return;
    }

    sendJSON('/download', { path: filePath })
    .then(response => {
        // Get filename from Content-Disposition header
        const contentDisposition = response.headers.get('Content-Disposition');
        let filename = 'download.html';
        if (contentDisposition) {
            const match = contentDisposition.match(/filename="?(.+)"?/);
            if (match) {
                filename = match[1].replace(/"/g, '');
            }
        }
        return response.blob().then(blob => ({ blob, filename }));
    })
    .then(({ blob, filename }) => {
        const url = window.URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = filename;
        document.body.appendChild(a);
        a.click();
        window.URL.revokeObjectURL(url);
        document.body.removeChild(a);
    })
    .catch(error => {
        console.error('Download error:', error);
        showErrorToast('Failed to download HTML file');
    });
}

// ===== LAN Share =====
// getCurrentFilePath() is defined in editor.js (handles both /view/ routes and data-file attribute)

function setSharePanelOpen(open) {
    var panel = document.getElementById('share-panel');
    var btn = document.getElementById('share-btn');
    if (panel) panel.style.display = open ? 'block' : 'none';
    if (btn) btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}

function updateSharePanel(data) {
    var panel = document.getElementById('share-panel');
    panel.dataset.token = data.token;
    document.getElementById('share-lan-url').value = data.url;
    var publicSection = document.getElementById('share-public-section');
    var makePublicBtn = document.getElementById('share-make-public-btn');
    if (data.public_url) {
        document.getElementById('share-public-url').value = data.public_url;
        publicSection.style.display = 'block';
        makePublicBtn.style.display = 'none';
    } else {
        publicSection.style.display = 'none';
        makePublicBtn.style.display = 'block';
        makePublicBtn.disabled = false;
        makePublicBtn.textContent = 'Make public';
    }
    var remaining = new Date(data.expires_at) - new Date();
    var mins = Math.ceil(remaining / 60000);
    var expiryEl = document.getElementById('share-expiry');
    if (mins <= 0) {
        expiryEl.textContent = 'Expired';
    } else if (mins > 60) {
        expiryEl.textContent = Math.floor(mins/60) + 'h ' + (mins%60) + 'm remaining';
    } else {
        expiryEl.textContent = mins + 'm remaining';
    }
}

async function toggleShare() {
    var shareBtn = document.getElementById('share-btn');
    if (shareBtn.classList.contains('share-active')) {
        var panel = document.getElementById('share-panel');
        setSharePanelOpen(panel && panel.style.display === 'none');
        return;
    }
    var filePath = getCurrentFilePath();
    if (!filePath) {
        showErrorToast('No file currently open');
        return;
    }
    shareBtn.disabled = true;
    try {
        var data = await (await sendJSON('/share', { path: filePath })).json();
        updateSharePanel(data);
        shareBtn.classList.add('share-active');
        setSharePanelOpen(true);
        copyText(data.url, function () { showToast('LAN share link copied'); });
    } catch (err) {
        showErrorToast('Failed to create share: ' + err.message);
    } finally {
        shareBtn.disabled = false;
    }
}

async function makeSharePublic() {
    var btn = document.getElementById('share-make-public-btn');
    var token = document.getElementById('share-panel').dataset.token;
    btn.disabled = true;
    btn.textContent = 'Connecting...';
    try {
        var data = await (await sendJSON('/share/public', { token: token })).json();
        updateSharePanel(data);
        copyText(data.public_url, function () { showToast('Public link copied'); });
    } catch (err) {
        btn.disabled = false;
        btn.textContent = 'Make public';
        showErrorToast('Failed to make public: ' + err.message);
    }
}

async function stopSharing() {
    var shareBtn = document.getElementById('share-btn');
    var token = document.getElementById('share-panel').dataset.token;
    try {
        await sendJSON('/share', { token: token }, 'DELETE');
        shareBtn.classList.remove('share-active');
        setSharePanelOpen(false);
        showToast('Share revoked');
    } catch (err) {
        showErrorToast('Failed to revoke share');
    }
}

function copyShareURL(inputId) {
    var url = document.getElementById(inputId).value;
    copyText(url, function () { showToast('URL copied'); });
}

async function submitReply() {
    var box = document.querySelector('.transcript-reply');
    var input = document.getElementById('reply-input');
    var btn = document.getElementById('reply-send');
    if (!box || !input || !btn) return;
    var session = box.dataset.session;
    var text = input.value.trim();
    if (!text) { input.focus(); return; }

    btn.disabled = true;
    input.disabled = true;
    btn.textContent = 'Sending…';
    try {
        var data = await (await sendJSON('/transcript/reply', { session: session, text: text })).json();
        input.value = '';
        showToast('Reply sent');
        // The reply forks/continues a peekm-owned branch whose new turns land in a
        // different transcript file, so switch the view to that branch id — otherwise
        // the original session re-renders unchanged and the response never appears.
        var branch = data && data.branch ? data.branch : session;
        await navigate(window.location.pathname + '?session=' + encodeURIComponent(branch), true);
    } catch (err) {
        btn.disabled = false;
        input.disabled = false;
        btn.textContent = 'Send';
        showErrorToast('Reply failed: ' + (err.message || err));
    }
}

function initReplyBox() {
    var input = document.getElementById('reply-input');
    if (!input || input.dataset.bound) return;
    input.dataset.bound = '1';
    input.addEventListener('keydown', function(e) {
        if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
            e.preventDefault();
            submitReply();
        }
    });
}

// Live transcript: refresh the open transcript when SSE reports activity on
// its session, preserving reply draft, expanded toggles, and scroll position.
let transcriptRefreshTimer = null;
function scheduleTranscriptRefresh(session) {
    const content = document.getElementById('content');
    if (!content || content.dataset.view !== 'transcript') return;
    const current = new URLSearchParams(window.location.search).get('session');
    if (!session || session !== current) return;
    clearTimeout(transcriptRefreshTimer);
    transcriptRefreshTimer = setTimeout(refreshTranscript, 2000);
}

async function refreshTranscript() {
    const content = document.getElementById('content');
    if (!content || content.dataset.view !== 'transcript') return;

    const input = document.getElementById('reply-input');
    const draft = input ? input.value : '';
    const hadFocus = input && document.activeElement === input;
    const opened = [];
    content.querySelectorAll('.transcript-longtext-toggle > input').forEach(function(cb, i) {
        if (cb.checked) opened.push(i);
    });
    const openDetails = [];
    content.querySelectorAll('details').forEach(function(d, i) {
        if (d.open) openDetails.push(i);
    });
    // #content is the scroll container (body is overflow:hidden), and navigate()
    // replaces it wholesale — the fresh element starts at scrollTop 0, so scroll
    // must be captured here and re-applied to the new element.
    const nearBottom = content.scrollTop + content.clientHeight >= content.scrollHeight - 120;
    const scrollTop = content.scrollTop;

    // Anchor to the topmost visible turn, not a pixel offset: new events append
    // below and collapse states can change above, so an absolute scrollTop would
    // land on different content. Turn indices are stable (appends only).
    let anchorIndex = -1, anchorTop = 0;
    if (!nearBottom) {
        const turns = content.querySelectorAll('.transcript-turn');
        for (let i = 0; i < turns.length; i++) {
            const r = turns[i].getBoundingClientRect();
            if (r.bottom > 0) { anchorIndex = i; anchorTop = r.top; break; }
        }
    }

    await navigate(window.location.pathname + window.location.search, false);

    const fresh = document.getElementById('content');
    if (!fresh) return;
    const toggles = fresh.querySelectorAll('.transcript-longtext-toggle > input');
    opened.forEach(function(i) { if (toggles[i]) toggles[i].checked = true; });
    const freshDetails = fresh.querySelectorAll('details');
    openDetails.forEach(function(i) { if (freshDetails[i]) freshDetails[i].open = true; });
    const freshInput = document.getElementById('reply-input');
    if (freshInput && draft) freshInput.value = draft;
    if (freshInput && hadFocus) freshInput.focus();
    if (nearBottom) {
        fresh.scrollTop = fresh.scrollHeight;
        return;
    }
    const freshTurns = fresh.querySelectorAll('.transcript-turn');
    if (anchorIndex >= 0 && freshTurns[anchorIndex]) {
        fresh.scrollTop += freshTurns[anchorIndex].getBoundingClientRect().top - anchorTop;
    } else {
        fresh.scrollTop = scrollTop;
    }
}

// Live timeline: refresh on SSE session activity so active pulses, last-tool
// lines, and new events appear without a manual reload. Preserves expanded
// session cards and scroll position (filter is restored by reinitializeScripts).
let timelineRefreshTimer = null;
function scheduleTimelineRefresh() {
    const content = document.getElementById('content');
    if (!content || content.dataset.view !== 'timeline') return;
    clearTimeout(timelineRefreshTimer);
    timelineRefreshTimer = setTimeout(refreshTimeline, 2000);
}

async function refreshTimeline() {
    const content = document.getElementById('content');
    if (!content || content.dataset.view !== 'timeline') return;

    const expanded = new Set();
    content.querySelectorAll('.timeline-session-header[aria-expanded="true"] .timeline-session-id').forEach(function(id) {
        expanded.add(id.textContent.trim());
    });
    // #content is the scroll container and navigate() replaces it (fresh element
    // starts at scrollTop 0), so capture and re-apply its scrollTop.
    const scrollTop = content.scrollTop;

    await navigate(window.location.pathname + window.location.search, false);

    const fresh = document.getElementById('content');
    if (!fresh) return;
    fresh.querySelectorAll('.timeline-session-header').forEach(function(header) {
        const id = header.querySelector('.timeline-session-id');
        if (id && expanded.has(id.textContent.trim())) toggleExpander(header);
    });
    fresh.scrollTop = scrollTop;
}

async function checkShareStatus() {
    var filePath = getCurrentFilePath();
    if (!filePath) return;
    try {
        var resp = await fetch('/share?path=' + encodeURIComponent(filePath));
        var data = await resp.json();
        var shareBtn = document.getElementById('share-btn');
        if (!shareBtn) return;
        if (data.active) {
            shareBtn.classList.add('share-active');
            updateSharePanel(data);
        } else {
            shareBtn.classList.remove('share-active');
        }
    } catch (e) { /* ignore */ }
}

// Click-outside and Escape to dismiss share panel
document.addEventListener('click', function(e) {
    var container = document.querySelector('.share-container');
    var panel = document.getElementById('share-panel');
    if (container && panel && panel.style.display !== 'none' && !container.contains(e.target)) {
        setSharePanelOpen(false);
    }
});
document.addEventListener('keydown', function(e) {
    if (e.key === 'Escape') {
        var panel = document.getElementById('share-panel');
        if (panel && panel.style.display !== 'none') {
            setSharePanelOpen(false);
        }
    }
});

// ===== Tree State Persistence =====

const TREE_STATE_KEY_PREFIX = 'peekm_tree_state_';

// Get directory-scoped storage key based on current browse directory
function getTreeStateKey() {
    const content = document.getElementById('content');
    const browseDir = content?.dataset.path || '';
    if (!browseDir) return null;

    // Use base64 encoding to handle special characters in paths
    try {
        return TREE_STATE_KEY_PREFIX + btoa(browseDir);
    } catch (e) {
        console.error('[TreeState] Failed to encode path:', e);
        return null;
    }
}

// Save tree expansion state and scroll position to localStorage
function saveTreeState() {
    try {
        // Recent mode renders no directories — saving would wipe the stored
        // collapse state of the real tree.
        if (treeSortMode() === 'recent') return;
        const storageKey = getTreeStateKey();
        if (!storageKey) return;

        const fileTree = document.querySelector('#sidebar-tree .tree');
        if (!fileTree) return;

        const expandedDirs = [];
        const directories = fileTree.querySelectorAll('.tree-directory');

        directories.forEach(dir => {
            const path = dir.dataset.path;
            if (!path) return;

            // Check actual visual state (not data attribute, which may not be set by server)
            const treeItem = dir.closest('.tree-item');
            const childrenContainer = treeItem?.querySelector('.tree-children');
            if (!childrenContainer) return;

            // Save only directories that are visually expanded
            const isCollapsed = childrenContainer.style.display === 'none';
            if (!isCollapsed) {
                expandedDirs.push(path);
            }
        });

        const state = {
            expandedDirs,
            scrollY: window.scrollY
        };

        localStorage.setItem(storageKey, JSON.stringify(state));
        console.log('[TreeState] Saved state for', storageKey, ':', state);
    } catch (error) {
        console.error('[TreeState] Failed to save:', error);
    }
}

// Restore tree expansion state and scroll position from localStorage
function restoreTreeState() {
    restoreSmartFolderState();
    try {
        const storageKey = getTreeStateKey();
        if (!storageKey) return;

        const stored = localStorage.getItem(storageKey);
        if (!stored) return;

        const state = JSON.parse(stored);
        const fileTree = document.querySelector('#sidebar-tree .tree');
        if (!fileTree) return;

        console.log('[TreeState] Restoring state for', storageKey, ':', state);

        // Restore expanded directories
        const directories = fileTree.querySelectorAll('.tree-directory');

        directories.forEach(dir => {
            // Use data-path attribute for unique identification
            const path = dir.dataset.path;

            const shouldBeExpanded = state.expandedDirs.includes(path);

            // Check actual visual state by looking at childrenContainer display
            const treeItem = dir.closest('.tree-item');
            const childrenContainer = treeItem?.querySelector('.tree-children');

            if (!childrenContainer) return; // No children, skip

            const isCurrentlyCollapsed = childrenContainer.style.display === 'none';

            // Toggle if current state doesn't match desired state
            if (shouldBeExpanded && isCurrentlyCollapsed) {
                // Should be expanded but is collapsed - expand it
                toggleDir(dir);
            } else if (!shouldBeExpanded && !isCurrentlyCollapsed) {
                // Should be collapsed but is expanded - collapse it
                toggleDir(dir);
            }
        });

        // Restore scroll position (after a small delay to ensure DOM is settled)
        if (state.scrollY !== undefined) {
            setTimeout(() => {
                window.scrollTo(0, state.scrollY);
                console.log('[TreeState] Restored scroll position:', state.scrollY);
            }, 50);
        }
    } catch (error) {
        console.error('[TreeState] Failed to restore:', error);
    }
}

// ===== Smart Folder State Persistence =====

const SMART_FOLDER_STATE_KEY = 'peekm_smart_folder_state';

function setSmartFolderCollapsed(folder, collapsed) {
    const header = folder.querySelector('.smart-folder-header');
    const children = folder.querySelector('.smart-folder-children');
    const icon = header?.querySelector('.expand-icon');
    if (!children || !icon) return;
    children.style.display = collapsed ? 'none' : 'block';
    icon.textContent = collapsed ? '▶' : '▼';
    header.dataset.collapsed = String(collapsed);
}

function toggleSmartFolder(header) {
    setSmartFolderCollapsed(header.closest('.smart-folder'), header.dataset.collapsed !== 'true');
    saveSmartFolderState();
}

function saveSmartFolderState() {
    try {
        const folders = document.querySelectorAll('.smart-folder-header');
        const state = {};
        folders.forEach(header => {
            const folder = header.closest('.smart-folder');
            const id = folder?.dataset.folder;
            if (id) {
                state[id] = header.dataset.collapsed === 'true';
            }
        });
        localStorage.setItem(SMART_FOLDER_STATE_KEY, JSON.stringify(state));
    } catch (e) {
        console.error('[SmartFolders] Failed to save state:', e);
    }
}

function restoreSmartFolderState() {
    try {
        const stored = localStorage.getItem(SMART_FOLDER_STATE_KEY);
        if (!stored) return;
        const state = JSON.parse(stored);
        document.querySelectorAll('.smart-folder').forEach(folder => {
            if (state[folder.dataset.folder]) setSmartFolderCollapsed(folder, true);
        });
    } catch (e) {
        console.error('[SmartFolders] Failed to restore state:', e);
    }
}

// ===== Focus Mode: Toggleable Sidebar Functions =====

const SIDEBAR_STORAGE_KEY = 'peekm_sidebar_state';

// Matches the CSS @media (max-width: 768px) breakpoint where the
// sidebar becomes a fixed-position overlay instead of persistent nav.
function isMobileViewport() {
    return window.matchMedia('(max-width: 768px)').matches;
}

// Single source of truth for sidebar state changes. Writes the dataset
// attribute and keeps the hamburger button label/tooltip in sync. Skips
// the work when the state hasn't changed. Does NOT persist — callers
// decide whether to write to localStorage.
function setSidebarState(state) {
    const container = document.querySelector('.layout-container');
    if (!container || container.dataset.sidebar === state) return;
    container.dataset.sidebar = state;

    const toggleBtn = document.getElementById('sidebar-toggle');
    if (toggleBtn) {
        const expanded = state === 'expanded';
        toggleBtn.title = expanded
            ? 'Hide navigation (Cmd/Ctrl+B)'
            : 'Show navigation (Cmd/Ctrl+B)';
        toggleBtn.setAttribute('aria-label',
            expanded ? 'Hide navigation sidebar' : 'Show navigation sidebar');
    }
}

function readSavedSidebarState() {
    try {
        return localStorage.getItem(SIDEBAR_STORAGE_KEY) === 'collapsed'
            ? 'collapsed'
            : 'expanded';
    } catch (error) {
        console.error('[Sidebar] Failed to load state:', error);
        return 'expanded';
    }
}

// Auto-dismiss the overlay after navigation on mobile so the tapped
// content becomes visible (standard mobile drawer pattern).
function collapseSidebarOnMobile() {
    if (isMobileViewport()) {
        setSidebarState('collapsed');
    }
}

function toggleSidebar() {
    const container = document.querySelector('.layout-container');
    if (!container) return;

    const newState = container.dataset.sidebar === 'expanded' ? 'collapsed' : 'expanded';
    setSidebarState(newState);

    // Persist desktop preference only — mobile sidebar is an ephemeral
    // overlay, so toggles there shouldn't overwrite the saved state.
    if (!isMobileViewport()) {
        try {
            localStorage.setItem(SIDEBAR_STORAGE_KEY, newState);
        } catch (error) {
            console.error('[Sidebar] Failed to save state:', error);
        }
    }

    console.log('[Sidebar] Toggled to:', newState);
}

// Initialize sidebar state from localStorage
function initializeSidebar() {
    const content = document.getElementById('content');
    if (!content) return;

    const viewType = content.dataset.view;

    // Restore saved state or default to expanded (Persistent Navigation)
    const container = document.querySelector('.layout-container');
    if (!container) return;

    // On mobile, always start collapsed regardless of saved state —
    // the sidebar is a temporary overlay there, not persistent nav.
    setSidebarState(isMobileViewport() ? 'collapsed' : readSavedSidebarState());

    // Memory mode accent: driven by sessionStorage so it persists across file clicks
    const inMemoryMode = viewType === 'memory' || sessionStorage.getItem('peekm_memory_mode') === 'true';
    const sidebar = document.querySelector('.file-sidebar');
    if (sidebar) sidebar.classList.toggle('memory-mode', inMemoryMode);

    if (viewType === 'memory' || (viewType === 'file' && inMemoryMode)) {
        const breadcrumb = document.getElementById('breadcrumb');
        if (breadcrumb) {
            breadcrumb.innerHTML = '<a href="/memory" style="font-weight: 600; font-size: 12px; text-transform: uppercase; letter-spacing: 0.5px; color: var(--fgColor-done); text-decoration: none; opacity: 1;">\u2190 Memory</a>';
        }
    }
    if (viewType === 'file' && !inMemoryMode) {
        updateBreadcrumb();
    }
    if (viewType === 'file') {
        highlightCurrentFile();
    }
}

// Note: syncSidebarContent() removed in unified layout
// Tree is now rendered directly in sidebar by server template
// and persists during SPA navigation

// Generate and update breadcrumb trail
function updateBreadcrumb() {
    const breadcrumb = document.getElementById('breadcrumb');
    const content = document.getElementById('content');

    if (!breadcrumb || !content) return;

    const browsePath = content.dataset.path || '';
    const viewType = content.dataset.view;

    if (viewType !== 'file' || !browsePath) {
        breadcrumb.innerHTML = '';
        return;
    }

    // Parse path and generate breadcrumb
    const homeDir = browsePath.split('/').slice(0, 3).join('/'); // /Users/username
    let relativePath = browsePath.replace(homeDir, '~');

    // Split into segments
    const segments = relativePath.split('/').filter(s => s.length > 0);

    let breadcrumbHTML = '<a href="/">~</a>';
    let currentPath = homeDir;

    for (let i = 1; i < segments.length - 1; i++) {
        const segment = segments[i];
        currentPath += '/' + segment;
        breadcrumbHTML += ` / <span>${escapeHtml(segment)}</span>`;
    }

    // Add current file (not clickable)
    if (segments.length > 0) {
        const fileName = segments[segments.length - 1];
        breadcrumbHTML += ` / <span>${escapeHtml(fileName)}</span>`;
    }

    breadcrumb.innerHTML = breadcrumbHTML;

    console.log('[Sidebar] Breadcrumb updated');
}

// Highlight current file in sidebar tree
function highlightCurrentFile() {
    const content = document.getElementById('content');
    if (!content || content.dataset.view !== 'file') return;

    // Remove existing highlights
    const sidebarTree = document.getElementById('sidebar-tree');
    if (!sidebarTree) return;

    const allLinks = sidebarTree.querySelectorAll('.tree-file a');
    allLinks.forEach(link => link.classList.remove('current'));

    // Get current file path from URL
    const currentPath = decodeURIComponent(window.location.pathname.replace('/view/', ''));

    // Auto-expand parent directories before highlighting
    expandParentDirectories(currentPath);

    // Find and highlight matching link
    for (let link of allLinks) {
        const href = link.getAttribute('href');
        if (href === `/view/${encodeURIComponent(currentPath)}` || href === `/view/${currentPath}`) {
            link.classList.add('current');

            // Scroll to highlighted item (with slight delay for transition)
            setTimeout(() => {
                link.scrollIntoView({ behavior: 'smooth', block: 'center' });
            }, 250);

            console.log('[Sidebar] Highlighted current file');
            break;
        }
    }
}

// Keyboard shortcut: Cmd/Ctrl+B toggles sidebar
document.addEventListener('keydown', function(e) {
    // Cmd+B (Mac) or Ctrl+B (Windows/Linux)
    if ((e.metaKey || e.ctrlKey) && e.key === 'b') {
        const toggleBtn = document.getElementById('sidebar-toggle');
        if (toggleBtn && toggleBtn.style.display !== 'none') {
            e.preventDefault();
            toggleSidebar();
        }
    }
});

// ===== File Search Functions =====

let searchResults = [];
let selectedIndex = -1;

// Get all files from sidebar tree
function getAllFiles() {
    const sidebarTree = document.getElementById('sidebar-tree');
    if (!sidebarTree) return [];

    const files = [];
    const allItems = sidebarTree.querySelectorAll('.tree-item .tree-file a');

    allItems.forEach(link => {
        const fileName = link.textContent.trim();
        const filePath = link.getAttribute('href')?.replace('/view/', '') || '';

        if (fileName && filePath) {
            files.push({
                name: fileName,
                path: decodeURIComponent(filePath),
                url: link.getAttribute('href')
            });
        }
    });

    return files;
}

// Fuzzy match score: returns score (higher is better), -1 if no match
function fuzzyMatchScore(str, query) {
    str = str.toLowerCase();
    query = query.toLowerCase();

    // Exact match gets highest score
    if (str === query) return 1000;

    // Starts with query gets very high score
    if (str.startsWith(query)) return 900;

    // Contains query as substring gets high score
    if (str.includes(query)) return 800;

    // Fuzzy match: all query chars must appear in order
    let strIndex = 0;
    let queryIndex = 0;
    let score = 0;
    let consecutiveMatches = 0;

    while (strIndex < str.length && queryIndex < query.length) {
        if (str[strIndex] === query[queryIndex]) {
            // Bonus for consecutive character matches
            consecutiveMatches++;
            score += 10 + (consecutiveMatches * 5);
            queryIndex++;
        } else {
            consecutiveMatches = 0;
            score -= 1; // Penalty for gaps
        }
        strIndex++;
    }

    // All query characters must match
    if (queryIndex !== query.length) return -1;

    // Bonus for shorter strings (more precise matches)
    score += Math.max(0, 100 - str.length);

    return score;
}

// Search files and show dropdown
function searchFiles(query) {
    const dropdown = document.getElementById('search-dropdown');
    const resultsContainer = document.getElementById('search-results');
    const clearBtn = document.getElementById('search-clear');

    // Show/hide clear button
    if (clearBtn) {
        clearBtn.style.display = query.length > 0 ? 'flex' : 'none';
    }

    if (!query || query.trim() === '') {
        // No search - hide dropdown
        if (dropdown) dropdown.style.display = 'none';
        searchResults = [];
        selectedIndex = -1;
        return;
    }

    const searchQuery = query.trim();
    const allFiles = getAllFiles();

    // Fuzzy match and score files
    const scoredFiles = allFiles
        .map(file => ({
            ...file,
            score: fuzzyMatchScore(file.name, searchQuery)
        }))
        .filter(file => file.score > 0)
        .sort((a, b) => b.score - a.score); // Sort by score descending

    searchResults = scoredFiles;

    // Show dropdown with results
    if (dropdown && resultsContainer) {
        if (searchResults.length === 0) {
            resultsContainer.innerHTML = '<div class="search-no-results">No files found</div>';
            dropdown.style.display = 'block';
        } else {
            resultsContainer.innerHTML = searchResults.map((file, index) =>
                `<div class="search-result-item" data-index="${index}">
                    <div class="search-result-name">${escapeHtml(file.name)}</div>
                    <div class="search-result-path">${escapeHtml(file.path)}</div>
                </div>`
            ).join('');
            dropdown.style.display = 'block';
            selectedIndex = -1;

            // Add click handlers to results
            const items = resultsContainer.querySelectorAll('.search-result-item');
            items.forEach((item, index) => {
                item.addEventListener('click', () => {
                    navigateToFile(searchResults[index].url);
                });
            });
        }
    }

    console.log(`[Search] Found ${searchResults.length} matches for "${query}"`);
}

// Navigate to selected file
function navigateToFile(url) {
    const searchInput = document.getElementById('file-search');
    const dropdown = document.getElementById('search-dropdown');

    // Hide dropdown
    if (dropdown) dropdown.style.display = 'none';

    // Clear search
    if (searchInput) {
        searchInput.value = '';
        searchInput.blur();
    }

    const clearBtn = document.getElementById('search-clear');
    if (clearBtn) clearBtn.style.display = 'none';

    searchResults = [];
    selectedIndex = -1;

    // Navigate using SPA
    if (url && typeof navigate === 'function') {
        navigate(url);
    }
}

// Update selected item in dropdown
function updateSelection() {
    const resultsContainer = document.getElementById('search-results');
    if (!resultsContainer) return;

    const items = resultsContainer.querySelectorAll('.search-result-item');

    items.forEach((item, index) => {
        if (index === selectedIndex) {
            item.classList.add('selected');
            // Scroll into view
            item.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
        } else {
            item.classList.remove('selected');
        }
    });
}

// Handle keyboard navigation in search
function handleSearchKeyboard(e) {
    const dropdown = document.getElementById('search-dropdown');

    // Only handle keys when dropdown is visible
    if (!dropdown || dropdown.style.display === 'none') {
        return;
    }

    if (e.key === 'ArrowDown') {
        e.preventDefault();
        selectedIndex = Math.min(selectedIndex + 1, searchResults.length - 1);
        updateSelection();
    } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        selectedIndex = Math.max(selectedIndex - 1, -1);
        updateSelection();
    } else if (e.key === 'Enter') {
        e.preventDefault();
        if (selectedIndex >= 0 && selectedIndex < searchResults.length) {
            navigateToFile(searchResults[selectedIndex].url);
        }
    } else if (e.key === 'Escape') {
        e.preventDefault();
        clearSearch();
    }
}

// Mobile search: reveal the search bar and focus input
var _mobileSearchBlurHandler = null;
function openMobileSearch() {
    var middle = document.querySelector('.top-bar-middle');
    var input = document.getElementById('file-search');
    if (!middle || !input) return;

    // Remove any stale listener
    if (_mobileSearchBlurHandler) {
        input.removeEventListener('blur', _mobileSearchBlurHandler);
    }

    middle.classList.add('mobile-expanded');
    input.focus();

    _mobileSearchBlurHandler = function() {
        setTimeout(function() {
            if (!input.value) {
                middle.classList.remove('mobile-expanded');
            }
            input.removeEventListener('blur', _mobileSearchBlurHandler);
            _mobileSearchBlurHandler = null;
        }, 200);
    };

    input.addEventListener('blur', _mobileSearchBlurHandler);
}

// Clear search and hide dropdown
function clearSearch() {
    const searchInput = document.getElementById('file-search');
    const clearBtn = document.getElementById('search-clear');
    const dropdown = document.getElementById('search-dropdown');

    if (searchInput) {
        searchInput.value = '';
        searchInput.focus();
    }

    if (clearBtn) {
        clearBtn.style.display = 'none';
    }

    if (dropdown) {
        dropdown.style.display = 'none';
    }

    searchResults = [];
    selectedIndex = -1;

    console.log('[Search] Cleared');
}

// Global keyboard shortcut: Cmd/Ctrl+P (VS Code style)
document.addEventListener('keydown', function(e) {
    if ((e.metaKey || e.ctrlKey) && !e.shiftKey && e.key === 'p') {
        e.preventDefault();
        const searchInput = document.getElementById('file-search');
        if (searchInput) {
            searchInput.focus();
            searchInput.select();
        }
    }
    if ((e.metaKey || e.ctrlKey) && e.shiftKey && e.key === 'f') {
        e.preventDefault();
        toggleFolderFilter();
    }
});

// Initialize search on page load
document.addEventListener('DOMContentLoaded', function() {
    const searchInput = document.getElementById('file-search');

    if (searchInput) {
        // Real-time search as user types
        searchInput.addEventListener('input', function(e) {
            searchFiles(e.target.value);
        });

        // Keyboard navigation
        searchInput.addEventListener('keydown', handleSearchKeyboard);

        console.log('[Search] Initialized');
    }

    // Close dropdown when clicking outside
    document.addEventListener('click', function(e) {
        const searchContainer = document.querySelector('.search-container');
        const dropdown = document.getElementById('search-dropdown');

        if (dropdown && searchContainer && !searchContainer.contains(e.target)) {
            dropdown.style.display = 'none';
        }
    });

    // Initialize folder filter (debounced)
    var folderInput = document.getElementById('folder-filter-input');
    if (folderInput) {
        _folderFilterEmptyEl = document.getElementById('folder-filter-empty');
        var filterTimer = null;
        folderInput.addEventListener('input', function(e) {
            clearTimeout(filterTimer);
            filterTimer = setTimeout(function() { filterTree(e.target.value); }, 150);
        });
        folderInput.addEventListener('keydown', function(e) {
            if (e.key === 'Escape') {
                clearTimeout(filterTimer);
                closeFolderFilter();
            }
        });
    }
});

// ===== Folder Filter Functions =====
var _folderFilterEmptyEl = null;

function getFolderFilterEls() {
    return {
        container: document.getElementById('folder-filter-container'),
        btn: document.getElementById('folder-filter-toggle'),
        input: document.getElementById('folder-filter-input')
    };
}

function toggleFolderFilter() {
    var els = getFolderFilterEls();
    if (!els.container) return;
    if (els.container.classList.contains('active')) {
        closeFolderFilter();
    } else {
        els.container.classList.add('active');
        if (els.btn) els.btn.classList.add('active');
        if (els.input) { els.input.value = ''; els.input.focus(); }
    }
}

function closeFolderFilter() {
    var els = getFolderFilterEls();
    if (els.container) els.container.classList.remove('active');
    if (els.btn) els.btn.classList.remove('active');
    if (els.input) els.input.value = '';
    clearTreeFilter();
}

function filterTree(query) {
    var tree = document.querySelector('#sidebar-tree .tree');
    if (!tree) return;

    query = (query || '').trim().toLowerCase();
    if (!query) { clearTreeFilter(); return; }

    // Single pass: collect visible items (matched dirs + their children + ancestors)
    var visible = new Set();
    tree.querySelectorAll('.tree-directory .dir-name').forEach(function(dirName) {
        if (dirName.closest('.smart-folder')) return;
        if (dirName.textContent.toLowerCase().indexOf(query) === -1) return;

        var treeItem = dirName.closest('.tree-item');
        if (!treeItem) return;
        visible.add(treeItem);
        highlightDirMatch(dirName, query);

        // Children
        treeItem.querySelectorAll('.tree-item').forEach(function(child) { visible.add(child); });

        // Expand matched dir
        var children = treeItem.querySelector('.tree-children');
        if (children) children.style.display = '';
        var chevron = treeItem.querySelector('.expand-icon');
        if (chevron) chevron.textContent = '\u25BC';

        // Ancestors (skip already-visited via Set)
        var parent = treeItem.parentElement;
        while (parent) {
            if (parent.classList && parent.classList.contains('tree-item')) {
                if (visible.has(parent)) break;
                visible.add(parent);
                var pc = parent.querySelector(':scope > .tree-children');
                if (pc) pc.style.display = '';
                var pv = parent.querySelector(':scope > .tree-node .expand-icon');
                if (pv) pv.textContent = '\u25BC';
            }
            parent = parent.parentElement;
        }
    });

    // Apply visibility only to file tree items (skip smart folders)
    tree.querySelectorAll('.tree-item').forEach(function(item) {
        if (!item.closest('.smart-folder')) {
            item.classList.toggle('filtered-out', !visible.has(item));
        }
    });

    if (_folderFilterEmptyEl) _folderFilterEmptyEl.classList.toggle('active', visible.size === 0);
}

function highlightDirMatch(dirNameEl, query) {
    var text = dirNameEl.textContent;
    var idx = text.toLowerCase().indexOf(query);
    if (idx === -1) return;
    dirNameEl.innerHTML =
        escapeHtml(text.substring(0, idx)) +
        '<span class="filter-match">' + escapeHtml(text.substring(idx, idx + query.length)) + '</span>' +
        escapeHtml(text.substring(idx + query.length));
}

function clearTreeFilter() {
    var tree = document.querySelector('#sidebar-tree .tree');
    if (!tree) return;
    tree.querySelectorAll('.tree-item.filtered-out').forEach(function(item) {
        item.classList.remove('filtered-out');
    });
    tree.querySelectorAll('.dir-name .filter-match').forEach(function(mark) {
        mark.replaceWith(mark.textContent);
    });
    if (_folderFilterEmptyEl) _folderFilterEmptyEl.classList.remove('active');
}

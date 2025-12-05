// SPA Navigation - maintains persistent SSE connection across page transitions
// This module handles client-side routing and content swapping

// Global state
let eventSource = null;
let reconnectAttempts = 0;
const maxReconnectDelay = 30000; // 30 seconds max

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
                showToast(`New file: ${data.path}`, data.path);
                // Dynamically insert file instead of reloading
                insertFileIntoTree(data.path);
            } else if (data.type === 'file_removed') {
                console.log('[SSE] Handling file_removed for:', data.path);
                showToast(`File removed: ${data.path}`, null);
                // Dynamically remove file from tree
                removeFileFromTree(data.path);
            } else if (data.type === 'file_modified') {
                console.log('[SSE] Handling file_modified for:', data.path);

                // Check if we're currently viewing this file
                const content = document.getElementById('content');
                const viewType = content ? content.dataset.view : null;

                if (viewType === 'file') {
                    // Extract current file path from URL (/view/{filepath})
                    const currentPath = decodeURIComponent(window.location.pathname.replace('/view/', ''));

                    if (currentPath === data.path) {
                        // Auto-refresh the current page
                        console.log('[SSE] Auto-refreshing current page');
                        navigate(window.location.pathname, false);
                    } else {
                        // Different file modified, show notification
                        showToast(`File updated: ${data.path}`, data.path);
                    }
                } else {
                    // In browser view, just show notification
                    showToast(`File updated: ${data.path}`, data.path);
                }
            } else if (data.type === 'connection_status') {
                console.log('[SSE] Handling connection_status:', data.count);
                updateConnectionStatus(data.count);
            }
        } catch (e) {
            console.log('[SSE] Not JSON, checking for plain string messages');
            // Fallback to plain string messages (backwards compatibility)
            if (event.data === 'reload') {
                console.log('[SSE] Handling reload message');
                // Check current view type from content element
                const content = document.getElementById('content');
                const viewType = content ? content.dataset.view : null;

                if (viewType === 'file') {
                    // File view - reload content to show updated markdown
                    const currentPath = window.location.pathname;
                    navigate(currentPath, false); // Don't add to history
                } else {
                    // Browser view - full reload
                    location.reload();
                }
            }
        }
    };

    eventSource.onerror = function(error) {
        console.log('[SSE] Connection error, reconnecting...');
        eventSource.close();

        // Show disconnected state
        const dot = document.getElementById('connection-dot');
        if (dot) {
            dot.classList.remove('connected');
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
        if (url === '/') {
            const newSidebarTree = doc.getElementById('sidebar-tree');
            const oldSidebarTree = document.getElementById('sidebar-tree');
            if (newSidebarTree && oldSidebarTree) {
                oldSidebarTree.innerHTML = newSidebarTree.innerHTML;
            }
        }

        // Update browser history
        if (addToHistory) {
            history.pushState({ url }, '', url);
        }

        // Reinitialize page-specific scripts
        reinitializeScripts();

        // Restore tree state after DOM update (for browser mode)
        restoreTreeState();

        console.log('[Navigate] Navigated to:', url);
    } catch (error) {
        console.error('[Navigate] Error:', error);
        // Fallback to full page load
        window.location.href = url;
    }
}

// Reinitialize page-specific functionality after content swap
function reinitializeScripts() {
    const content = document.getElementById('content');
    if (!content) return;

    const viewType = content.dataset.view;

    try {
        // Update download button visibility
        const downloadBtn = document.getElementById('download-btn');
        if (downloadBtn) {
            if (viewType === 'file') {
                downloadBtn.style.display = 'inline-block';
            } else {
                downloadBtn.style.display = 'none';
            }
        }

        // Common initialization for both views
        if (viewType === 'browser') {
            // Browser mode - setup collapsible directories
            if (typeof setupCollapse === 'function') {
                setupCollapse();
            } else {
                console.warn('[Reinit] setupCollapse not available');
            }
        } else if (viewType === 'file') {
            // File mode - no special initialization needed
            // Delete button uses inline onclick handler, no reinitialization required
        }

        // Initialize sidebar (Focus Mode) - works for both views
        if (typeof initializeSidebar === 'function') {
            initializeSidebar();
        }

        console.log('[Reinit] Scripts reinitialized for view:', viewType);
    } catch (error) {
        console.error('[Reinit] Error during script initialization:', error);
        // Don't crash - graceful degradation
    }
}

// Setup collapsible directory functionality
function setupCollapse() {
    // Initialize collapsed directories on page load
    const allItems = document.querySelectorAll('.tree-item');

    // Hide children of collapsed directories
    for (let item of allItems) {
        const depth = parseInt(item.dataset.depth) || 0;
        if (depth > 1) {
            const parent = findParentItem(item, allItems);
            if (parent) {
                const parentDir = parent.querySelector('.tree-directory');
                if (parentDir && parentDir.dataset.collapsed === 'true') {
                    item.classList.add('hidden');
                }
            }
        }
    }
}

// Setup delete button functionality
function setupDeleteButton() {
    // Delete button already set up via onclick in template
    // No additional setup needed
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

    // Intercept all internal navigation links (root and file views)
    if (url === '/' || url.startsWith('/view/')) {
        e.preventDefault();
        navigate(url);
    }
}

// Handle browser back/forward buttons
window.addEventListener('popstate', function(e) {
    if (e.state && e.state.url) {
        navigate(e.state.url, false);
    } else {
        // Fallback to current location
        navigate(window.location.pathname, false);
    }
});

// Initialize on page load
document.addEventListener('DOMContentLoaded', function() {
    console.log('[SPA] Initializing...');

    // Setup persistent SSE connection
    connectSSE();

    // Setup link interception
    document.body.addEventListener('click', interceptLinks);

    // Initialize current page scripts
    reinitializeScripts();

    // Restore tree state on initial page load
    restoreTreeState();

    // Add initial history state
    history.replaceState({ url: window.location.pathname }, '', window.location.pathname);

    // Initialize notification badge
    updateNotificationBadge();

    console.log('[SPA] Initialization complete');
});

// Cleanup on page unload
window.addEventListener('beforeunload', function() {
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

// Find parent tree item (used by setupCollapse)
function findParentItem(item, allItems) {
    const depth = parseInt(item.dataset.depth) || 0;
    if (depth <= 1) return null;

    const index = Array.from(allItems).indexOf(item);
    for (let i = index - 1; i >= 0; i--) {
        const candidateDepth = parseInt(allItems[i].dataset.depth) || 0;
        if (candidateDepth === depth - 1) {
            return allItems[i];
        }
    }
    return null;
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

function showToast(message, filePath) {
    // Save to notification history immediately
    saveNotification(message, filePath);

    // Create file info object
    const fileInfo = {
        name: filePath ? filePath.split('/').pop() : null,
        path: filePath,
        message: message,
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
        // Single file - show full message
        const file = files[0];
        return {
            primary: file.message,
            secondary: null,
            icon: '📄',
            href: file.path ? `/view/${encodeURIComponent(file.path)}` : '#',
            clickAction: null
        };
    }

    // Batch formatting
    const names = files.map(f => f.name);
    const icon = '📚';
    const href = '#';
    const clickAction = function(e) {
        if (e.target.classList.contains('toast-close')) return;
        e.preventDefault();
        toggleNotificationHistory();
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
        elements.icon.textContent = config.icon;
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

    // Show toast
    toast.style.display = 'flex';
    toast.classList.add('show');

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

    // Hide completely after transition completes
    setTimeout(() => {
        toast.style.display = 'none';
    }, TOAST_CONFIG.TRANSITION_TIME);
}

// ===== Connection Status Functions =====

function updateConnectionStatus(count) {
    const dot = document.getElementById('connection-dot');
    const countEl = document.getElementById('connection-count');

    if (countEl) {
        countEl.textContent = count;
    }

    if (dot) {
        if (count > 0) {
            dot.classList.add('connected');
        } else {
            dot.classList.remove('connected');
        }
    }
}

// ===== Dynamic Tree Manipulation =====

// Dynamically insert a new file into the tree
// Note: Event delegation from body.addEventListener('click', interceptLinks)
// automatically handles SPA navigation for dynamically inserted links
function insertFileIntoTree(filePath) {
    try {
        console.log('[insertFileIntoTree] Adding file:', filePath);
        const fileName = filePath.split('/').pop();
        const fileTree = document.getElementById('file-tree');

        if (!fileTree) {
            console.log('[insertFileIntoTree] No file-tree element found, skipping');
            return;
        }

        // Check if file already exists in tree (atomic write = CREATE event for existing file)
        const existingLinks = fileTree.querySelectorAll('.tree-item .tree-file a');
        for (let link of existingLinks) {
            if (link.textContent.trim() === fileName) {
                console.log('[insertFileIntoTree] File already exists in tree, skipping insertion');
                return;
            }
        }

        // Create new tree item HTML
        const div = document.createElement('div');
        div.className = 'tree-item';
        div.dataset.depth = '1';
        div.innerHTML = `
            <span class="tree-connector">├── </span>
            <span class="tree-file">
                <a href="/view/${encodeURIComponent(filePath)}">${escapeHtml(fileName)}</a>
                <span class="file-size">(0 bytes)</span>
            </span>
        `;

        // Find correct position (alphabetically among depth=1 files)
        const allItems = fileTree.querySelectorAll('.tree-item[data-depth="1"]');
        let inserted = false;

        for (let item of allItems) {
            const link = item.querySelector('.tree-file a');
            if (link) {
                // Get just the link text (filename), not the entire tree-file content
                const itemName = link.textContent.trim();
                console.log('[insertFileIntoTree] Comparing:', fileName, 'vs', itemName);

                if (fileName.localeCompare(itemName) < 0) {
                    item.parentNode.insertBefore(div, item);
                    inserted = true;
                    console.log('[insertFileIntoTree] Inserted before:', itemName);
                    break;
                }
            }
        }

        // If not inserted, append at end
        if (!inserted) {
            fileTree.appendChild(div);
            console.log('[insertFileIntoTree] Appended at end');
        }

        // Update file count in subtitle
        const subtitle = document.querySelector('.subtitle');
        if (subtitle) {
            const match = subtitle.textContent.match(/(\d+) markdown file/);
            if (match) {
                const newCount = parseInt(match[1]) + 1;
                subtitle.textContent = subtitle.textContent.replace(/\d+ markdown file/, `${newCount} markdown file`);
                console.log('[insertFileIntoTree] Updated count to:', newCount);
            }
        }

        console.log('[insertFileIntoTree] Successfully added file');
    } catch (error) {
        console.error('[insertFileIntoTree] Error:', error);
        // Don't crash the page - just log the error
    }
}

// Dynamically remove a file from the tree
function removeFileFromTree(filePath) {
    try {
        console.log('[removeFileFromTree] Removing file:', filePath);
        const fileName = filePath.split('/').pop();
        const fileTree = document.getElementById('file-tree');

        if (!fileTree) {
            console.log('[removeFileFromTree] No file-tree element found, skipping');
            return;
        }

        // Find and remove the tree item
        const allItems = fileTree.querySelectorAll('.tree-item');
        let removed = false;

        for (let item of allItems) {
            const link = item.querySelector('.tree-file a');
            if (link) {
                const href = link.getAttribute('href');
                const linkText = link.textContent.trim();

                // Debug logging
                console.log('[removeFileFromTree] Checking item - href:', href, 'text:', linkText, 'target:', fileName);

                // Match by href path or by filename (link text content)
                // The href should be /view/{filePath} where filePath is URL-encoded
                if (href === `/view/${encodeURIComponent(filePath)}` ||
                    href === `/view/${filePath}` ||
                    linkText === fileName) {
                    item.remove();
                    removed = true;
                    console.log('[removeFileFromTree] Removed item:', fileName);
                    break;
                }
            }
        }

        if (!removed) {
            console.log('[removeFileFromTree] File not found in tree:', fileName);
            return;
        }

        // Update file count in subtitle
        const subtitle = document.querySelector('.subtitle');
        if (subtitle) {
            const match = subtitle.textContent.match(/(\d+) markdown file/);
            if (match) {
                const newCount = Math.max(0, parseInt(match[1]) - 1);
                subtitle.textContent = subtitle.textContent.replace(/\d+ markdown file/, `${newCount} markdown file`);
                console.log('[removeFileFromTree] Updated count to:', newCount);
            }
        }

        console.log('[removeFileFromTree] Successfully removed file');
    } catch (error) {
        console.error('[removeFileFromTree] Error:', error);
        // Don't crash the page - just log the error
    }
}

// Download HTML functionality
function downloadHTML() {
    fetch('/download', {
        method: 'POST'
    })
    .then(response => {
        if (!response.ok) {
            throw new Error('Download failed');
        }
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
        alert('Failed to download HTML file');
    });
}

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

// Save tree expansion state and scroll position to sessionStorage
function saveTreeState() {
    try {
        const storageKey = getTreeStateKey();
        if (!storageKey) return;

        const fileTree = document.querySelector('#sidebar-tree .tree');
        if (!fileTree) return;

        const expandedDirs = [];
        const directories = fileTree.querySelectorAll('.tree-directory');

        directories.forEach(dir => {
            // Save directories that are NOT collapsed (i.e., expanded)
            if (dir.dataset.collapsed !== 'true') {
                // Use .dir-name element for reliable text extraction
                const dirNameEl = dir.querySelector('.dir-name');
                const name = dirNameEl ? dirNameEl.textContent.trim() : '';

                if (name) {
                    expandedDirs.push(name);
                }
            }
        });

        const state = {
            expandedDirs,
            scrollY: window.scrollY
        };

        sessionStorage.setItem(storageKey, JSON.stringify(state));
        console.log('[TreeState] Saved state for', storageKey, ':', state);
    } catch (error) {
        console.error('[TreeState] Failed to save:', error);
    }
}

// Restore tree expansion state and scroll position from sessionStorage
function restoreTreeState() {
    try {
        const storageKey = getTreeStateKey();
        if (!storageKey) return;

        const stored = sessionStorage.getItem(storageKey);
        if (!stored) return;

        const state = JSON.parse(stored);
        const fileTree = document.querySelector('#sidebar-tree .tree');
        if (!fileTree) return;

        console.log('[TreeState] Restoring state for', storageKey, ':', state);

        // Restore expanded directories
        const directories = fileTree.querySelectorAll('.tree-directory');

        directories.forEach(dir => {
            // Use .dir-name element for reliable text extraction
            const dirNameEl = dir.querySelector('.dir-name');
            const name = dirNameEl ? dirNameEl.textContent.trim() : '';

            const shouldBeExpanded = state.expandedDirs.includes(name);
            const isCurrentlyCollapsed = dir.dataset.collapsed === 'true';

            // If directory should be expanded but is currently collapsed, toggle it
            if (shouldBeExpanded && isCurrentlyCollapsed) {
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

// ===== Notification History Functions =====

const NOTIFICATION_STORAGE_KEY = 'peekm_notification_history';
const MAX_NOTIFICATIONS = 10;
const RECENT_THRESHOLD_MS = 5 * 60 * 1000; // 5 minutes

// Save notification to sessionStorage
function saveNotification(message, filePath) {
    try {
        const notifications = getNotificationHistory();
        const notification = {
            id: Date.now(),
            message: message,
            filePath: filePath,
            timestamp: Date.now()
        };

        // Add to beginning, keep max 10
        notifications.unshift(notification);
        if (notifications.length > MAX_NOTIFICATIONS) {
            notifications.pop();
        }

        sessionStorage.setItem(NOTIFICATION_STORAGE_KEY, JSON.stringify(notifications));
        updateNotificationBadge();
    } catch (error) {
        console.error('[Notification] Failed to save:', error);
    }
}

// Get notification history from sessionStorage
function getNotificationHistory() {
    try {
        const stored = sessionStorage.getItem(NOTIFICATION_STORAGE_KEY);
        return stored ? JSON.parse(stored) : [];
    } catch (error) {
        console.error('[Notification] Failed to load history:', error);
        return [];
    }
}

// Close notification dropdown
function closeNotificationDropdown() {
    const dropdown = document.getElementById('notification-dropdown');
    if (dropdown) dropdown.style.display = 'none';
    document.removeEventListener('click', closeNotificationDropdown);
}

// Clear notification history
function clearNotificationHistory() {
    sessionStorage.removeItem(NOTIFICATION_STORAGE_KEY);
    updateNotificationBadge();
    renderNotificationList();
    closeNotificationDropdown();
}

// Toggle notification history dropdown
function toggleNotificationHistory() {
    const dropdown = document.getElementById('notification-dropdown');
    if (!dropdown) return;

    const isVisible = dropdown.style.display !== 'none';

    if (isVisible) {
        closeNotificationDropdown();
    } else {
        // Close theme dropdown if open (mutual exclusivity)
        const themeDropdown = document.getElementById('theme-dropdown');
        if (themeDropdown && themeDropdown.style.display !== 'none') {
            if (typeof closeThemeDropdown === 'function') {
                closeThemeDropdown();
            }
        }

        renderNotificationList();
        dropdown.style.display = 'flex';

        // Add click-outside listener (prevents immediate close from current click bubbling)
        setTimeout(() => {
            document.addEventListener('click', closeNotificationDropdown);
        }, 0);
    }
}

// Render notification list in dropdown
function renderNotificationList() {
    const listEl = document.getElementById('notification-list');
    if (!listEl) return;

    const notifications = getNotificationHistory();

    if (notifications.length === 0) {
        listEl.innerHTML = '<div class="notification-empty">No recent notifications</div>';
        return;
    }

    listEl.innerHTML = notifications.map(notif => {
        const timeAgo = getTimeAgo(notif.timestamp);
        const href = notif.filePath ? `/view/${encodeURIComponent(notif.filePath)}` : '#';

        return `
            <a href="${href}" class="notification-item" onclick="handleNotificationClick(event, '${href}')">
                <div class="notification-item-message">${escapeHtml(notif.message)}</div>
                <div class="notification-item-time">${timeAgo}</div>
            </a>
        `;
    }).join('');
}

// Handle notification item click (close dropdown + navigate)
function handleNotificationClick(event, href) {
    // Close dropdown
    const dropdown = document.getElementById('notification-dropdown');
    if (dropdown) {
        dropdown.style.display = 'none';
    }

    // Let browser handle Cmd/Ctrl+Click naturally (opens new tab)
    if (event.metaKey || event.ctrlKey) {
        return; // Don't prevent default - let <a> tag handle it
    }

    // For normal clicks, use SPA navigation if available
    if (href && href !== '#' && typeof navigate === 'function') {
        event.preventDefault();
        navigate(href);
    }
}

// Update notification badge count (shows count of notifications < 5 min old)
function updateNotificationBadge() {
    const badge = document.getElementById('notification-badge');
    if (!badge) return;

    const notifications = getNotificationHistory();
    const now = Date.now();

    // Count notifications less than 5 minutes old
    const recentCount = notifications.filter(n => {
        return (now - n.timestamp) < RECENT_THRESHOLD_MS;
    }).length;

    if (recentCount > 0) {
        badge.textContent = recentCount;
        badge.style.display = 'inline-block';
    } else {
        badge.style.display = 'none';
    }
}

// Convert timestamp to relative time string
function getTimeAgo(timestamp) {
    const seconds = Math.floor((Date.now() - timestamp) / 1000);

    if (seconds < 60) {
        return 'just now';
    }

    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) {
        return `${minutes} min ago`;
    }

    const hours = Math.floor(minutes / 60);
    if (hours < 24) {
        return `${hours} hour${hours > 1 ? 's' : ''} ago`;
    }

    const days = Math.floor(hours / 24);
    return `${days} day${days > 1 ? 's' : ''} ago`;
}

// Close dropdown when clicking outside
document.addEventListener('click', function(e) {
    const dropdown = document.getElementById('notification-dropdown');
    const btn = document.getElementById('notification-btn');

    if (!dropdown || !btn) return;

    // If click is outside both dropdown and button, close dropdown
    if (!dropdown.contains(e.target) && !btn.contains(e.target)) {
        dropdown.style.display = 'none';
    }
});

// ===== Focus Mode: Toggleable Sidebar Functions =====

const SIDEBAR_STORAGE_KEY = 'peekm_sidebar_state';

// Toggle sidebar visibility
function toggleSidebar() {
    const container = document.querySelector('.layout-container');
    if (!container) return;

    const isExpanded = container.dataset.sidebar === 'expanded';
    const newState = isExpanded ? 'collapsed' : 'expanded';

    container.dataset.sidebar = newState;

    // Update button tooltip
    const toggleBtn = document.getElementById('sidebar-toggle');
    if (toggleBtn) {
        toggleBtn.title = newState === 'expanded'
            ? 'Hide navigation (Cmd/Ctrl+B)'
            : 'Show navigation (Cmd/Ctrl+B)';
        toggleBtn.setAttribute('aria-label',
            newState === 'expanded'
                ? 'Hide navigation sidebar'
                : 'Show navigation sidebar'
        );
    }

    // Save preference to localStorage
    try {
        localStorage.setItem(SIDEBAR_STORAGE_KEY, newState);
    } catch (error) {
        console.error('[Sidebar] Failed to save state:', error);
    }

    console.log('[Sidebar] Toggled to:', newState);
}

// Initialize sidebar state from localStorage
function initializeSidebar() {
    const content = document.getElementById('content');
    if (!content) return;

    const viewType = content.dataset.view;

    // Unified layout: Always show sidebar (for 'file' and 'empty' views)
    if (viewType === 'file' || viewType === 'empty') {
        // Show hamburger button
        updateSidebarToggleButton();

        // Restore saved state or default to expanded (Persistent Navigation)
        const container = document.querySelector('.layout-container');
        if (!container) return;

        try {
            const savedState = localStorage.getItem(SIDEBAR_STORAGE_KEY);
            if (savedState === 'collapsed') {
                // User explicitly hid it before, respect that
                container.dataset.sidebar = 'collapsed';
            } else {
                // Default: show sidebar (visible by default)
                container.dataset.sidebar = 'expanded';
            }
        } catch (error) {
            console.error('[Sidebar] Failed to load state:', error);
            // Fallback: show sidebar
            container.dataset.sidebar = 'expanded';
        }

        // Update breadcrumb (only for file view)
        if (viewType === 'file') {
            updateBreadcrumb();
            // Highlight current file in sidebar
            highlightCurrentFile();
        }
    }
}

// Update hamburger button visibility
function updateSidebarToggleButton() {
    const toggleBtn = document.getElementById('sidebar-toggle');
    const content = document.getElementById('content');

    if (!toggleBtn || !content) return;

    const viewType = content.dataset.view;

    // Show hamburger button in unified layout (file or empty views)
    if (viewType === 'file' || viewType === 'empty') {
        toggleBtn.style.display = 'inline-block';
    } else {
        toggleBtn.style.display = 'none';
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
        const content = document.getElementById('content');
        if (content && content.dataset.view === 'file') {
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

    const searchQuery = query.toLowerCase().trim();
    const allFiles = getAllFiles();

    // Filter matching files
    searchResults = allFiles.filter(file =>
        file.name.toLowerCase().includes(searchQuery)
    );

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
    if ((e.metaKey || e.ctrlKey) && e.key === 'p') {
        e.preventDefault();
        const searchInput = document.getElementById('file-search');
        if (searchInput) {
            searchInput.focus();
            searchInput.select();
        }
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
});

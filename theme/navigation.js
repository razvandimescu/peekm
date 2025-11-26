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

        // Update browser history
        if (addToHistory) {
            history.pushState({ url }, '', url);
        }

        // Reinitialize page-specific scripts
        reinitializeScripts();

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

function showToast(message, filePath) {
    const toast = document.getElementById('toast');
    const messageEl = document.getElementById('toast-message');
    if (!toast || !messageEl) return;

    messageEl.textContent = message;
    toastFilePath = filePath;

    // Set href for native browser behavior (Cmd/Ctrl+Click, context menu, etc.)
    if (filePath) {
        toast.href = `/view/${encodeURIComponent(filePath)}`;
    } else {
        toast.href = '#';
    }

    // Show toast (remove inline display:none, let CSS handle visibility via .show class)
    toast.style.display = 'flex';
    toast.classList.add('show');

    // Clear existing timeout
    if (toastTimeout) {
        clearTimeout(toastTimeout);
    }

    // Auto-hide after 5 seconds
    toastTimeout = setTimeout(hideToast, 5000);

    // Save to notification history
    saveNotification(message, filePath);
}

function hideToast() {
    const toast = document.getElementById('toast');
    if (!toast) return;

    toast.classList.remove('show');
    toastFilePath = null;

    // Hide completely after transition completes (300ms per CSS)
    setTimeout(() => {
        toast.style.display = 'none';
    }, 300);
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

// Clear notification history
function clearNotificationHistory() {
    sessionStorage.removeItem(NOTIFICATION_STORAGE_KEY);
    updateNotificationBadge();
    renderNotificationList();

    // Close dropdown
    const dropdown = document.getElementById('notification-dropdown');
    if (dropdown) {
        dropdown.style.display = 'none';
    }
}

// Toggle notification history dropdown
function toggleNotificationHistory() {
    const dropdown = document.getElementById('notification-dropdown');
    if (!dropdown) return;

    const isVisible = dropdown.style.display !== 'none';

    if (isVisible) {
        dropdown.style.display = 'none';
    } else {
        renderNotificationList();
        dropdown.style.display = 'flex';
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

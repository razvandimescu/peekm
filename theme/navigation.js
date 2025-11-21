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
        // Try to parse as JSON for typed messages
        try {
            const data = JSON.parse(event.data);

            if (data.type === 'file_added') {
                showToast(`New file: ${data.path}`, data.path);
                // Dynamically insert file instead of reloading
                insertFileIntoTree(data.path);
            } else if (data.type === 'connection_status') {
                updateConnectionStatus(data.count);
            }
        } catch (e) {
            // Fallback to plain string messages (backwards compatibility)
            if (event.data === 'reload') {
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

    // Common initialization for both views
    if (viewType === 'browser') {
        // Browser mode - setup collapsible directories
        setupCollapse();
    } else if (viewType === 'file') {
        // File mode - setup delete button
        setupDeleteButton();
    }

    console.log('[Reinit] Scripts reinitialized for view:', viewType);
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

    // Only intercept internal links
    const url = link.getAttribute('href');
    if (!url || url.startsWith('http') || url.startsWith('//')) {
        return;
    }

    // Don't intercept if it's the navigation modal or other special cases
    if (link.classList.contains('back-button') || url === '/') {
        e.preventDefault();
        navigate(url);
        return;
    }

    // Intercept file view links
    if (url.startsWith('/view/')) {
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

    console.log('[SPA] Initialization complete');
});

// Cleanup on page unload
window.addEventListener('beforeunload', function() {
    if (eventSource) {
        eventSource.close();
    }
});

// Theme management
function setTheme(mode) {
    const html = document.documentElement;
    const body = document.body;
    localStorage.setItem('theme', mode);

    if (mode === 'auto') {
        // Remove data-theme to let media query handle it
        html.removeAttribute('data-theme');
        body.removeAttribute('data-theme');
        html.setAttribute('data-color-mode', 'auto');
    } else {
        // Force specific theme
        html.setAttribute('data-theme', mode);
        body.setAttribute('data-theme', mode);
        html.setAttribute('data-color-mode', mode);
    }

    updateThemeButton(mode);
}

function updateThemeButton(mode) {
    const icons = { light: '☀️', dark: '🌙', auto: '💻' };
    const labels = { light: 'Light', dark: 'Dark', auto: 'Auto' };

    // Update toggle button display
    const currentIcon = document.getElementById('theme-current-icon');
    const currentLabel = document.getElementById('theme-current-label');
    if (currentIcon) currentIcon.textContent = icons[mode];
    if (currentLabel) currentLabel.textContent = labels[mode];

    // Update checkmarks and aria-selected in dropdown
    document.querySelectorAll('.theme-option').forEach(opt => {
        const isSelected = opt.dataset.theme === mode;
        opt.setAttribute('aria-selected', isSelected);
        const checkmark = opt.querySelector('.theme-checkmark');
        if (checkmark) checkmark.style.display = isSelected ? 'inline' : 'none';
    });
}

// Dropdown interaction functions
function toggleThemeDropdown(event) {
    event.stopPropagation();
    const dropdown = document.getElementById('theme-dropdown');
    const button = document.getElementById('theme-toggle-btn');
    const isOpen = dropdown.style.display !== 'none';

    if (isOpen) {
        closeThemeDropdown();
    } else {
        // Close notification dropdown if open (mutual exclusivity)
        const notifDropdown = document.getElementById('notification-dropdown');
        if (notifDropdown && notifDropdown.style.display !== 'none') {
            if (typeof closeNotificationDropdown === 'function') {
                closeNotificationDropdown();
            }
        }

        dropdown.style.display = 'block';
        button.setAttribute('aria-expanded', 'true');

        // Auto-focus first option after dropdown renders (prevents race condition with display: block)
        setTimeout(() => {
            const firstOption = dropdown.querySelector('.theme-option');
            if (firstOption) firstOption.focus();
        }, 0);

        // Register click-outside listener after current event completes
        // (prevents immediate close from toggle button click bubbling)
        setTimeout(() => {
            document.addEventListener('click', closeThemeDropdown);
        }, 0);
    }
}

function closeThemeDropdown() {
    const dropdown = document.getElementById('theme-dropdown');
    const button = document.getElementById('theme-toggle-btn');
    if (dropdown) dropdown.style.display = 'none';
    if (button) button.setAttribute('aria-expanded', 'false');
    document.removeEventListener('click', closeThemeDropdown);
}

function selectTheme(theme) {
    setTheme(theme);
    closeThemeDropdown();
}

// Keyboard navigation for theme dropdown
function initKeyboardNavigation() {
    const themeDropdown = document.getElementById('theme-dropdown');
    if (!themeDropdown) return;

    themeDropdown.addEventListener('keydown', function(e) {
        const options = Array.from(themeDropdown.querySelectorAll('.theme-option'));
        const currentIndex = options.indexOf(document.activeElement);

        switch(e.key) {
            case 'ArrowDown':
                e.preventDefault();
                const nextIdx = (currentIndex + 1) % options.length;
                options[nextIdx].focus();
                break;
            case 'ArrowUp':
                e.preventDefault();
                const prevIdx = (currentIndex - 1 + options.length) % options.length;
                options[prevIdx].focus();
                break;
            case 'Escape':
                e.preventDefault();
                closeThemeDropdown();
                document.getElementById('theme-toggle-btn').focus();
                break;
            case 'Enter':
            case ' ':
                e.preventDefault();
                if (document.activeElement?.classList.contains('theme-option')) {
                    document.activeElement.click();
                }
                break;
        }
    });
}

// Initialize theme on page load
const savedTheme = localStorage.getItem('theme') || 'auto';
setTheme(savedTheme);

// Initialize keyboard navigation when DOM is ready
if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initKeyboardNavigation);
} else {
    initKeyboardNavigation();
}

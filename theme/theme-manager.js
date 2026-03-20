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
    const icons = {
        light: '<svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor"><path d="M8 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8Zm0 1.5a5.5 5.5 0 1 1 0-11 5.5 5.5 0 0 1 0 11ZM8 0a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0V.75A.75.75 0 0 1 8 0Zm0 13a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 8 13ZM2.343 2.343a.75.75 0 0 1 1.061 0l1.06 1.061a.75.75 0 0 1-1.06 1.06l-1.06-1.06a.75.75 0 0 1 0-1.06Zm9.193 9.193a.75.75 0 0 1 1.06 0l1.061 1.06a.75.75 0 0 1-1.06 1.061l-1.061-1.06a.75.75 0 0 1 0-1.061ZM0 8a.75.75 0 0 1 .75-.75h1.5a.75.75 0 0 1 0 1.5H.75A.75.75 0 0 1 0 8Zm13 0a.75.75 0 0 1 .75-.75h1.5a.75.75 0 0 1 0 1.5h-1.5A.75.75 0 0 1 13 8ZM2.343 13.657a.75.75 0 0 1 0-1.06l1.06-1.061a.75.75 0 0 1 1.061 1.06l-1.06 1.061a.75.75 0 0 1-1.061 0Zm9.193-9.193a.75.75 0 0 1 0-1.06l1.061-1.061a.75.75 0 0 1 1.06 1.06l-1.06 1.061a.75.75 0 0 1-1.06 0Z"/></svg>',
        dark: '<svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor"><path d="M9.598 1.591a.749.749 0 0 1 .785-.175 7.001 7.001 0 1 1-8.967 8.967.75.75 0 0 1 .961-.96 5.5 5.5 0 0 0 7.221-7.832.749.749 0 0 1 0-.785v-.215Z"/></svg>',
        auto: '<svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor"><path d="M14.5 1h-13a.5.5 0 0 0-.5.5v10a.5.5 0 0 0 .5.5h13a.5.5 0 0 0 .5-.5v-10a.5.5 0 0 0-.5-.5ZM14 11H2V2h12v9ZM5 14.25a.75.75 0 0 1 .75-.75h4.5a.75.75 0 0 1 0 1.5h-4.5a.75.75 0 0 1-.75-.75Z"/></svg>'
    };
    const labels = { light: 'Light', dark: 'Dark', auto: 'Auto' };

    // Update toggle button display
    const currentIcon = document.getElementById('theme-current-icon');
    const currentLabel = document.getElementById('theme-current-label');
    if (currentIcon) currentIcon.innerHTML = icons[mode];
    if (currentLabel) currentLabel.textContent = labels[mode];

    // Update checkmarks and aria-selected in dropdown
    document.querySelectorAll('.theme-option').forEach(opt => {
        const isSelected = opt.dataset.themeValue === mode;
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

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

    updateThemeButtons(mode);
}

function updateThemeButtons(mode) {
    document.querySelectorAll('.theme-toggle button').forEach(btn => {
        btn.classList.remove('active');
    });
    const btn = document.getElementById('theme-' + mode);
    if (btn) btn.classList.add('active');
}

// Initialize theme on page load
const savedTheme = localStorage.getItem('theme') || 'auto';
setTheme(savedTheme);

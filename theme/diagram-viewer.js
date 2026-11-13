// Full-screen viewer for mermaid diagrams. A click on a diagram opens it at its
// natural size, scrolled so the clicked point stays under the pointer. The
// browser does the scrolling; the inline diagram is the overview.
document.addEventListener('click', function (e) {
    var svg = e.target.closest('.mermaid > svg');
    // Links in the diagram keep working, and selecting label text does not open the viewer.
    if (!svg || e.target.closest('a, .clickable') || !window.getSelection().isCollapsed) return;
    var rect = svg.getBoundingClientRect();
    var box = svg.viewBox.baseVal;
    var w = (box && box.width) || rect.width, h = (box && box.height) || rect.height;

    var art = svg.cloneNode(true);
    art.style.maxWidth = 'none';
    art.setAttribute('width', w);
    art.setAttribute('height', h);

    var dialog = document.createElement('dialog');
    dialog.className = 'diagram-viewer';
    dialog.setAttribute('aria-label', 'Diagram');
    dialog.tabIndex = -1;
    dialog.innerHTML = '<button class="diagram-viewer-close" title="Close (Esc)" aria-label="Close">×</button>';
    dialog.appendChild(art);
    dialog.firstChild.addEventListener('click', function () { dialog.close(); });
    // The page behind is inert, so its shortcuts (Escape handlers included) must not see these keys.
    // Escape still closes the dialog: that is the browser's default action, not a listener.
    dialog.addEventListener('keydown', function (ev) { ev.stopPropagation(); });
    dialog.addEventListener('close', function () { dialog.remove(); });
    // Inside .markdown-body, so the dialog inherits the content's forced light/dark theme.
    svg.parentElement.after(dialog);
    dialog.showModal();
    // Focus the scroller, not the fixed close button, so the arrow and Page keys scroll the diagram.
    dialog.focus();
    dialog.scrollLeft = (e.clientX - rect.left) / rect.width * w - e.clientX;
    dialog.scrollTop = (e.clientY - rect.top) / rect.height * h - e.clientY;
});

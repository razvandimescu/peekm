// Full-screen viewer for mermaid diagrams. The expand button, or a click on the
// diagram, opens it. Drag or scroll pans; pinch, Ctrl/Cmd + scroll, double-click,
// or +/- zooms.
(function () {
    var MIN_SCALE = 0.1, MAX_SCALE = 8, MAX_FIT = 2, STEP = 1.25, PAN_STEP = 80;
    var WHEEL_ZOOM_MAX_DELTA = 10; // a mouse-wheel notch zooms about 10%, like a trackpad pinch

    // The button sits beside the diagram, not inside it: mermaid reads the element's
    // text as its source, and the shared view re-renders diagrams on theme change.
    window.enhanceDiagrams = function () {
        document.querySelectorAll('.mermaid').forEach(function (el) {
            if (el.parentElement.classList.contains('diagram')) return;
            var wrap = document.createElement('div');
            wrap.className = 'diagram';
            var btn = document.createElement('button');
            btn.className = 'diagram-expand';
            btn.title = 'Expand diagram';
            btn.setAttribute('aria-label', 'Expand diagram');
            el.replaceWith(wrap);
            wrap.append(el, btn);
        });
    };

    document.addEventListener('click', function (e) {
        var btn = e.target.closest('.diagram-expand');
        if (btn) {
            openViewer(btn.parentElement.querySelector('.mermaid > svg'));
            return;
        }
        var svg = e.target.closest('.mermaid > svg');
        // Links in the diagram keep working, and selecting label text does not open the viewer.
        if (svg && !e.target.closest('a, .clickable') && window.getSelection().isCollapsed) openViewer(svg);
    });

    function openViewer(svg) {
        if (!svg) return;
        var box = (svg.getAttribute('viewBox') || '').split(/[\s,]+/).map(Number);
        var rect = svg.getBoundingClientRect();
        var w = box[2] || rect.width, h = box[3] || rect.height;

        var art = svg.cloneNode(true);
        art.style.maxWidth = 'none';
        art.setAttribute('width', w);
        art.setAttribute('height', h);

        var dialog = document.createElement('dialog');
        dialog.className = 'diagram-viewer';
        dialog.setAttribute('aria-label', 'Diagram');
        dialog.innerHTML =
            '<div class="diagram-viewer-stage"><div class="diagram-viewer-canvas"></div></div>' +
            '<div class="diagram-viewer-tools">' +
            '<button data-act="out" title="Zoom out (-)" aria-label="Zoom out">−</button>' +
            '<button data-act="fit" title="Fit to screen (0)">Fit</button>' +
            '<button data-act="in" title="Zoom in (+)" aria-label="Zoom in">+</button>' +
            '<button data-act="close" title="Close (Esc)" aria-label="Close" autofocus>×</button>' +
            '</div>';
        var stage = dialog.querySelector('.diagram-viewer-stage');
        var canvas = dialog.querySelector('.diagram-viewer-canvas');
        canvas.appendChild(art);

        var view = { x: 0, y: 0, s: 1 };
        function apply() {
            canvas.style.transform = 'translate(' + view.x + 'px,' + view.y + 'px) scale(' + view.s + ')';
        }
        function pan(dx, dy) {
            view.x += dx;
            view.y += dy;
            apply();
        }
        // The toolbar overlays the top edge, so the fitted diagram starts below it.
        function fit() {
            var top = 64, pad = 24;
            var sw = stage.clientWidth, sh = stage.clientHeight;
            view.s = Math.min((sw - 2 * pad) / w, (sh - top - pad) / h, MAX_FIT);
            view.x = (sw - w * view.s) / 2;
            view.y = top + (sh - top - pad - h * view.s) / 2;
            apply();
        }
        // (cx, cy) is in viewport pixels; the stage fills the viewport.
        function zoomAt(cx, cy, factor) {
            var s = Math.min(MAX_SCALE, Math.max(MIN_SCALE, view.s * factor));
            var f = s / view.s;
            view.x = cx - (cx - view.x) * f;
            view.y = cy - (cy - view.y) * f;
            view.s = s;
            apply();
        }
        function zoomCenter(factor) {
            zoomAt(stage.clientWidth / 2, stage.clientHeight / 2, factor);
        }

        // One pointer pans. Two pointers pinch: pan by the midpoint, zoom by the spread.
        var pointers = new Map();
        stage.addEventListener('pointerdown', function (e) {
            if (e.button !== 0) return;
            stage.setPointerCapture(e.pointerId);
            pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
        });
        stage.addEventListener('pointermove', function (e) {
            var prev = pointers.get(e.pointerId);
            if (!prev) return;
            var cur = { x: e.clientX, y: e.clientY };
            if (pointers.size === 1) {
                pan(cur.x - prev.x, cur.y - prev.y);
            } else if (pointers.size === 2) {
                var other;
                pointers.forEach(function (p, id) { if (id !== e.pointerId) other = p; });
                var before = Math.hypot(prev.x - other.x, prev.y - other.y);
                var after = Math.hypot(cur.x - other.x, cur.y - other.y);
                view.x += (cur.x - prev.x) / 2;
                view.y += (cur.y - prev.y) / 2;
                zoomAt((cur.x + other.x) / 2, (cur.y + other.y) / 2, before ? after / before : 1);
            }
            pointers.set(e.pointerId, cur);
        });
        function release(e) { pointers.delete(e.pointerId); }
        stage.addEventListener('pointerup', release);
        stage.addEventListener('pointercancel', release);
        stage.addEventListener('dblclick', function (e) { zoomAt(e.clientX, e.clientY, 2); });

        // On the dialog, not the stage, so a scroll over the toolbar cannot scroll the page behind.
        // Chrome and Firefox report a trackpad pinch as a wheel event with ctrlKey.
        dialog.addEventListener('wheel', function (e) {
            e.preventDefault();
            var unit = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? stage.clientHeight : 1;
            if (e.ctrlKey || e.metaKey) {
                var d = Math.max(-WHEEL_ZOOM_MAX_DELTA, Math.min(WHEEL_ZOOM_MAX_DELTA, e.deltaY * unit));
                zoomAt(e.clientX, e.clientY, Math.exp(-d / 100));
            } else {
                pan(-e.deltaX * unit, -e.deltaY * unit);
            }
        }, { passive: false });

        // Safari reports a trackpad pinch as gesture events. On touch screens pointer events handle it.
        var gestureScale = 1;
        stage.addEventListener('gesturestart', function (e) {
            e.preventDefault();
            gestureScale = 1;
        });
        stage.addEventListener('gesturechange', function (e) {
            e.preventDefault();
            if (pointers.size) return;
            zoomAt(e.clientX, e.clientY, e.scale / gestureScale);
            gestureScale = e.scale;
        });

        var actions = {
            in: function () { zoomCenter(STEP); },
            out: function () { zoomCenter(1 / STEP); },
            fit: fit,
            close: function () { dialog.close(); }
        };
        dialog.querySelector('.diagram-viewer-tools').addEventListener('click', function (e) {
            var b = e.target.closest('button');
            if (b) actions[b.dataset.act]();
        });
        var keys = {
            '+': actions.in, '=': actions.in, '-': actions.out, '0': fit,
            ArrowLeft: function () { pan(PAN_STEP, 0); },
            ArrowRight: function () { pan(-PAN_STEP, 0); },
            ArrowUp: function () { pan(0, PAN_STEP); },
            ArrowDown: function () { pan(0, -PAN_STEP); }
        };
        // The page behind is inert, so its shortcuts (Escape handlers included) must not see these keys.
        // Escape still closes the dialog: that is the browser's default action, not a listener.
        dialog.addEventListener('keydown', function (e) {
            e.stopPropagation();
            var act = keys[e.key];
            if (!act || e.ctrlKey || e.metaKey || e.altKey) return;
            e.preventDefault();
            act();
        });

        dialog.addEventListener('close', function () { dialog.remove(); });
        // Inside the content, so the dialog inherits the content's forced light/dark theme.
        (svg.closest('.diagram') || document.body).appendChild(dialog);
        dialog.showModal();
        fit();
    }
})();

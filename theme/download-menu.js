(function() {
    // Cap display width so Word doesn't render at the PNG's native pixel size.
    // 600px ≈ 6.25in at 96dpi, fits Word's default content area (8.5in - 2in margins).
    var TARGET_DISPLAY_WIDTH_PX = 600;
    var libsPromise = null;

    // Resolve once mermaid has rendered every diagram in root, or after a short timeout.
    function waitForMermaid(root) {
        var els = root.querySelectorAll('.mermaid');
        if (els.length === 0) return Promise.resolve();
        return new Promise(function(resolve) {
            var start = Date.now();
            (function tick() {
                var done = Array.prototype.every.call(els, function(el) {
                    return el.getAttribute('data-processed') === 'true' || el.querySelector('svg');
                });
                if (done || Date.now() - start > 5000) resolve();
                else setTimeout(tick, 100);
            })();
        });
    }

    // Lazy-load html-to-image (handles foreignObject) + html-docx-js. Both are
    // vendored into the binary and served same-origin, so the export needs no
    // third-party CDN and no SRI to drift. html-to-image is needed because mermaid
    // uses <foreignObject> for HTML labels, which a plain <img src="svg-blob"> +
    // canvas pipeline can't rasterise.
    function loadLibs(vendorBase) {
        if (libsPromise) return libsPromise;
        libsPromise = Promise.all([
            loadScript(vendorBase + 'html-to-image.min.js'),
            loadScript(vendorBase + 'html-docx.min.js')
        ]).then(function() {
            return { htmlToImage: window.htmlToImage, htmlDocx: window.htmlDocx };
        }).catch(function(err) {
            libsPromise = null;
            throw err;
        });
        return libsPromise;
    }

    function loadScript(src) {
        return new Promise(function(resolve, reject) {
            var s = document.createElement('script');
            s.src = src;
            s.onload = resolve;
            s.onerror = function() { reject(new Error('Failed to load ' + src)); };
            document.head.appendChild(s);
        });
    }

    // Rasterise an SVG (including <foreignObject> content) to a PNG data URL plus
    // pixel display dimensions for Word's HTML import to honour.
    function rasterise(svgEl, htmlToImage) {
        var rect = svgEl.getBoundingClientRect();
        var srcW = Math.max(rect.width || 600, 1);
        var srcH = Math.max(rect.height || 400, 1);
        var displayW = Math.min(srcW, TARGET_DISPLAY_WIDTH_PX);
        var displayH = displayW * (srcH / srcW);
        return htmlToImage.toPng(svgEl, {
            width: srcW,
            height: srcH,
            pixelRatio: 2,
            backgroundColor: '#ffffff'
        }).then(function(dataUrl) {
            return { dataUrl: dataUrl, width: Math.round(displayW), height: Math.round(displayH) };
        });
    }

    // Re-render a mermaid source in light theme into an offscreen light-mode
    // wrapper, then rasterise. Needed because dark-mode diagrams on a white DOCX
    // page lose contrast (especially classDef-styled "dim" nodes whose foreignObject
    // text was inheriting white from page CSS).
    async function renderMermaidLight(sourceCode, htmlToImage) {
        var id = 'peekm-docx-mm-' + Math.random().toString(36).slice(2, 8);
        var result = await mermaid.render(id, sourceCode);

        var wrapper = document.createElement('div');
        wrapper.setAttribute('data-color-mode', 'light');
        wrapper.setAttribute('data-light-theme', 'light');
        wrapper.style.cssText = 'position:fixed;left:-10000px;top:0;background:#fff;color:#1f2328;';
        wrapper.innerHTML = result.svg;
        document.body.appendChild(wrapper);

        try {
            var svgEl = wrapper.querySelector('svg');
            if (!svgEl) throw new Error('mermaid.render returned no SVG');
            await new Promise(function(r) { requestAnimationFrame(r); });
            return await rasterise(svgEl, htmlToImage);
        } finally {
            wrapper.remove();
        }
    }

    function escapeHtml(s) {
        return String(s).replace(/[&<>"']/g, function(c) {
            return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
        });
    }

    async function buildDocxBlob(src, fileName, vendorBase) {
        await waitForMermaid(src);
        var libs = await loadLibs(vendorBase);

        var clone = src.cloneNode(true);

        // Switch mermaid to light theme for any re-renders during export.
        // Live SVGs already on the page are unaffected (initialize only changes future renders).
        var pageIsDark = isDarkMode();
        if (typeof mermaid !== 'undefined') {
            mermaid.initialize({ startOnLoad: false, theme: 'default', securityLevel: 'loose' });
        }

        try {
            var liveSvgs = src.querySelectorAll('svg');
            var clonedSvgs = clone.querySelectorAll('svg');

            for (var i = 0; i < clonedSvgs.length; i++) {
                try {
                    var liveSvg = liveSvgs[i] || clonedSvgs[i];
                    var block = liveSvg.closest('.mermaid');
                    var source = block && block.dataset.mermaidSource;
                    var png = source
                        ? await renderMermaidLight(source, libs.htmlToImage)
                        : await rasterise(liveSvg, libs.htmlToImage);
                    var img = document.createElement('img');
                    img.src = png.dataUrl;
                    img.setAttribute('width', png.width);
                    img.setAttribute('height', png.height);
                    clonedSvgs[i].parentNode.replaceChild(img, clonedSvgs[i]);
                } catch (err) {
                    console.warn('[docx] svg→png failed', err);
                    var ph = document.createElement('p');
                    ph.textContent = '[Diagram could not be exported]';
                    ph.style.fontStyle = 'italic';
                    ph.style.color = '#888';
                    clonedSvgs[i].parentNode.replaceChild(ph, clonedSvgs[i]);
                }
            }
        } finally {
            // Restore mermaid theme to match the live page state.
            if (typeof mermaid !== 'undefined') {
                mermaid.initialize({ startOnLoad: false, theme: pageIsDark ? 'dark' : 'default', securityLevel: 'loose' });
            }
        }

        // Inline the page's styles so headings/code/tables survive Word's HTML import.
        var css = '';
        document.querySelectorAll('style').forEach(function(s) { css += s.textContent + '\n'; });

        // No body padding — Word's page margins (1" default = ~96px each side on letter)
        // already provide breathing room. Adding HTML padding compounds unpredictably
        // with Word's margins and squeezes the content area.
        // Force light theme on the exported HTML — DOCX is read on white paper.
        var html = '<!DOCTYPE html><html data-color-mode="light" data-light-theme="light"><head><meta charset="utf-8"><title>' +
            escapeHtml(fileName) + '</title><style>' + css +
            'body{font-family:-apple-system,"Segoe UI",Helvetica,Arial,sans-serif;margin:0;padding:0;background:#fff;color:#1f2328;}' +
            'img{max-width:100%;height:auto;}' +
            '</style></head><body class="markdown-body">' +
            clone.innerHTML + '</body></html>';

        return libs.htmlDocx.asBlob(html);
    }

    function triggerDownload(blob, name) {
        var url = URL.createObjectURL(blob);
        var a = document.createElement('a');
        a.href = url;
        a.download = name;
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(function() { URL.revokeObjectURL(url); }, 0);
    }

    window.initDownloadMenu = function(split) {
        var caret = split.querySelector('.dl-caret');
        var menu = split.querySelector('.dl-menu');
        var docxItem = split.querySelector('[data-format="docx"]');
        var docxHint = docxItem && docxItem.querySelector('.dl-menu-hint');
        var content = document.getElementById(split.dataset.content);
        var fileName = split.dataset.fileName;
        var vendorBase = split.dataset.vendor;

        async function generateDocx() {
            if (docxItem.getAttribute('aria-disabled') === 'true') return;
            docxItem.setAttribute('aria-disabled', 'true');
            if (docxHint) { docxHint.textContent = ' — generating…'; docxHint.hidden = false; }
            try {
                var blob = await buildDocxBlob(content, fileName, vendorBase);
                triggerDownload(blob, fileName.replace(/\.md$/i, '') + '.docx');
                closeMenu();
            } catch (err) {
                console.error('[docx] export failed', err);
                alert('Could not generate DOCX. Please try again.');
            } finally {
                docxItem.setAttribute('aria-disabled', 'false');
                if (docxHint) docxHint.hidden = true;
            }
        }

        function menuItems() {
            return Array.prototype.slice.call(menu.querySelectorAll('.dl-menu-item'));
        }
        function openMenu() {
            menu.hidden = false;
            caret.setAttribute('aria-expanded', 'true');
            var items = menuItems();
            if (items.length) items[0].focus();
            document.addEventListener('click', onDocClick, true);
            document.addEventListener('keydown', onKeydown, true);
        }
        function closeMenu() {
            if (menu.hidden) return;
            menu.hidden = true;
            caret.setAttribute('aria-expanded', 'false');
            document.removeEventListener('click', onDocClick, true);
            document.removeEventListener('keydown', onKeydown, true);
        }
        function onDocClick(e) {
            if (!split.contains(e.target)) closeMenu();
        }
        function onKeydown(e) {
            var items = menuItems();
            var idx = items.indexOf(document.activeElement);
            if (e.key === 'Escape') {
                e.preventDefault(); closeMenu(); caret.focus();
            } else if (e.key === 'ArrowDown') {
                e.preventDefault(); items[(idx + 1) % items.length].focus();
            } else if (e.key === 'ArrowUp') {
                e.preventDefault(); items[(idx - 1 + items.length) % items.length].focus();
            } else if (e.key === 'Tab') {
                closeMenu();
            }
        }

        caret.addEventListener('click', function(e) {
            e.stopPropagation();
            menu.hidden ? openMenu() : closeMenu();
        });
        menuItems().forEach(function(item) {
            item.addEventListener('click', item === docxItem ? generateDocx : closeMenu);
        });

        // Reveal the whole control immediately — ".md" needs no rendering. Gate only the
        // DOCX item until mermaid (if any) has finished. On diagram-free docs this
        // resolves on the same tick, so the disabled state is never seen.
        split.classList.add('ready');
        waitForMermaid(content).then(function() {
            docxItem.setAttribute('aria-disabled', 'false');
            if (docxHint) docxHint.hidden = true;
        });
    };
})();

// peekm.dev has no peekm server behind it. This shim keeps the real app JS
// working on a static page: SSE stays quiet, a reply answers from the
// pre-rendered fork in <template id="demo-branch">, and every other
// navigation shows a toast instead of a 404.
(function () {
    function QuietEventSource() {
        var es = this;
        es.readyState = 1;
        setTimeout(function () { if (es.onopen) es.onopen(); }, 0);
    }
    QuietEventSource.CONNECTING = 0;
    QuietEventSource.OPEN = 1;
    QuietEventSource.CLOSED = 2;
    QuietEventSource.prototype.close = function () { this.readyState = 2; };
    QuietEventSource.prototype.addEventListener = function () {};
    window.EventSource = QuietEventSource;

    // The site is light-only; a visitor's own toggle still wins.
    if (!localStorage.getItem('theme')) localStorage.setItem('theme', 'light');

    function branchTemplate() { return document.getElementById('demo-branch'); }
    function isBranchURL(url) { return String(url).indexOf('session=' + branchTemplate().dataset.session) !== -1; }
    function lastQuestion(root) {
        var texts = root.querySelectorAll('.transcript-user .transcript-text');
        if (!texts.length) return null;
        var last = texts[texts.length - 1];
        return last.querySelector('p') || last;
    }

    var question = '';
    window.fetch = function (url, opts) {
        if (String(url) === '/transcript/reply') {
            question = JSON.parse(opts.body).text;
            var body = JSON.stringify({ branch: branchTemplate().dataset.session });
            return new Promise(function (resolve) {
                setTimeout(function () { resolve(new Response(body, { headers: { 'Content-Type': 'application/json' } })); }, 1200);
            });
        }
        if (isBranchURL(url)) {
            var box = document.createElement('div');
            box.appendChild(branchTemplate().content.cloneNode(true));
            var asked = lastQuestion(box);
            if (question && asked) asked.textContent = question;
            return Promise.resolve(new Response(box.innerHTML, { headers: { 'Content-Type': 'text/html' } }));
        }
        return Promise.resolve(new Response('', { status: 404 }));
    };

    document.addEventListener('DOMContentLoaded', function () {
        var navigate = window.navigate;
        window.navigate = function (url, addToHistory) {
            if (isBranchURL(url)) return navigate(url, addToHistory);
            if (String(url).split('?')[0] === location.pathname) { location.assign(url); return Promise.resolve(); }
            showToast('This demo shows one session. Run npx @peekm/peekm . to browse your own.');
            return Promise.resolve();
        };

        var input = document.getElementById('reply-input');
        var asked = lastQuestion(branchTemplate().content);
        if (input && asked) input.value = asked.textContent.trim();
    });
})();

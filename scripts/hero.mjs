// Screenshot the demo transcript page for the README and peekm.dev (make hero).
// Usage: node scripts/hero.mjs <page.html> <out.png>
// Headless Chrome over CDP; every request is answered from disk, so nothing
// leaves the machine. Set CHROME to the browser binary outside macOS.
import { spawn } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const [page, out] = process.argv.slice(2);
const chrome = process.env.CHROME ?? '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const profile = mkdtempSync(join(tmpdir(), 'peekm-hero-'));
const browser = spawn(chrome, ['--headless=new', '--remote-debugging-port=0', `--user-data-dir=${profile}`, 'about:blank'], { stdio: 'ignore' });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

try {
  const portFile = join(profile, 'DevToolsActivePort');
  while (!existsSync(portFile)) await sleep(100);
  const port = readFileSync(portFile, 'utf8').split('\n')[0];
  const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
  const ws = new WebSocket(targets.find((t) => t.type === 'page').webSocketDebuggerUrl);
  await new Promise((r) => (ws.onopen = r));

  let id = 0;
  const pending = new Map();
  const send = (method, params = {}) => new Promise((r) => { pending.set(++id, r); ws.send(JSON.stringify({ id, method, params })); });
  const html = readFileSync(page);
  ws.onmessage = ({ data }) => {
    const msg = JSON.parse(data);
    if (pending.has(msg.id)) { pending.get(msg.id)(msg.result); pending.delete(msg.id); }
    if (msg.method !== 'Fetch.requestPaused') return;
    const { pathname } = new URL(msg.params.request.url);
    const font = pathname.match(/^\/assets\/fonts\/([\w-]+\.woff2)$/);
    const [body, type] = pathname === '/transcript' ? [html, 'text/html; charset=utf-8']
      : font ? [readFileSync(join('theme', 'fonts', font[1])), 'font/woff2'] : [Buffer.alloc(0), 'text/plain'];
    send('Fetch.fulfillRequest', {
      requestId: msg.params.requestId, responseCode: body.length ? 200 : 404,
      responseHeaders: [{ name: 'Content-Type', value: type }], body: body.toString('base64'),
    });
  };

  await send('Page.enable');
  await send('Fetch.enable', { patterns: [{ urlPattern: '*' }] });
  // No server behind the page: report the live-reload stream as connected.
  await send('Page.addScriptToEvaluateOnNewDocument', {
    source: `window.EventSource = class { static CLOSED = 2; readyState = 1;
      constructor() { setTimeout(() => this.onopen?.()); } addEventListener() {} close() { this.readyState = 2; } };`,
  });
  await send('Emulation.setDeviceMetricsOverride', { width: 1200, height: 1180, deviceScaleFactor: 2, mobile: false });
  await send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: 'light' }] });
  await send('Page.navigate', { url: 'http://peekm.localhost/transcript' });
  await sleep(1500);
  const opened = await send('Runtime.evaluate', {
    awaitPromise: true, returnByValue: true,
    expression: `document.fonts.ready.then(() => {
      const edit = [...document.querySelectorAll('#content details')].find((d) => d.querySelector('summary').innerText.includes('Edit'));
      if (edit) edit.open = true;
      return !!edit;
    })`,
  });
  if (!opened.result.value) throw new Error('no Edit tool call on the page');
  await sleep(300);
  const shot = await send('Page.captureScreenshot', { format: 'png' });
  writeFileSync(out, Buffer.from(shot.data, 'base64'));
  ws.close();
} finally {
  browser.kill();
  await sleep(300);
  rmSync(profile, { recursive: true, force: true });
}

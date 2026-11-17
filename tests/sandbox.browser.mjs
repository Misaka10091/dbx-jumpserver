// Reproduce DBX's allow-scripts iframe in a real Chromium browser.
// All bridge data is synthetic; no JumpServer requests or DBX URLs are opened.
import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { randomUUID } from 'node:crypto';

const root = resolve(import.meta.dirname, '..');
const browser = [
  process.env.DBX_TEST_BROWSER,
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser',
].find((candidate) => candidate && existsSync(candidate));
assert.ok(browser, 'Set DBX_TEST_BROWSER to an installed Chrome or Edge executable.');
const directory = join(root, '.cache', 'sandbox-browser', randomUUID());
await mkdir(directory, { recursive: true });
const escapeScript = (source) => source.replaceAll('</script', '<\\/script');
const inline = (source) => '<script>' + escapeScript(source) + '</script>';

const baseline = '<!doctype html><form id="legacy-form"><button id="legacy-fill" type="submit">Fill</button></form>' + inline(`
  let submitEvents = 0;
  document.getElementById('legacy-form').addEventListener('submit', event => { event.preventDefault(); submitEvents++; });
  document.getElementById('legacy-fill').click();
  parent.postMessage({ kind: 'baseline', submitEvents }, '*');
`);
const stub = `
  window.fillCalls = 0;
  window.dbxPlugin = {
    ready: Promise.resolve(), context: { connectionId: 'test-service' },
    capabilities: { storage: true, connectionsApi: true },
    storage: {
      async get() { return { orgId: 'org-1', kind: 'application', resourceId: 'db-1', accountId: 'account-1', targetId: 'target-1', database: '', updateEndpoint: false }; },
      async set() {},
    },
    async getConnections() { return [{ id: 'target-1', name: 'Test connection', dbType: 'mysql' }]; },
    onContext() {},
    async invoke(method) {
      if (method === 'jumpserver/profile') return { username: 'test-user', organizations: [{ id: 'org-1', name: 'Test organization' }], expireMinutes: 60 };
      if (method === 'jumpserver/resources') return { items: [{ id: 'db-1', name: 'Test database', protocol: 'mysql' }] };
      if (method === 'jumpserver/accounts') return { items: [{ id: 'account-1', name: 'Test account', protocol: 'mysql' }] };
      if (method === 'jumpserver/fill') { window.fillCalls++; return { opened: true, expiresAt: '2030-01-01T00:00:00Z', message: 'DBX test request accepted' }; }
      throw new Error('Unexpected test method');
    },
  };
`;
const probe = `
  (async () => {
    async function waitFor(predicate) {
      for (let i = 0; i < 200; i++) { if (predicate()) return; await new Promise(resolve => setTimeout(resolve, 10)); }
      throw new Error('Sandbox UI timed out');
    }
    try {
      await waitFor(() => document.getElementById('form').getAttribute('aria-busy') === 'false');
      const button = document.getElementById('fill');
      if (button.disabled) throw new Error('Fill button unexpectedly disabled');
      let submitEvents = 0;
      document.getElementById('form').addEventListener('submit', () => { submitEvents++; });
      button.click();
      const immediateFeedback = button.textContent === '正在填入…';
      await waitFor(() => window.fillCalls === 1 && document.getElementById('form').getAttribute('aria-busy') === 'false');
      const feedback = document.getElementById('action-status');
      parent.postMessage({ kind: 'fixed', fillCalls: window.fillCalls, submitEvents, immediateFeedback, feedbackVisible: !feedback.hidden, feedbackTone: feedback.dataset.tone, feedbackText: feedback.textContent }, '*');
    } catch (error) { parent.postMessage({ kind: 'fixed', error: error.message }, '*'); }
  })();
`;
let ui = await readFile(join(root, 'ui/index.html'), 'utf8');
ui = ui.replace('<link rel="stylesheet" href="./style.css">', '');
ui = ui.replace('<script src="./app.js"></script>', inline(stub) + inline(await readFile(join(root, 'ui/app.js'), 'utf8')) + inline(probe));
const controller = `
  const results = {};
  window.addEventListener('message', event => {
    if (!['baseline', 'fixed'].includes(event.data?.kind)) return;
    results[event.data.kind] = event.data;
    if (results.baseline && results.fixed) document.getElementById('sandbox-test-result').textContent = JSON.stringify(results);
  });
  const baseline = document.createElement('iframe');
  baseline.setAttribute('sandbox', 'allow-scripts'); baseline.srcdoc = ${JSON.stringify(baseline)};
  const fixed = document.createElement('iframe');
  fixed.setAttribute('sandbox', 'allow-scripts'); fixed.srcdoc = ${JSON.stringify(ui)};
  document.body.append(baseline, fixed);
`;
const html = '<!doctype html><html><body><pre id="sandbox-test-result">pending</pre>' + inline(controller) + '</body></html>';
const entry = join(directory, 'sandbox.html');
await writeFile(entry, html, 'utf8');
const result = spawnSync(browser, [
  '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
  '--disable-background-networking', '--disable-component-update', '--disable-sync',
  '--user-data-dir=' + join(directory, 'profile'), '--dump-dom', '--virtual-time-budget=5000',
  pathToFileURL(entry).href,
], { encoding: 'utf8', timeout: 30000, maxBuffer: 2 * 1024 * 1024, windowsHide: true });
assert.ifError(result.error);
assert.equal(result.status, 0, 'Headless browser failed: ' + result.stderr);
await writeFile(join(directory, 'browser-dom.html'), result.stdout, 'utf8');
const capture = result.stdout.match(/<pre id="sandbox-test-result">([^<]*)<\/pre>/);
assert.ok(capture && capture[1] !== 'pending', 'Browser did not report both sandbox results.');
const results = JSON.parse(capture[1]);
assert.equal(results.baseline.submitEvents, 0, 'Expected native form submission to be blocked without allow-forms.');
assert.equal(results.fixed.error, undefined, results.fixed.error);
assert.equal(results.fixed.fillCalls, 1);
assert.equal(results.fixed.submitEvents, 0);
assert.equal(results.fixed.immediateFeedback, true);
assert.equal(results.fixed.feedbackVisible, true);
assert.equal(results.fixed.feedbackTone, 'success');
assert.match(results.fixed.feedbackText, /DBX test request accepted/);
console.log('PASS: real browser blocks legacy submit; fixed click invokes fill once and displays progress and success inside sandbox="allow-scripts".');

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { JSDOM } from 'jsdom';

const html = await readFile(new URL('../ui/index.html', import.meta.url), 'utf8');
const script = await readFile(new URL('../ui/app.js', import.meta.url), 'utf8');
const manifest = JSON.parse(await readFile(new URL('../manifest.json', import.meta.url), 'utf8'));
const turn = () => new Promise((resolve) => setTimeout(resolve, 0));
async function waitFor(predicate) {
  for (let i = 0; i < 200; i++) {
    if (predicate()) return;
    await turn();
  }
  throw new Error('UI condition did not settle');
}
const profileStore = (store, connectionId = 'service-1') => store.get(`profiles:v2:${connectionId}`);
async function settled(dom) {
  for (let i = 0; i < 100; i++) {
    await turn();
    if (dom.window.document.getElementById('form').getAttribute('aria-busy') === 'false') return;
  }
  throw new Error('UI did not finish loading');
}
const remembered = { orgId: 'org-1', kind: 'application', resourceId: 'db-1', accountId: 'account-1', targetId: 'target-1', targetType: 'mysql', database: 'orders', updateEndpoint: false };
async function mount({ store = new Map(), connectionId = 'service-1', resourceMissing = false, accountMissing = false, fail, holdFill } = {}) {
  const dom = new JSDOM(html, { runScripts: 'outside-only' });
  const calls = [];
  const bridge = {
    ready: Promise.resolve(), context: { connectionId },
    capabilities: { storage: true, connectionsApi: true, clipboardWrite: true },
    storage: { get: async (key) => store.get(key) ?? null, set: async (key, value) => { store.set(key, structuredClone(value)); } },
    getConnections: async () => [{ id: 'target-1', name: 'Saved MySQL', dbType: 'mysql' }, { id: 'pg', name: 'Postgres', dbType: 'postgres' }],
    clipboard: { writeText: async () => {} },
    onContext(fn) { this.contextHandler = fn; },
    async invoke(method, params) {
      calls.push({ method, params });
      if (fail === method) throw new Error('服务暂时不可用');
      if (method === 'jumpserver/profile') return { username: 'alice', organizations: [{ id: 'org-1', name: '研发组织' }, { id: 'org-2', name: '其他组织' }], expireMinutes: 60 };
      if (method === 'jumpserver/resources') return { items: resourceMissing ? [] : [{ id: 'db-1', name: '<img src=x onerror=alert(1)>', protocol: 'mysql' }] };
      if (method === 'jumpserver/accounts') return { items: accountMissing ? [] : [{ id: 'account-1', name: 'Read only', username: 'readonly', protocol: 'mysql' }] };
      if (method === 'jumpserver/fill') { if (holdFill) await holdFill; return { opened: true, expiresAt: '2030-01-01T00:00:00Z', message: '已请求打开 DBX 连接表单' }; }
      if (method === 'jumpserver/credentials') return { host: 'proxy.test', port: 33061, username: 'token-id-for-test', password: 'temporary-password', database: 'orders', expiresAt: '2030-01-01T00:00:00Z' };
      throw new Error(`Unexpected method: ${method}`);
    },
  };
  dom.window.dbxPlugin = bridge;
  dom.window.eval(script);
  await settled(dom);
  return { dom, store, calls, bridge, $: (id) => dom.window.document.getElementById(id), change(id, value) { const el = dom.window.document.getElementById(id); el.value = value; el.dispatchEvent(new dom.window.Event('change', { bubbles: true })); } };
}
async function saveAs(ui, name) {
  ui.$('profile-new').click();
  assert.equal(ui.$('profile-dialog').hidden, false);
  ui.$('profile-name').value = name;
  ui.$('profile-dialog-confirm').click();
  await waitFor(() => ui.$('profile-dialog').hidden);
}
function selectProfile(ui, name) {
  const option = [...ui.$('profile-select').options].find((item) => item.textContent === name);
  assert.ok(option, `missing profile ${name}`);
  ui.change('profile-select', option.value);
  return settled(ui.dom);
}
async function chooseDatabase(ui) {
  ui.change('org', 'org-1'); await settled(ui.dom);
  ui.change('resource', 'db-1'); await settled(ui.dom);
}

test('restores the last full selection and enables one-click fill', async (t) => {
  const ui = await mount({ store: new Map([['binding:service-1', remembered]]) }); t.after(() => ui.dom.window.close());
  for (const [field, expected] of [['org', 'org-1'], ['resource', 'db-1'], ['account', 'account-1'], ['target', 'target-1'], ['database', 'orders']]) assert.equal(ui.$(field).value, expected);
  assert.equal(ui.$('fill').disabled, false);
  assert.equal(ui.$('target').options.length, 2, 'other database protocols should not be offered');
  assert.match(ui.$('status').textContent, /迁移为配置「默认配置」/);
  assert.equal(ui.$('profile-select').selectedOptions[0].textContent, '默认配置');
  assert.equal(ui.dom.window.document.querySelector('option img'), null, 'server names must render as text');
});

test('persists selections before minting and restores them after reopening', async (t) => {
  const store = new Map(); const ui = await mount({ store }); t.after(() => ui.dom.window.close());
  ui.change('org', 'org-1'); await settled(ui.dom);
  ui.change('resource', 'db-1'); await settled(ui.dom);
  ui.change('target', 'target-1'); ui.change('database', 'billing'); await turn();
  assert.equal(profileStore(store).profiles[0].selection.accountId, 'account-1');
  assert.equal(ui.calls.some((call) => call.method === 'jumpserver/fill'), false);
  const reopened = await mount({ store }); t.after(() => reopened.dom.window.close());
  assert.equal(reopened.$('target').value, 'target-1'); assert.equal(reopened.$('database').value, 'billing');
  assert.equal(reopened.$('fill').disabled, false);
});

test('isolates remembered choices by JumpServer service connection', async (t) => {
  const ui = await mount({ store: new Map([['binding:service-1', remembered]]), connectionId: 'service-2' }); t.after(() => ui.dom.window.close());
  assert.equal(ui.$('org').value, ''); assert.equal(ui.$('resource').value, ''); assert.equal(ui.$('fill').disabled, true);
});

for (const missing of ['resourceMissing', 'accountMissing']) test(`does not reuse stale authorization: ${missing}`, async (t) => {
  const ui = await mount({ store: new Map([['binding:service-1', remembered]]), [missing]: true }); t.after(() => ui.dom.window.close());
  assert.equal(ui.$('fill').disabled, true);
  assert.equal(ui.$('account').value, '');
});

test('organization changes clear stale resource/account/target choices', async (t) => {
  const ui = await mount({ store: new Map([['binding:service-1', remembered]]) }); t.after(() => ui.dom.window.close());
  ui.change('org', 'org-2'); await settled(ui.dom);
  assert.equal(ui.$('resource').value, ''); assert.equal(ui.$('account').value, ''); assert.equal(ui.$('target').value, ''); assert.equal(ui.$('fill').disabled, true);
  assert.equal(profileStore(ui.store).profiles.find((profile) => profile.id === profileStore(ui.store).activeId).selection.resourceId, '');
});

test('fill rejects duplicate clicks and persists no temporary password', async (t) => {
  let release; const gate = new Promise((resolve) => { release = resolve; });
  const ui = await mount({ store: new Map([['binding:service-1', remembered]]), holdFill: gate }); t.after(() => ui.dom.window.close());
  ui.$('fill').click(); ui.$('fill').click(); await turn();
  assert.equal(ui.calls.filter((c) => c.method === 'jumpserver/fill').length, 1); assert.equal(ui.$('fill').disabled, true);
  assert.equal(ui.$('fill').textContent, '正在填入…');
  assert.equal(ui.$('action-status').hidden, false);
  assert.match(ui.$('action-status').textContent, /正在创建临时凭证/);
  release(); await settled(ui.dom); assert.equal(ui.$('fill').disabled, false);
  assert.match(ui.$('status').textContent, /已请求打开 DBX/);
  assert.equal(JSON.stringify([...ui.store.values()]).includes('password'), false);
});

test('API errors are visible and allow retry', async (t) => {
  const ui = await mount({ store: new Map([['binding:service-1', remembered]]), fail: 'jumpserver/fill' }); t.after(() => ui.dom.window.close());
  ui.$('fill').click(); await settled(ui.dom);
  assert.equal(ui.$('status').dataset.tone, 'error'); assert.match(ui.$('status').textContent, /服务暂时不可用/); assert.equal(ui.$('fill').disabled, false);
  assert.equal(ui.$('action-status').hidden, false); assert.equal(ui.$('action-status').dataset.tone, 'error');
  assert.match(ui.$('action-status').textContent, /服务暂时不可用/);
  assert.equal(ui.$('fill').textContent, '刷新并填入 DBX');
});

test('fill uses click without relying on a native submit event', async (t) => {
  const ui = await mount({ store: new Map([['binding:service-1', remembered]]) }); t.after(() => ui.dom.window.close());
  assert.equal(ui.$('fill').type, 'button');
  let submitEvents = 0;
  ui.$('form').addEventListener('submit', () => { submitEvents++; });
  ui.$('fill').click(); await settled(ui.dom);
  assert.equal(submitEvents, 0);
  assert.equal(ui.calls.filter((call) => call.method === 'jumpserver/fill').length, 1);
  assert.equal(ui.$('action-status').dataset.tone, 'success');
});

test('manual credentials stay ephemeral and can be cleared', async (t) => {
  const ui = await mount({ store: new Map([['binding:service-1', remembered]]) }); t.after(() => ui.dom.window.close());
  ui.$('preview').click(); await settled(ui.dom);
  assert.equal(ui.$('value-username').value, 'token-id-for-test');
  assert.equal(ui.$('value-password').type, 'password'); assert.equal(ui.$('value-password').value, 'temporary-password');
  assert.equal(JSON.stringify([...ui.store.values()]).includes('temporary-password'), false);
  ui.$('clear-preview').click(); assert.equal(ui.$('preview-panel').hidden, true); assert.equal(ui.$('preview-values').children.length, 0);
});

test('new connections fill without manual gateway configuration', async (t) => {
  const fields = manifest.contributions.find((item) => item.type === 'connection-provider').fields;
  assert.equal(fields.some((field) => ['gateway_host', 'gateway_port', 'username_template'].includes(field.key)), false);
  const ui = await mount({ store: new Map([['binding:service-1', { ...remembered, targetId: '', targetType: '' }]]) }); t.after(() => ui.dom.window.close());
  assert.equal(ui.$('fill').disabled, false);
  assert.equal(ui.$('update-endpoint-row').hidden, true);
  assert.match(ui.$('target-hint').textContent, /自动获取主机和端口/);
  ui.$('fill').click(); await settled(ui.dom);
  assert.equal(ui.calls.find((call) => call.method === 'jumpserver/fill').params.targetId, '');
});

test('restores the automatic endpoint update choice along with the target', async (t) => {
  const ui = await mount({ store: new Map([['binding:service-1', { ...remembered, updateEndpoint: true }]]) }); t.after(() => ui.dom.window.close());
  assert.equal(ui.$('update-endpoint').checked, true);
  assert.equal(ui.$('update-endpoint-row').hidden, false);
  assert.match(ui.$('target-hint').textContent, /smart/);
  ui.$('fill').click(); await settled(ui.dom);
  assert.equal(ui.calls.find((call) => call.method === 'jumpserver/fill').params.updateEndpoint, true);
});

test('one JumpServer keeps multiple independent named profiles', async (t) => {
  const store = new Map();
  const ui = await mount({ store }); t.after(() => ui.dom.window.close());
  await chooseDatabase(ui);
  ui.change('target', 'target-1'); ui.change('database', 'orders'); await turn();
  await saveAs(ui, '生产订单');
  assert.equal(ui.$('profile-select').selectedOptions[0].textContent, '生产订单');

  await saveAs(ui, '测试库');
  assert.equal(ui.$('profile-select').selectedOptions[0].textContent, '测试库');
  ui.change('target', ''); ui.change('database', 'billing'); await turn();

  const saved = profileStore(store);
  assert.equal(saved.profiles.length, 2);
  const production = saved.profiles.find((profile) => profile.name === '生产订单');
  const testing = saved.profiles.find((profile) => profile.name === '测试库');
  assert.equal(production.selection.targetId, 'target-1');
  assert.equal(production.selection.database, 'orders');
  assert.equal(testing.selection.targetId, '');
  assert.equal(testing.selection.database, 'billing');

  await selectProfile(ui, '生产订单');
  assert.equal(ui.$('target').value, 'target-1');
  assert.equal(ui.$('database').value, 'orders');
  assert.equal(ui.calls.filter((call) => call.method === 'jumpserver/fill').length, 0, 'switching profiles must not mint tokens');

  await selectProfile(ui, '测试库');
  assert.equal(ui.$('target').value, '');
  assert.equal(ui.$('database').value, 'billing');
});

test('last used profile survives reopening and profiles remain isolated per service', async (t) => {
  const store = new Map();
  const first = await mount({ store }); t.after(() => first.dom.window.close());
  await chooseDatabase(first);
  first.change('target', 'target-1'); first.change('database', 'orders'); await turn();
  await saveAs(first, '生产订单');
  await saveAs(first, '测试库');
  first.change('database', 'billing'); await turn();
  first.dom.window.close();

  const reopened = await mount({ store }); t.after(() => reopened.dom.window.close());
  assert.equal(reopened.$('profile-select').selectedOptions[0].textContent, '测试库');
  assert.equal(reopened.$('database').value, 'billing');
  assert.equal(profileStore(store).profiles.length, 2);

  const otherService = await mount({ store, connectionId: 'service-2' }); t.after(() => otherService.dom.window.close());
  assert.equal(otherService.$('profile-count').textContent, '1 个配置 · 修改自动保存');
  assert.notEqual(profileStore(store, 'service-2').profiles[0].name, '测试库');
});

test('rename and delete update the profile list without touching DBX connections', async (t) => {
  const store = new Map();
  const ui = await mount({ store }); t.after(() => ui.dom.window.close());
  await chooseDatabase(ui);
  ui.change('target', 'target-1'); ui.change('database', 'orders'); await turn();
  await saveAs(ui, '生产订单');
  await saveAs(ui, '测试库');

  ui.$('profile-rename').click();
  ui.$('profile-name').value = '测试库（只读）';
  ui.$('profile-dialog-confirm').click();
  await waitFor(() => ui.$('profile-dialog').hidden);
  assert.equal(profileStore(store).profiles.some((profile) => profile.name === '测试库（只读）'), true);

  ui.$('profile-delete').click();
  assert.match(ui.$('profile-dialog-copy').textContent, /不会删除或修改 DBX 中的数据库连接/);
  ui.$('profile-dialog-confirm').click();
  await waitFor(() => ui.$('profile-dialog').hidden);
  await settled(ui.dom);
  assert.equal(profileStore(store).profiles.length, 1);
  assert.equal(profileStore(store).profiles[0].name, '生产订单');
  assert.equal(ui.$('target').value, 'target-1');
  assert.equal(ui.$('profile-delete').disabled, true, 'the last profile must be protected');
});

test('duplicate profile names are rejected inline', async (t) => {
  const ui = await mount({ store: new Map() }); t.after(() => ui.dom.window.close());
  await chooseDatabase(ui);
  ui.change('target', 'target-1'); await turn();
  await saveAs(ui, '生产订单');
  ui.$('profile-new').click();
  ui.$('profile-name').value = '生产订单';
  ui.$('profile-dialog-confirm').click();
  await turn();
  assert.equal(ui.$('profile-dialog').hidden, false);
  assert.equal(ui.$('profile-dialog-error').hidden, false);
  assert.match(ui.$('profile-dialog-error').textContent, /同名配置/);
  assert.equal(profileStore(ui.store).profiles.length, 1);
});

test('missing authorization in a saved profile is reported without being reused', async (t) => {
  const store = new Map([['profiles:v2:service-1', {
    version: 2, activeId: 'stale-profile',
    profiles: [{ id: 'stale-profile', name: '旧生产库', selection: remembered }],
  }]]);
  const ui = await mount({ store, accountMissing: true }); t.after(() => ui.dom.window.close());
  assert.equal(ui.$('account').value, '');
  assert.equal(ui.$('fill').disabled, true);
  assert.match(ui.$('status').textContent, /原系统账号已不可用/);
});

test('context changes invalidate the loaded service and disable fill', async (t) => {
  const ui = await mount({ store: new Map([['binding:service-1', remembered]]) }); t.after(() => ui.dom.window.close());
  ui.bridge.context = { connectionId: 'service-2' }; ui.bridge.contextHandler();
  assert.equal(ui.$('fill').disabled, true); assert.match(ui.$('status').textContent, /上下文已更改/);
});

test('switches terminal and file tabs and uses DBX file dialogs for both transfers', async (t) => {
  const dom = new JSDOM(html, { runScripts: 'outside-only' });
  t.after(() => dom.window.close());
  const calls = [];
  const saves = [];
  const $ = (id) => dom.window.document.getElementById(id);
  $('ssh-file-delete-dialog').showModal = function () { this.open = true; };
  $('ssh-file-delete-dialog').close = function () { this.open = false; this.dispatchEvent(new dom.window.Event('close')); };
  dom.window.Terminal = function () {};
  dom.window.dbxPlugin = {
    ready: Promise.resolve(), context: { connectionId: 'connection' },
    capabilities: { fileTransfer: { pick: true, read: true, beginSave: true } },
    fileTransfer: {
      async pick({ multiple }) {
        assert.equal(multiple, true);
        return { files: [{ handleId: 'local-in', name: 'upload.bin', size: 2 }, { handleId: 'local-in-2', name: 'upload2.bin', size: 2 }] };
      },
      async read(handle, offset, maximum) {
        assert.ok(['local-in', 'local-in-2'].includes(handle));
        assert.deepEqual([offset, maximum], [0, 512 * 1024]);
        return { dataBase64: 'YWI=' };
      },
      async beginSave({ name, size }) {
        assert.equal(size, 4);
        saves.push(name);
        return { handleId: 'local-out' };
      },
      async write(handle, offset, data) {
        assert.deepEqual([handle, offset, data], ['local-out', 0, 'dGVzdA==']);
        return { nextOffset: 4 };
      },
      async finish(handle) { assert.equal(handle, 'local-out'); },
      async cancel(handle) { assert.ok(['local-in', 'local-in-2'].includes(handle)); },
    },
    async invoke(method, params) {
      calls.push({ method, params });
      switch (method) {
        case 'jumpserver/profile': return { organizations: [{ id: 'org' }] };
        case 'jumpserver/ssh/assets': return { items: [{ id: 'asset' }] };
        case 'jumpserver/ssh/accounts': return { items: [{ id: 'account' }] };
        case 'jumpserver/ssh/files/list':
          return { sftpRoot: '/tmp', resolvedPath: '/tmp', items: [{ name: 'fixture.jar', size: '4', is_dir: false }, { name: 'other.jar', size: '4', is_dir: false }, { name: 'tmp-folder', is_dir: true }] };
        case 'jumpserver/ssh/files/prepare-download':
          assert.ok(['/tmp/fixture.jar', '/tmp/other.jar'].includes(params.remotePath));
          return { transferId: 'download', size: 4 };
        case 'jumpserver/ssh/files/read-download':
          return { data: 'dGVzdA==', nextOffset: 4 };
        case 'jumpserver/ssh/files/prepare-upload':
          assert.ok(['/tmp/upload.bin', '/tmp/upload2.bin'].includes(params.remotePath));
          return { transferId: 'upload', size: 2 };
        case 'jumpserver/ssh/files/write-upload':
          assert.equal(params.data, 'YWI=');
          return { nextOffset: 2 };
        case 'jumpserver/ssh/files/delete':
          assert.equal(params.remotePath, '/tmp');
          assert.equal(params.entries.length, 3);
          assert.ok(params.entries.some((entry) => entry.name === 'tmp-folder' && entry.isDir));
          return { items: [{ name: 'fixture.jar' }, { name: 'other.jar', error: '权限不足' }, { name: 'tmp-folder' }] };
        case 'jumpserver/ssh/files/close-transfer':
        case 'jumpserver/ssh/files/commit-upload':
          return {};
        default: throw new Error(method);
      }
    },
  };
  for (const path of ['files.js', 'ssh.js']) {
    dom.window.eval(await readFile(new URL('../ui/' + path, import.meta.url), 'utf8'));
  }
  $('show-ssh').click();
  await waitFor(() => !$('ssh-open').disabled);
  $('ssh-tab-files').click();
  await waitFor(() => $('ssh-file-list').rows.length === 3 && !$('ssh-file-upload').disabled);
  assert.equal($('ssh-terminal-panel').hidden, true);
  assert.equal($('ssh-file-path').value, '/tmp');
  assert.equal($('ssh-file-up').disabled, true);
  $('ssh-file-filter').value = '.jar';
  $('ssh-file-filter').dispatchEvent(new dom.window.Event('input'));
  $('ssh-file-select-all').click();
  assert.equal($('ssh-file-selection').textContent, '已选 2 项');
  $('ssh-file-download').click();
  await waitFor(() => $('ssh-file-status').textContent.includes('下载完成 2 项') && !$('ssh-file-upload').disabled);
  $('ssh-file-upload').click();
  await waitFor(() => $('ssh-file-status').textContent.includes('上传完成 2 项') && !$('ssh-file-upload').disabled);
  assert.deepEqual(saves, ['fixture.jar', 'other.jar']);
  $('ssh-file-select-all').click();
  assert.equal($('ssh-file-download').disabled, true);
  $('ssh-file-delete').click();
  assert.match($('ssh-file-delete-copy').textContent, /文件夹及其内容/);
  $('ssh-file-delete-cancel').click();
  assert.ok(calls.every((call) => !call.method.endsWith('/delete')));
  $('ssh-file-delete').click();
  $('ssh-file-delete-confirm').click();
  await waitFor(() => $('ssh-file-status').textContent.includes('删除完成 2 项，失败 1 项'));
  $('ssh-file-path').value = '/data';
  $('ssh-file-go').click();
  await waitFor(() => $('ssh-file-status').textContent.includes('超出'));
  assert.ok(calls.every((call) => call.params?.remotePath !== '/data'));
  $('ssh-tab-terminal').click();
  assert.equal($('ssh-files-panel').hidden, true);
  assert.equal($('ssh-terminal-panel').hidden, false);
  assert.ok(calls.every((call) => !call.method.endsWith('/close')));
});

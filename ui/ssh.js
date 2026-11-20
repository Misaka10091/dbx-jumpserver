/* SSH credentials and the KoKo WebSocket stay in the DBX sidecar. */
(() => {
  const bridge = window.dbxPlugin;
  if (!bridge || typeof window.Terminal !== 'function') return;

  const $ = (id) => document.getElementById(id);
  const org = $('ssh-org');
  const asset = $('ssh-asset');
  const account = $('ssh-account');
  const screen = $('ssh-terminal');
  let connectionId = '';
  let active = null;
  let busy = false;
  let epoch = 0;
  const files = window.createJumpServerFiles?.({ bridge, invoke, selection, connection: () => connectionId, run, update });
  const tabs = [$('ssh-tab-terminal'), $('ssh-tab-files')];
  let selectedTab = 0;

  function status(message, tone = '') {
    $('ssh-status').textContent = message;
    $('ssh-status').dataset.tone = tone;
  }

  function options(select, placeholder, values = [], label = (item) => item.name || item.id) {
    select.replaceChildren(new Option(placeholder, ''));
    for (const item of values) select.add(new Option(label(item), item.id));
    if (values.length === 1) select.selectedIndex = 1;
  }

  function update() {
    org.disabled = busy || !!active;
    asset.disabled = busy || !!active || !org.value;
    account.disabled = busy || !!active || !asset.value;
    $('ssh-find').disabled = busy || !!active || !org.value;
    $('ssh-search').disabled = busy || !!active || !org.value;
    $('ssh-open').disabled = busy || !!active || !account.value;
    $('ssh-close').disabled = !active;
    files?.sync(busy, !!account.value);
  }

  function invoke(method, params, boundId = connectionId) {
    const timeoutMs = { open: 120000, 'files/list': 50000, 'files/delete': 50000, 'files/prepare-download': 250000, 'files/commit-upload': 50000 }[method] || 25000;
    return bridge.invoke(`jumpserver/ssh/${method}`, { ...params, connectionId: boundId }, { timeoutMs });
  }

  function selection() {
    return { orgId: org.value, assetId: asset.value, accountId: account.value, search: $('ssh-search').value };
  }

  async function run(action) {
    if (busy) return;
    busy = true;
    const started = epoch;
    update();
    try {
      await action();
    } catch (error) {
      if (started === epoch) status(error?.message || String(error), 'error');
    } finally {
      busy = false;
      update();
    }
  }

  async function loadAccounts() {
    files?.reset();
    options(account, '选择授权账号');
    if (!asset.value) return;
    const id = connectionId;
    status('正在加载授权账号…');
    const result = await invoke('accounts', selection(), id);
    if (id !== connectionId) return;
    options(account, result.items.length ? '选择授权账号' : '当前资产没有可用账号', result.items,
      (item) => item.username && item.username !== item.name ? `${item.name || item.id} (${item.username})` : item.name || item.id);
    status(result.items.length ? '选择账号后打开终端。' : '当前资产没有可用的托管 SSH 账号。');
  }

  async function loadAssets() {
    files?.reset();
    options(asset, '选择 SSH 资产');
    options(account, '先选择 SSH 资产');
    if (!org.value) return;
    const id = connectionId;
    status('正在加载 SSH 资产…');
    const result = await invoke('assets', selection(), id);
    if (id !== connectionId) return;
    options(asset, result.items.length ? '选择 SSH 资产' : '未找到 SSH 资产', result.items,
      (item) => item.address ? `${item.name || item.id} · ${item.address}` : item.name || item.id);
    status(result.items.length ? `找到 ${result.items.length} 个 SSH 资产。` : '未找到匹配的 SSH 资产。');
    if (asset.value) await loadAccounts();
  }

  async function loadOrganizations() {
    options(org, '选择组织');
    options(asset, '先选择组织');
    options(account, '先选择 SSH 资产');
    await bridge.ready;
    connectionId = bridge.context?.connectionId || '';
    const id = connectionId;
    if (!id) throw new Error('请先在 DBX 连接树中连接 JumpServer。');
    status('正在加载组织…');
    const result = await bridge.invoke('jumpserver/profile', { connectionId: id });
    if (id !== connectionId) return;
    options(org, '选择组织', result.organizations);
    status(result.organizations.length ? '选择组织后加载 SSH 资产。' : 'JumpServer 没有返回可用组织。');
    await loadAssets();
  }

  function closeTerminal(message = 'SSH 已断开。', tone = '') {
    const session = active;
    if (!session) return;
    active = null;
    session.resize.disconnect();
    session.term.options.disableStdin = true;
    void invoke('close', { terminalId: session.id }, session.connectionId).catch(() => {});
    status(message, tone);
    update();
  }

  function dimensions() {
    // Match CSS's 13px monospaced cell at line height 1.2.
    return {
      cols: Math.max(10, Math.min(500, Math.floor((screen.clientWidth - 20) / 8))),
      rows: Math.max(3, Math.min(250, Math.floor((screen.clientHeight - 20) / 16))),
    };
  }

  async function readTerminal(session) {
    try {
      while (active === session) {
        const result = await invoke('read', { terminalId: session.id }, session.connectionId);
        if (active !== session) return;
        if (result.closed) {
          closeTerminal('SSH 会话已结束。');
          return;
        }
        if (result.data) {
          const bytes = Uint8Array.from(atob(result.data), (char) => char.charCodeAt(0));
          await new Promise((resolve) => session.term.write(bytes, resolve));
        }
      }
    } catch (error) {
      if (active === session) closeTerminal(error?.message || String(error), 'error');
    }
  }

  let display = null;
  async function openTerminal() {
    const id = connectionId;
    const started = epoch;
    status('正在通过 JumpServer KoKo 打开终端…');
    display?.dispose();
    screen.hidden = false;
    const term = new window.Terminal({
      ...dimensions(), fontSize: 13, lineHeight: 1.2, cursorBlink: true, scrollback: 2500,
      fontFamily: 'Menlo, Consolas, monospace', theme: { background: '#141821', foreground: '#e5e9f0' },
    });
    display = term;
    term.open(screen);
    const result = await invoke('open', { ...selection(), cols: term.cols, rows: term.rows }, id);
    if (started !== epoch || id !== connectionId) {
      void invoke('close', { terminalId: result.terminalId }, id).catch(() => {});
      return;
    }
    const session = { id: result.terminalId, connectionId: id, term, pending: Promise.resolve(), resize: null };
    const send = (method, params) => {
      session.pending = session.pending.then(async () => {
        if (active === session) await invoke(method, { ...params, terminalId: session.id }, id);
      }).catch((error) => {
        if (active === session) closeTerminal(error?.message || String(error), 'error');
      });
    };
    session.resize = new ResizeObserver(() => {
      if (screen.offsetWidth === 0 || active !== session) return;
      const size = dimensions();
      if (size.cols === term.cols && size.rows === term.rows) return;
      term.resize(size.cols, size.rows);
      send('resize', size);
    });
    active = session;
    session.resize.observe(screen);
    term.onData((data) => {
      for (const chunk of data.match(/[\s\S]{1,1800}/gu) || []) send('write', { data: chunk });
    });
    status('已连接 KoKo，正在等待资产终端输出。', 'success');
    term.focus();
    void readTerminal(session);
  }

  function selectTab(index, focus = false) {
    selectedTab = index;
    for (const [i, tab] of tabs.entries()) {
      tab.setAttribute('aria-selected', String(i === index));
      tab.tabIndex = i === index ? 0 : -1;
      $(tab.getAttribute('aria-controls')).hidden = i !== index;
    }
    if (focus) tabs[index].focus();
    if (index === 1) void files?.open();
  }

  function switchPanel(ssh) {
    $('database-panel').hidden = ssh;
    $('reload').hidden = ssh;
    $('ssh-panel').hidden = !ssh;
    document.body.toggleAttribute('data-ssh-workspace', ssh);
    $('show-database').setAttribute('aria-pressed', String(!ssh));
    $('show-ssh').setAttribute('aria-pressed', String(ssh));
    if (ssh && !connectionId) void run(loadOrganizations);
  }

  options(org, '选择组织');
  options(asset, '先选择组织');
  options(account, '先选择 SSH 资产');
  $('show-database').addEventListener('click', () => switchPanel(false));
  $('show-ssh').addEventListener('click', () => switchPanel(true));
  $('ssh-find').addEventListener('click', () => run(loadAssets));
  org.addEventListener('change', () => run(loadAssets));
  asset.addEventListener('change', () => run(loadAccounts));
  account.addEventListener('change', () => {
    files?.reset();
    update();
    if (selectedTab === 1) void files?.open();
  });
  $('ssh-search').addEventListener('keydown', (event) => { if (event.key === 'Enter') void run(loadAssets); });
  $('ssh-open').addEventListener('click', () => run(openTerminal));
  $('ssh-close').addEventListener('click', () => closeTerminal());
  for (const [index, tab] of tabs.entries()) {
    tab.addEventListener('click', () => selectTab(index));
    tab.addEventListener('keydown', (event) => {
      const next = { ArrowLeft: 1 - index, ArrowRight: 1 - index, Home: 0, End: 1 }[event.key];
      if (next === undefined) return;
      event.preventDefault();
      selectTab(next, true);
    });
  }
  window.addEventListener('pagehide', () => closeTerminal());
  void bridge.ready.then(() => bridge.onContext?.((context) => {
    if ((context?.connectionId || bridge.context?.connectionId) === connectionId) return;
    epoch++;
    files?.reset();
    selectTab(0);
    closeTerminal();
    connectionId = '';
    display?.dispose();
    display = null;
    screen.hidden = true;
    if (!$('ssh-panel').hidden) void run(loadOrganizations);
  }));
  update();
})();

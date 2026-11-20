/* DBX injects window.dbxPlugin; no network or credentials are stored in this UI. */
(async function () {
  const $ = (id) => document.getElementById(id);
  const bridge = window.dbxPlugin;
  const PROFILE_STORE_VERSION = 2;
  const MAX_PROFILES = 50;
  const MAX_PROFILE_NAME = 64;
  const MAX_VALUE = 256;
  const EMPTY_SELECTION = { orgId: '', kind: 'application', resourceId: '', accountId: '', targetId: '', targetType: '', database: '', updateEndpoint: false };
  const state = {
    connectionId: '', busy: false, action: '', resources: [], accounts: [], targets: [],
    service: null, profiles: [], activeId: '', profileDialog: null, preview: null, epoch: 0, migrated: false,
  };
  function status(message, tone = '') {
    $('status').textContent = message; $('status').dataset.tone = tone;
    if (state.action) {
      $('action-status').textContent = message;
      $('action-status').dataset.tone = tone;
      $('action-status').hidden = false;
    }
  }
  function options(select, items, placeholder, label) {
    select.replaceChildren(new Option(placeholder, ''));
    for (const item of items) select.add(new Option(label(item), item.id));
  }
  function clearPreview() { state.preview = null; $('preview-values').replaceChildren(); $('preview-panel').hidden = true; }
  function readyToFill() { return !!(state.connectionId && $('org').value && $('resource').value && $('account').value); }
  function canFill() { return readyToFill() && (!$('target').value || bridge.capabilities?.connectionEditApi); }
  function updateButtons() {
    const hasService = !!state.service;
    $('fill').disabled = state.busy || !canFill();
    $('preview').disabled = state.busy || !readyToFill();
    $('fill').textContent = state.busy && state.action === 'fill' ? '正在填入…' : '刷新并填入 DBX';
    $('preview').textContent = state.busy && state.action === 'preview' ? '正在获取…' : '获取参数';
    $('reload').disabled = state.busy;
    $('fields').disabled = state.busy || !hasService;
    $('profile-select').disabled = state.busy || !hasService || !state.profiles.length;
    $('profile-new').disabled = state.busy || !hasService || !readyToFill() || state.profiles.length >= MAX_PROFILES;
    $('profile-rename').disabled = state.busy || !hasService || !activeProfile();
    $('profile-delete').disabled = state.busy || !hasService || state.profiles.length <= 1;
    $('form').setAttribute('aria-busy', String(state.busy));
  }
  async function run(action, loading, actionName = '') {
    if (state.busy) return;
    const epoch = state.epoch;
    state.action = actionName;
    if (!actionName) $('action-status').hidden = true;
    state.busy = true; updateButtons(); status(loading);
    try { await action(); } catch (error) { if (epoch === state.epoch) status(error?.message || String(error), 'error'); }
    finally { state.busy = false; state.action = ''; updateButtons(); }
  }
  async function invoke(method, params = {}) {
    const epoch = state.epoch;
    const value = await bridge.invoke(method, { ...params, connectionId: state.connectionId }, { timeoutMs: 120000 });
    if (epoch !== state.epoch) throw new Error('连接上下文已更改，请重新加载。');
    return value;
  }
  function selection() {
    const target = state.targets.find((item) => item.id === $('target').value);
    return { orgId: $('org').value, kind: $('kind').value, resourceId: $('resource').value, accountId: $('account').value,
      targetId: target?.id || '', targetType: target?.dbType || target?.dialect || '',
      database: $('database').value, updateEndpoint: $('update-endpoint').checked };
  }
  function bounded(value, max = MAX_VALUE) { return typeof value === 'string' ? value.slice(0, max) : ''; }
  function normalizeSelection(value) {
    const input = value && typeof value === 'object' ? value : {};
    return {
      orgId: bounded(input.orgId), kind: input.kind === 'asset' ? 'asset' : 'application',
      resourceId: bounded(input.resourceId), accountId: bounded(input.accountId),
      targetId: bounded(input.targetId), targetType: bounded(input.targetType, 32),
      database: bounded(input.database), updateEndpoint: !!input.updateEndpoint,
    };
  }
  function newProfileId() {
    if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID();
    return `profile-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
  }
  function profileStoreKey() { return `profiles:v${PROFILE_STORE_VERSION}:${state.connectionId}`; }
  function legacyKey() { return `binding:${state.connectionId}`; }
  function activeProfile() { return state.profiles.find((profile) => profile.id === state.activeId) || null; }
  function profileNameExists(name, excludeId = '') {
    const normalized = name.trim().toLocaleLowerCase();
    return state.profiles.some((profile) => profile.id !== excludeId && profile.name.toLocaleLowerCase() === normalized);
  }
  function uniqueProfileName(base, excludeId = '') {
    let candidate = bounded(base.trim(), MAX_PROFILE_NAME) || '未命名配置';
    if (!profileNameExists(candidate, excludeId)) return candidate;
    for (let index = 2; index < 1000; index++) {
      const suffix = ` (${index})`;
      candidate = bounded(base.trim(), MAX_PROFILE_NAME - suffix.length) + suffix;
      if (!profileNameExists(candidate, excludeId)) return candidate;
    }
    return bounded(`配置 ${Date.now()}`, MAX_PROFILE_NAME);
  }
  function normalizeProfile(raw) {
    if (!raw || typeof raw !== 'object') return null;
    const name = bounded(raw.name, MAX_PROFILE_NAME).trim();
    if (!name) return null;
    const id = typeof raw.id === 'string' && /^[A-Za-z0-9_-]{1,64}$/.test(raw.id) ? raw.id : newProfileId();
    const source = raw.selection && typeof raw.selection === 'object' ? raw.selection : raw;
    return { id, name, selection: normalizeSelection(source) };
  }
  async function persistProfiles() {
    if (!bridge.capabilities?.storage) return;
    await bridge.storage.set(profileStoreKey(), { version: PROFILE_STORE_VERSION, activeId: state.activeId, profiles: state.profiles });
  }
  async function loadProfiles() {
    state.profiles = []; state.activeId = ''; state.migrated = false;
    if (!bridge.capabilities?.storage) {
      state.profiles = [{ id: newProfileId(), name: '默认配置', selection: { ...EMPTY_SELECTION } }];
      state.activeId = state.profiles[0].id;
      return;
    }
    let stored = await bridge.storage.get(profileStoreKey());
    if (stored && stored.version === PROFILE_STORE_VERSION && Array.isArray(stored.profiles)) {
      const usedIds = new Set(); const usedNames = new Set();
      for (const raw of stored.profiles.slice(0, MAX_PROFILES)) {
        const profile = normalizeProfile(raw);
        if (!profile) continue;
        if (usedIds.has(profile.id)) profile.id = newProfileId();
        profile.name = uniqueNameFor(profile.name, usedNames);
        usedIds.add(profile.id); usedNames.add(profile.name.toLocaleLowerCase());
        state.profiles.push(profile);
      }
      if (stored.activeId && state.profiles.some((profile) => profile.id === stored.activeId)) state.activeId = stored.activeId;
    }
    if (!state.profiles.length) {
      const legacy = await bridge.storage.get(legacyKey());
      const hasLegacy = legacy && typeof legacy === 'object' &&
        ['orgId', 'kind', 'resourceId', 'accountId', 'targetId', 'database', 'updateEndpoint'].some((key) => key in legacy);
      state.profiles = [{ id: newProfileId(), name: '默认配置', selection: normalizeSelection(hasLegacy ? legacy : EMPTY_SELECTION) }];
      state.activeId = state.profiles[0].id;
      state.migrated = !!hasLegacy;
      await persistProfiles();
    }
    if (!state.activeId) state.activeId = state.profiles[0].id;
  }
  function uniqueNameFor(name, usedNames) {
    let candidate = name;
    if (!usedNames.has(candidate.toLocaleLowerCase())) return candidate;
    for (let index = 2; index < 1000; index++) {
      const suffix = ` (${index})`;
      candidate = bounded(name, MAX_PROFILE_NAME - suffix.length) + suffix;
      if (!usedNames.has(candidate.toLocaleLowerCase())) return candidate;
    }
    return bounded(`配置 ${Date.now()}`, MAX_PROFILE_NAME);
  }
  async function saveSelection() {
    const profile = activeProfile();
    if (!profile) return;
    profile.selection = selection();
    await persistProfiles();
  }
  function renderProfiles() {
    const select = $('profile-select');
    select.replaceChildren(new Option(state.profiles.length ? '选择配置' : '暂无配置', ''));
    for (const profile of state.profiles) select.add(new Option(profile.name, profile.id));
    if (state.profiles.some((profile) => profile.id === state.activeId)) select.value = state.activeId;
    $('profile-count').textContent = state.profiles.length ? `${state.profiles.length} 个配置 · 修改自动保存` : '';
    updateButtons();
  }
  function defaultProfileName() {
    const resourceText = $('resource').selectedOptions?.[0]?.textContent || '';
    const resourceName = resourceText.split(' · ')[0].trim();
    const targetText = $('target').value ? ($('target').selectedOptions?.[0]?.textContent || '').trim() : '';
    const base = resourceName ? `${resourceName}${targetText ? ` → ${targetText}` : ''}` : `配置 ${state.profiles.length + 1}`;
    return uniqueProfileName(bounded(base, MAX_PROFILE_NAME));
  }
  function targetHint() {
    const existing = !!$('target').value;
    const canEdit = bridge.capabilities?.connectionEditApi;
    $('update-endpoint-row').hidden = !existing || !canEdit;
    if (!existing) {
      $('target-hint').textContent = '将从 JumpServer smart 接口自动获取主机和端口，填入 DBX 新建连接表单。';
    } else if (!canEdit) {
      $('target-hint').textContent = '当前工作台不支持预填已保存连接，请使用“获取参数”手动填入。';
    } else {
      const endpoint = $('update-endpoint').checked ? '同时填入从 JumpServer smart 接口获取的主机和端口。' : '';
      $('target-hint').textContent = `将在 DBX 编辑表单中填入临时用户名和密码。${endpoint}数据库名留空时沿用原值。`;
    }
    $('lifetime').textContent = `令牌有效期 ${state.service?.expireMinutes || 60} 分钟 · 连接地址自动发现`;
    updateButtons();
  }
  function refreshTargets(preferred = '') {
    const account = state.accounts.find((item) => item.id === $('account').value);
    const resource = state.resources.find((item) => item.id === $('resource').value);
    const protocol = account?.protocol || resource?.protocol;
    const type = ({ mysql: 'mysql', mariadb: 'mysql', postgresql: 'postgres', postgres: 'postgres', sqlserver: 'sqlserver' })[protocol];
    const existing = preferred || $('target').value;
    options($('target'), state.targets.filter((item) => (item.dbType || item.dialect) === type), '新建数据库连接', (item) => item.name);
    if (Array.from($('target').options).some((option) => option.value === existing)) $('target').value = existing;
    targetHint();
  }
  async function loadAccounts(preferred = '') {
    state.accounts = []; options($('account'), [], '请选择系统账号', String); refreshTargets();
    if (!$('resource').value) { updateButtons(); return; }
    const data = await invoke('jumpserver/accounts', selection());
    state.accounts = data.items;
    options($('account'), state.accounts, state.accounts.length ? '请选择系统账号' : '没有受支持的数据库账号，请检查授权',
      (item) => `${item.name}${item.username ? ` / ${item.username}` : ''} · ${item.protocol}`);
    if (state.accounts.some((account) => account.id === preferred)) $('account').value = preferred;
    else if (state.accounts.length === 1) $('account').value = state.accounts[0].id;
    refreshTargets();
  }
  async function loadResources(preferredResource = '', preferredAccount = '') {
    clearPreview(); state.resources = []; state.accounts = [];
    options($('resource'), [], '正在加载资源…', String); options($('account'), [], '先选择数据库资源', String); refreshTargets();
    if (!$('org').value) { options($('resource'), [], '先选择组织', String); return; }
    const data = await invoke('jumpserver/resources', { ...selection(), search: $('search').value });
    state.resources = data.items;
    options($('resource'), state.resources, state.resources.length ? '请选择数据库资源' : '当前组织未找到数据库，请检查组织和资源类型',
      (item) => `${item.name}${item.protocol ? ` · ${item.protocol}` : ''}`);
    if (state.resources.some((resource) => resource.id === preferredResource)) {
      $('resource').value = preferredResource;
      await loadAccounts(preferredAccount);
    }
    status(`已加载 ${state.resources.length} 个资源。`);
  }
  async function applyProfileSelection(value) {
    const saved = normalizeSelection(value);
    const warnings = [];
    clearPreview();
    $('kind').value = saved.kind;
    $('database').value = saved.database;
    $('update-endpoint').checked = saved.updateEndpoint;
    $('search').value = '';
    const org = state.service?.organizations.find((item) => item.id === saved.orgId);
    if (org) $('org').value = org.id;
    else if (state.service?.organizations.length === 1) $('org').value = state.service.organizations[0].id;
    else $('org').value = '';
    if (saved.orgId && !org && state.service?.organizations.length !== 1) warnings.push('原组织已不可用');
    if ($('org').value) {
      await loadResources(saved.resourceId, saved.accountId);
      if (saved.resourceId && !state.resources.some((resource) => resource.id === saved.resourceId)) warnings.push('原数据库已不可用');
      if (saved.accountId && !state.accounts.some((account) => account.id === saved.accountId)) warnings.push('原系统账号已不可用');
      refreshTargets(saved.targetId);
      if (saved.targetId && !state.targets.some((target) => target.id === saved.targetId)) warnings.push('原目标连接已不可用');
    } else {
      options($('resource'), [], '先选择组织', String);
      options($('account'), [], '先选择数据库资源', String);
      refreshTargets();
    }
    $('database').value = saved.database;
    $('update-endpoint').checked = saved.updateEndpoint;
    targetHint();
    return warnings;
  }
  async function initialize() {
    const epoch = state.epoch;
    $('profile-dialog').hidden = true;
    state.service = null; state.profiles = []; state.activeId = ''; state.profileDialog = null; clearPreview();
    state.connectionId = bridge.context?.connectionId || '';
    if (!state.connectionId) { status('请先在 DBX 新建并连接“JumpServer 凭证服务”，再打开它的工作台。', 'error'); return; }
    const service = await invoke('jumpserver/profile');
    if (epoch !== state.epoch) return;
    state.service = service;
    $('service').textContent = `${service.username} · JumpServer 凭证服务`;
    options($('org'), service.organizations, '选择实际组织', (org) => org.name);
    state.targets = bridge.capabilities?.connectionsApi ? await bridge.getConnections() : [];
    await loadProfiles();
    if (epoch !== state.epoch) return;
    renderProfiles();
    const active = activeProfile();
    const warnings = active ? await applyProfileSelection(active.selection) : [];
    if (!service.organizations.length) status('用户信息没有返回组织列表，请检查 JumpServer 版本和组织权限。', 'error');
    else if (warnings.length) status(`已恢复配置「${active.name}」，但${warnings.join('、')}，请重新选择后保存。`, 'error');
    else if (state.migrated) status(`已将原来的单条选择迁移为配置「${active.name}」。`);
    else if (readyToFill()) status(`已恢复配置「${active.name}」，${canFill() ? '可直接刷新并填入 DBX' : '可获取参数后手动填入'}。`);
    else status(`当前配置「${active.name}」尚未完成选择。可先选择组织和数据库，或用“另存为配置”保存多个组合。`);
  }
  function openProfileDialog(mode) {
    if (state.busy) return;
    const active = activeProfile();
    if (mode === 'create' && !readyToFill()) { status('请先选择组织、数据库资源和系统账号，再另存为配置。', 'error'); return; }
    if (!active) return;
    state.profileDialog = { mode, profileId: mode === 'create' ? '' : active.id, returnFocus: document.activeElement };
    const deletion = mode === 'delete';
    $('profile-dialog-title').textContent = mode === 'create' ? '另存为新配置' : mode === 'rename' ? '重命名配置' : '删除配置';
    $('profile-dialog-copy').textContent = mode === 'create'
      ? '当前组织、数据库、系统账号和目标 DBX 连接会保存成一个独立配置。'
      : mode === 'rename' ? '只修改配置名称，不影响已保存的选择。' : `确定删除配置「${active.name}」？这不会删除或修改 DBX 中的数据库连接。`;
    $('profile-name-row').hidden = deletion;
    $('profile-name').value = mode === 'create' ? defaultProfileName() : mode === 'rename' ? active.name : '';
    $('profile-dialog-error').hidden = true;
    $('profile-dialog-confirm').textContent = deletion ? '删除' : '保存';
    $('profile-dialog-confirm').className = deletion ? 'primary destructive' : 'primary';
    $('profile-dialog').hidden = false;
    (deletion ? $('profile-dialog-confirm') : $('profile-name')).focus();
  }
  function closeProfileDialog() {
    const returnFocus = state.profileDialog?.returnFocus;
    $('profile-dialog').hidden = true;
    state.profileDialog = null;
    if (returnFocus && document.contains(returnFocus)) returnFocus.focus();
  }
  function profileDialogError(message) {
    $('profile-dialog-error').textContent = message;
    $('profile-dialog-error').hidden = false;
  }
  async function confirmProfileDialog() {
    const dialog = state.profileDialog;
    if (!dialog) return;
    const mode = dialog.mode;
    if (mode !== 'delete') {
      const name = $('profile-name').value.trim();
      if (!name) { profileDialogError('请填写配置名称。'); $('profile-name').focus(); return; }
      if (name.length > MAX_PROFILE_NAME) { profileDialogError(`配置名称不能超过 ${MAX_PROFILE_NAME} 个字符。`); return; }
      if (profileNameExists(name, mode === 'rename' ? dialog.profileId : '')) { profileDialogError('已有同名配置，请换一个名称。'); $('profile-name').focus(); return; }
    }
    $('profile-dialog-confirm').disabled = true;
    try {
      if (mode === 'delete') {
        if (state.profiles.length <= 1) { profileDialogError('至少保留一个配置。'); return; }
        const index = state.profiles.findIndex((profile) => profile.id === dialog.profileId);
        const removed = state.profiles[index];
        state.profiles.splice(index, 1);
        state.activeId = state.profiles[Math.min(index, state.profiles.length - 1)].id;
        await persistProfiles();
        renderProfiles();
        closeProfileDialog();
        const next = activeProfile();
        await run(async () => {
          const warnings = await applyProfileSelection(next.selection);
          status(warnings.length ? `已删除「${removed.name}」，切换到「${next.name}」，但${warnings.join('、')}。` : `已删除「${removed.name}」，已切换到「${next.name}」。`, warnings.length ? 'error' : 'success');
        }, '正在切换配置…');
        return;
      }
      if (mode === 'create') {
        if (state.profiles.length >= MAX_PROFILES) { profileDialogError(`最多保存 ${MAX_PROFILES} 个配置。`); return; }
        const onlyProfile = state.profiles.length === 1 ? state.profiles[0] : null;
        const initialBlank = onlyProfile && onlyProfile.name === '默认配置';
        // The first named save turns the automatically-created default into
        // the user's profile instead of leaving an extra unnamed duplicate.
        const profile = initialBlank ? onlyProfile : { id: newProfileId(), name: '', selection: null };
        profile.name = $('profile-name').value.trim();
        profile.selection = selection();
        if (!initialBlank) state.profiles.push(profile);
        state.activeId = profile.id;
        await persistProfiles();
        renderProfiles();
        closeProfileDialog();
        status(`已保存配置「${profile.name}」，后续修改会单独保存。`, 'success');
        return;
      }
      const active = activeProfile();
      active.name = $('profile-name').value.trim();
      await persistProfiles();
      renderProfiles();
      closeProfileDialog();
      status(`配置已重命名为「${active.name}」。`, 'success');
    } catch (error) {
      profileDialogError(`保存失败：${error?.message || error}`);
    } finally {
      $('profile-dialog-confirm').disabled = false;
    }
  }
  async function switchProfile(profileId) {
    const profile = state.profiles.find((item) => item.id === profileId);
    if (!profile || profile.id === state.activeId) return;
    const previousId = state.activeId;
    state.activeId = profile.id;
    await run(async () => {
      try {
        await persistProfiles();
        const warnings = await applyProfileSelection(profile.selection);
        if (warnings.length) status(`已切换到配置「${profile.name}」，但${warnings.join('、')}，请重新选择。`, 'error');
        else status(`已切换到配置「${profile.name}」。`);
      } catch (error) {
        state.activeId = previousId;
        renderProfiles();
        throw error;
      }
    }, `正在加载配置「${profile.name}」…`);
  }
  async function showPreview(value) {
    clearPreview(); state.preview = value;
    const fields = [
      ...(value.host && value.port > 0 ? [['host', '主机'], ['port', '端口']] : []),
      ['username', '用户名'], ['password', '密码'], ['database', '数据库'],
    ];
    for (const [key, label] of fields) {
      const row = document.createElement('div'); row.className = 'value-row';
      const caption = document.createElement('label'); caption.textContent = label; caption.htmlFor = `value-${key}`;
      const input = document.createElement('input'); input.id = `value-${key}`; input.readOnly = true;
      input.type = key === 'password' ? 'password' : 'text'; input.value = String(value[key] ?? '');
      const copy = document.createElement('button'); copy.type = 'button'; copy.className = 'secondary'; copy.textContent = '复制';
      copy.setAttribute('aria-label', `复制${label}`); copy.disabled = !bridge.capabilities?.clipboardWrite;
      copy.addEventListener('click', async () => { try { await bridge.clipboard.writeText(input.value); status(`已复制${label}。`, 'success'); } catch { status('复制失败，请选中参数后手动复制。', 'error'); } });
      row.append(caption, input, copy); $('preview-values').append(row);
    }
    $('expiry').textContent = `到期时间：${new Date(value.expiresAt).toLocaleString()}`;
    $('preview-panel').hidden = false;
  }
  $('org').addEventListener('change', () => run(async () => { await loadResources(); await saveSelection(); }, '正在加载组织资源…'));
  $('kind').addEventListener('change', () => run(async () => { await loadResources(); await saveSelection(); }, '正在切换资源类型…'));
  $('search-button').addEventListener('click', () => run(async () => { await loadResources(); await saveSelection(); }, '正在搜索数据库…'));
  $('search').addEventListener('keydown', (event) => { if (event.key === 'Enter') { event.preventDefault(); $('search-button').click(); } });
  $('resource').addEventListener('change', () => run(async () => { clearPreview(); await loadAccounts(); await saveSelection(); status('已加载系统账号。'); }, '正在加载系统账号…'));
  function persistChange() { void saveSelection().catch(() => status('选择尚未保存，请重新选择后重试。', 'error')); }
  $('account').addEventListener('change', () => { clearPreview(); refreshTargets(); updateButtons(); persistChange(); });
  $('target').addEventListener('change', () => { clearPreview(); targetHint(); persistChange(); });
  $('database').addEventListener('change', () => { clearPreview(); persistChange(); });
  $('update-endpoint').addEventListener('change', () => { clearPreview(); targetHint(); persistChange(); });
  $('profile-select').addEventListener('change', () => { void switchProfile($('profile-select').value); });
  $('profile-new').addEventListener('click', () => openProfileDialog('create'));
  $('profile-rename').addEventListener('click', () => openProfileDialog('rename'));
  $('profile-delete').addEventListener('click', () => openProfileDialog('delete'));
  $('profile-dialog-cancel').addEventListener('click', closeProfileDialog);
  $('profile-dialog-confirm').addEventListener('click', () => { void confirmProfileDialog(); });
  $('profile-dialog').addEventListener('click', (event) => { if (event.target === $('profile-dialog')) closeProfileDialog(); });
  $('profile-name').addEventListener('keydown', (event) => { if (event.key === 'Enter') { event.preventDefault(); void confirmProfileDialog(); } });
  document.addEventListener('keydown', (event) => { if (event.key === 'Escape' && !$('profile-dialog').hidden) closeProfileDialog(); });
  $('reload').addEventListener('click', () => run(initialize, '正在重新加载…'));
  $('clear-preview').addEventListener('click', clearPreview);
  // DBX uses sandbox="allow-scripts" without allow-forms. Native submit is
  // blocked before the submit event, so run this action directly from click.
  $('form').addEventListener('submit', (event) => event.preventDefault());
  $('fill').addEventListener('click', () => {
    if (!canFill()) return;
    const epoch = state.epoch;
    const current = selection();
    run(async () => {
      clearPreview(); await saveSelection();
      if (epoch !== state.epoch) return;
      let expiresAt;
      let message;
      if (current.targetId) {
        const keys = ['username', 'password'];
        if (current.updateEndpoint) keys.push('host', 'port');
        if (current.database) keys.push('database');
        const editable = await bridge.connections.getEditFields(current.targetId);
        if (epoch !== state.epoch) return;
        if (!keys.every((key) => editable.some((field) => field.key === key))) {
          throw new Error('该 DBX 连接不支持预填所选字段，请检查连接模式，或使用“获取参数”手动填入。');
        }
        const credentials = await invoke('jumpserver/credentials', current);
        const fields = Object.fromEntries(keys.map((key) => [key, credentials[key]]));
        const result = await bridge.connections.openEdit({ connectionId: current.targetId, fields });
        if (!result?.opened) throw new Error('未能打开 DBX 连接编辑表单，请重试或使用“获取参数”。');
        expiresAt = credentials.expiresAt;
        message = '请在 DBX 连接表单中检查并保存';
      } else {
        const result = await invoke('jumpserver/fill', current);
        if (!result?.opened) throw new Error('未能请求打开 DBX 新建连接表单，请重试或使用“获取参数”。');
        expiresAt = result.expiresAt;
        message = result.message;
      }
      if (epoch === state.epoch) {
        status(`${message}。令牌到期：${new Date(expiresAt).toLocaleString()}。`, 'success');
      }
    }, '正在准备临时凭证并打开 DBX 连接表单…', 'fill');
  });
  $('preview').addEventListener('click', () => run(async () => {
    clearPreview(); await saveSelection(); const value = await invoke('jumpserver/credentials', selection());
    await showPreview(value); status('已获取临时参数。', 'success');
  }, '正在获取连接参数…', 'preview'));
  if (!bridge) { status('此页面需要在 DBX 插件工作台或官方插件开发环境中运行。', 'error'); $('reload').disabled = true; return; }
  await bridge.ready;
  if (bridge.onContext) bridge.onContext(() => {
    state.epoch++; clearPreview(); state.service = null; state.connectionId = ''; state.profiles = []; state.activeId = '';
    $('profile-dialog').hidden = true; state.profileDialog = null;
    renderProfiles(); status('连接上下文已更改，请点击“重新加载”后继续。'); updateButtons();
  });
  await run(initialize, '正在加载 JumpServer 组织与配置…');
})();

window.createJumpServerFiles = function ({ bridge, invoke, selection, connection, run, update }) {
  const $ = (name) => document.getElementById('ssh-file-' + name);
  const chunkBytes = 512 * 1024;
  let directory = '/';
  let root = '/';
  let listed = false;
  let entries = [];
  const selected = new Set();
  let pendingDelete = null;
  let revision = 0;

  function status(message, tone = '') {
    $('status').textContent = message;
    $('status').title = message;
    $('status').dataset.tone = tone;
  }

  function setProgress(value, total) {
    const progress = $('progress');
    progress.hidden = total === 0;
    progress.max = total || 1;
    progress.value = value;
  }

  function reset() {
    revision++;
    directory = root = '/';
    listed = false;
    entries = [];
    selected.clear();
    if ($('delete-dialog').open) $('delete-dialog').close();
    pendingDelete = null;
    $('path').value = '/';
    $('filter').value = '';
    $('list').replaceChildren();
    setProgress(0, 0);
    status('请选择 SSH 资产和账号。');
  }

  function sync(busy, hasAccount) {
    const enabled = !busy && hasAccount;
    const files = bridge.capabilities?.fileTransfer;
    $('up').disabled = !enabled || directory === root;
    $('go').disabled = !enabled;
    $('refresh').disabled = !enabled;
    $('upload').disabled = !enabled || !listed || !files?.pick || !files?.read;
    const checked = entries.filter((item) => selected.has(item.name));
    const visibleCount = visibleEntries().length;
    $('selection').textContent = checked.length ? `已选 ${checked.length} 项` : '';
    $('download').disabled = !enabled || !listed || !checked.length || checked.some((item) => item.is_dir) || !files?.beginSave;
    $('delete').disabled = !enabled || !listed || !checked.length;
    $('select-all').disabled = !enabled || !visibleCount;
    $('select-all').checked = visibleCount > 0 && checked.length === visibleCount;
    $('select-all').indeterminate = checked.length > 0 && checked.length < visibleCount;
    $('path').disabled = !enabled;
    $('filter').disabled = busy;
    for (const control of $('list').querySelectorAll('button, input')) control.disabled = !enabled;
  }

  function child(name) {
    return directory === '/' ? '/' + name : directory + '/' + name;
  }

  function validName(name) {
    return typeof name === 'string' && name.length > 0 && name !== '.' && name !== '..' && !/[\/\\\u0000-\u001f]/u.test(name);
  }

  function formatSize(size) {
    const bytes = Number(size);
    if (!Number.isFinite(bytes) || bytes < 0) return '—';
    if (bytes >= 1048576) return (bytes / 1048576).toFixed(1) + ' MiB';
    if (bytes >= 1024) return (bytes / 1024).toFixed(1) + ' KiB';
    return bytes + ' B';
  }

  function formatTime(value) {
    if (!value) return '—';
    const date = typeof value === 'number' || /^\d+$/.test(value) ? new Date(Number(value) * 1000) : new Date(value);
    return Number.isNaN(date.valueOf()) ? '—' : date.toLocaleString();
  }

  function visibleEntries() {
    const filter = $('filter').value.trim().toLowerCase();
    return entries.filter((item) => !filter || item.name.toLowerCase().includes(filter));
  }

  function render() {
    $('list').replaceChildren();
    const visible = visibleEntries();
    const names = new Set(visible.map((item) => item.name));
    for (const name of selected) {
      if (!names.has(name)) selected.delete(name);
    }
    for (const item of visible) {
      const row = $('list').insertRow();
      const check = document.createElement('input');
      check.type = 'checkbox';
      check.checked = selected.has(item.name);
      check.setAttribute('aria-label', '选择 ' + item.name);
      row.toggleAttribute('data-selected', check.checked);
      check.addEventListener('change', () => {
        if (check.checked) selected.add(item.name);
        else selected.delete(item.name);
        row.toggleAttribute('data-selected', check.checked);
        update();
      });
      row.insertCell().append(check);
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'ssh-file-name';
      button.dataset.kind = item.is_dir ? 'directory' : 'file';
      button.textContent = item.name;
      button.title = item.name;
      button.addEventListener('click', () => {
        if (item.is_dir) void perform((request, current) => list(request, child(item.name), current));
        else {
          selected.clear();
          selected.add(item.name);
          render();
          for (const control of $('list').querySelectorAll('.ssh-file-name')) {
            if (control.textContent === item.name) control.focus();
          }
        }
      });
      row.insertCell().append(button);
      row.insertCell().textContent = item.is_dir ? '—' : formatSize(item.size);
      row.insertCell().textContent = formatTime(item.mod_time);
    }
    update();
  }

  function perform(action) {
    if (!selection().accountId) return;
    const started = revision;
    const id = connection();
    const target = selection();
    const current = () => revision === started && id === connection();
    const request = (method, payload) => invoke('files/' + method, { ...target, ...payload }, id);
    const ensureCurrent = () => {
      if (!current()) throw new Error('连接已切换');
    };
    return run(async () => {
      try {
        await action(request, ensureCurrent);
      } catch (error) {
        if (current()) status(error?.message || String(error), 'error');
      } finally {
        if (current()) setProgress(0, 0);
      }
    });
  }

  async function list(request, input, ensureCurrent = () => {}) {
    const remotePath = input.trim().replace(/\/+$/, '') || '/';
    if (!remotePath.startsWith('/')) throw new Error('请输入绝对路径');
    if (root !== '/' && remotePath !== '/' && remotePath !== root && !remotePath.startsWith(root + '/')) {
      throw new Error('路径超出 JumpServer 授权的 SFTP 根目录 ' + root);
    }
    status('正在读取目录…');
    const result = await request('list', { remotePath });
    ensureCurrent();
    root = result.sftpRoot || '/';
    directory = result.resolvedPath || remotePath;
    listed = true;
    selected.clear();
    $('path').value = directory;
    $('filter').value = '';
    entries = result.items.filter((item) => validName(item.name)).sort((a, b) => Number(!!b.is_dir) - Number(!!a.is_dir) || a.name.localeCompare(b.name));
    render();
    status(`SFTP 根目录 ${root} · ${entries.length} 项`);
  }

  function open() {
    if (!listed) return perform((request, current) => list(request, '/', current));
  }

  function verifyOffset(next, offset, total) {
    if (!Number.isSafeInteger(next) || next <= offset || next > total) throw new Error('文件传输分块大小不一致');
  }

  async function uploadFile(request, current, file) {
    let transfer;
    try {
      current();
      if (!validName(file.name)) throw new Error('不支持此文件名');
      if (file.size > 8 * 1024 * 1024) throw new Error('单个上传文件上限为 8 MiB');
      const remotePath = child(file.name);
      status('准备上传 ' + file.name + '…');
      transfer = await request('prepare-upload', { remotePath, size: file.size });
      for (let offset = 0; offset < file.size;) {
        current();
        const chunk = await bridge.fileTransfer.read(file.handleId, offset, chunkBytes);
        const reply = await request('write-upload', { transferId: transfer.transferId, offset, data: chunk.dataBase64 });
        verifyOffset(reply.nextOffset, offset, file.size);
        offset = reply.nextOffset;
        setProgress(offset, file.size);
      }
      current();
      status('正在保存到远端 ' + remotePath + '…');
      await request('commit-upload', { transferId: transfer.transferId });
      transfer = null;
      return true;
    } finally {
      if (transfer) await request('close-transfer', { transferId: transfer.transferId }).catch(() => {});
    }
  }

  async function downloadFile(request, current, { name }) {
    status('正在从 JumpServer 读取 ' + name + '…');
    const transfer = await request('prepare-download', { remotePath: child(name) });
    let destination;
    try {
      current();
      destination = await bridge.fileTransfer.beginSave({ name, size: transfer.size });
      if (!destination) {
        return false;
      }
      for (let offset = 0; offset < transfer.size;) {
        current();
        const chunk = await request('read-download', { transferId: transfer.transferId, offset });
        verifyOffset(chunk.nextOffset, offset, transfer.size);
        const saved = await bridge.fileTransfer.write(destination.handleId, offset, chunk.data);
        if (saved.nextOffset !== chunk.nextOffset) throw new Error('本机文件写入不完整');
        offset = chunk.nextOffset;
        setProgress(offset, transfer.size);
      }
      await bridge.fileTransfer.finish(destination.handleId);
      destination = null;
      return true;
    } finally {
      if (destination) await bridge.fileTransfer.cancel(destination.handleId).catch(() => {});
      await request('close-transfer', { transferId: transfer.transferId }).catch(() => {});
    }
  }

  function reportBatch(action, succeeded, skipped, failures) {
    const issues = failures.map(({ name, error }) => `${name} · ${error}`);
    const message = `${action}完成 ${succeeded} 项` + (skipped ? `，跳过 ${skipped} 项` : '') +
      (issues.length ? `，失败 ${issues.length} 项。 ${issues.join('；')}` : '');
    status(message, issues.length ? 'error' : succeeded ? 'success' : '');
  }

  async function transferBatch(action, items, runItem, current) {
    let succeeded = 0;
    let skipped = 0;
    const failures = [];
    for (const item of items) {
      current();
      setProgress(0, 0);
      try {
        if (await runItem(item)) succeeded++;
        else skipped++;
      } catch (error) {
        failures.push({ name: item.name, error: error?.message || String(error) });
      }
    }
    return { action, succeeded, skipped, failures };
  }

  async function upload(request, current) {
    const { files } = await bridge.fileTransfer.pick({ multiple: true });
    if (!files.length) return;
    try {
      const outcome = await transferBatch('上传', files, (file) => uploadFile(request, current, file), current);
      if (outcome.succeeded) {
        try { await list(request, directory, current); }
        catch (error) { outcome.failures.push({ name: '刷新目录', error: error?.message || String(error) }); }
      }
      current();
      reportBatch(outcome.action, outcome.succeeded, outcome.skipped, outcome.failures);
    } finally {
      await Promise.allSettled(files.map((file) => bridge.fileTransfer.cancel(file.handleId)));
    }
  }

  async function download(request, current) {
    const items = entries.filter((item) => selected.has(item.name) && !item.is_dir);
    const outcome = await transferBatch('下载', items, (item) => downloadFile(request, current, item), current);
    current();
    reportBatch(outcome.action, outcome.succeeded, outcome.skipped, outcome.failures);
  }

  function openDelete() {
    const items = entries.filter((item) => selected.has(item.name))
      .map((item) => ({ name: item.name, isDir: !!item.is_dir }));
    if (!items.length) return;
    const chosenDirectory = directory;
    const chosenRevision = revision;
    $('delete-copy').textContent = `将永久删除 ${chosenDirectory} 下的 ${items.length} 项。` +
      (items.some((item) => item.isDir) ? ' 文件夹及其内容会一起删除。' : '');
    $('delete-list').replaceChildren(...items.map(({ name }) => {
      const li = document.createElement('li');
      li.textContent = name;
      return li;
    }));
    pendingDelete = () => perform(async (request, current) => {
      if (revision !== chosenRevision || directory !== chosenDirectory) throw new Error('目录已切换，请重新选择');
      status('正在删除所选项目…');
      const result = await request('delete', { remotePath: chosenDirectory, entries: items });
      current();
      const failures = result.items.filter((item) => item.error);
      const succeeded = result.items.length - failures.length;
      try { await list(request, chosenDirectory, current); }
      catch (error) { failures.push({ name: '刷新目录', error: error?.message || String(error) }); }
      current();
      reportBatch('删除', succeeded, 0, failures);
    });
    $('delete-dialog').showModal();
  }

  $('up').addEventListener('click', () => perform((request, current) => list(request, directory.slice(0, directory.lastIndexOf('/')) || '/', current)));
  $('go').addEventListener('click', () => perform((request, current) => list(request, $('path').value, current)));
  $('path').addEventListener('keydown', (event) => { if (event.key === 'Enter') $('go').click(); });
  $('refresh').addEventListener('click', () => perform((request, current) => list(request, directory, current)));
  $('filter').addEventListener('input', render);
  $('select-all').addEventListener('change', () => {
    selected.clear();
    if ($('select-all').checked) {
      for (const item of visibleEntries()) selected.add(item.name);
    }
    render();
  });
  $('delete').addEventListener('click', openDelete);
  $('delete-cancel').addEventListener('click', () => $('delete-dialog').close());
  $('delete-dialog').addEventListener('close', () => { pendingDelete = null; });
  $('delete-confirm').addEventListener('click', () => {
    const action = pendingDelete;
    pendingDelete = null;
    $('delete-dialog').close();
    if (action) void action();
  });
  $('upload').addEventListener('click', () => perform(upload));
  $('download').addEventListener('click', () => perform(download));
  reset();
  return { open, reset, sync };
};

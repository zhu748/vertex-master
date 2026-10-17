async function loadDatabase() {
  try {
    const s = await API.database.stats();
    const bytes = n => `${(Number(n || 0) / 1048576).toFixed(2)} MiB`;
    const values = [
      ['数据库文件', bytes(s.file_bytes)], ['WAL 日志', bytes(s.wal_bytes)],
      ['空闲页空间', bytes(s.reclaimable_bytes)], ['数据页', String(s.page_count || 0)],
    ];
    const host = $('#dbStats');
    host.replaceChildren();
    for (const [label, value] of values) {
      const row = document.createElement('p');
      row.textContent = `${label}：${value}`;
      host.appendChild(row);
    }
  } catch (e) { toast('读取数据库状态失败：' + e.message); }
}

async function runDatabaseAction(button) {
  button.disabled = true;
  const result = $('#dbResult');
  result.textContent = '正在执行…';
  try {
    const action = button.dataset.dbAction;
    if (action === 'check') {
      const data = await API.database.check();
      result.textContent = data.ok ? '完整性检查通过' : '检查发现问题：' + (data.results || []).join('；');
    } else if (action === 'checkpoint') {
      const data = await API.database.checkpoint();
      result.textContent = `检查点完成：已写回 ${data.checkpointed_pages} / ${data.wal_pages} 页${data.busy ? '，数据库正忙' : ''}`;
    }
    await loadDatabase();
  } catch (e) { result.textContent = '操作失败：' + e.message; }
  finally { button.disabled = false; }
}

registerActions({ loadDatabase: function () { loadDatabase(); }, databaseAction: function (el) { runDatabaseAction(el); } });

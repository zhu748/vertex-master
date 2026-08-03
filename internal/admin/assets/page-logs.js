let logsRefreshTimer = null;
let logsETag = '';
let logsLoadPromise = null;
let logsPreferenceSynced = false;
let renderedLogsContent = null;

function logsPageVisible() {
  const app = $('#app');
  const page = $('#page-logs');
  return !!app && !app.classList.contains('hidden') && !!page && !page.classList.contains('hidden');
}

function startLogsAutoRefresh() {
  if (logsRefreshTimer || document.hidden || !logsPageVisible()) return;
  logsRefreshTimer = setInterval(() => {
    if (!document.hidden && logsPageVisible()) loadLogs();
  }, 3000);
}

function stopLogsAutoRefresh() {
  if (!logsRefreshTimer) return;
  clearInterval(logsRefreshTimer);
  logsRefreshTimer = null;
}

function syncLogsAutoRefresh() {
  const check = $('#autoRefreshLogsCheck');
  if (check && check.checked) startLogsAutoRefresh();
  else stopLogsAutoRefresh();
}

async function fetchLogs() {
  try {
    const headers = {};
    if (logsETag) headers['If-None-Match'] = logsETag;
    const res = await fetch('/api/admin/log', { headers, cache: 'no-store' });
    const nextETag = res.headers.get('ETag');
    if (nextETag) logsETag = nextETag;

    if (!logsPreferenceSynced) {
      const autoRefresh = res.headers.get('X-Vertex-Auto-Refresh-Logs');
      const check = $('#autoRefreshLogsCheck');
      if (check && autoRefresh !== null) {
        check.checked = autoRefresh === 'true';
        logsPreferenceSynced = true;
      }
    }
    syncLogsAutoRefresh();

    if (res.status === 304) return;
    const data = await res.json();
    if (res.ok && data.ok) {
      const content = data.content || '';
      if (content !== renderedLogsContent) {
        renderLogs(content);
        renderedLogsContent = content;
      }
    } else {
      toast('拉取日志失败', true);
    }
  } catch (e) {
    console.error(e);
  }
}

function loadLogs() {
  if (logsLoadPromise) return logsLoadPromise;
  logsLoadPromise = fetchLogs().finally(() => { logsLoadPromise = null; });
  return logsLoadPromise;
}

function renderLogs(content) {
  // Render UI
  const tbody = $('#logUITbody');
  if (tbody) {
    const lines = content.split('\n').filter(l => l.trim() !== '');
    let html = '';
    
    // Parse standard Go log format: "2026/06/29 01:21:21 [Level] Message"
    // Or our custom format like "[Config] ..."
    lines.forEach(line => {
      let level = 'info';
      let timeStr = '';
      let msg = line;
      
      // Basic heuristic parsing
      const timeMatch = line.match(/^(\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2})\s+(.*)/);
      if (timeMatch) {
        timeStr = timeMatch[1];
        msg = timeMatch[2];
      }
      
      if (msg.includes('[Config]') || msg.includes('[Server]')) level = 'info';
      if (msg.includes('警告') || msg.includes('warn') || msg.includes('WARN')) level = 'warn';
      if (msg.includes('失败') || msg.includes('error') || msg.includes('ERROR') || msg.includes('错误')) level = 'error';
      
      let levelClass = '';
      let levelText = 'INFO';
      if (level === 'info') { levelClass = 'log-level-info'; levelText = 'INFO'; }
      if (level === 'warn') { levelClass = 'log-level-warn'; levelText = 'WARN'; }
      if (level === 'error') { levelClass = 'log-level-error'; levelText = 'ERRO'; }
      
      html += `<tr>
        <td>${timeStr}</td>
        <td class="${levelClass}">${levelText}</td>
        <td>${escapeHtml(msg)}</td>
      </tr>`;
    });
    
    tbody.innerHTML = html;
    const uiEl = $('#logContentUI');
    if (uiEl) uiEl.scrollTop = uiEl.scrollHeight;
  }
}

function escapeHtml(str) {
  return str.replace(/[&<>"']/g, function(m) {
    switch (m) {
      case '&': return '&amp;';
      case '<': return '&lt;';
      case '>': return '&gt;';
      case '"': return '&quot;';
      case "'": return '&#39;';
    }
  });
}

function toggleAutoRefreshLogs(silent) {
  const check = $('#autoRefreshLogsCheck');
  if (!check) return;
  if (silent !== true) {
    logsPreferenceSynced = true;
    API.settings.put({ auto_refresh_logs: check.checked }).catch(() => {});
  }
  if (check.checked) {
    startLogsAutoRefresh();
    if (silent !== true) toast('已开启自动刷新日志');
  } else {
    stopLogsAutoRefresh();
    if (silent !== true) toast('已关闭自动刷新日志');
  }
}

function leaveLogsPage() {
  stopLogsAutoRefresh();
  logsPreferenceSynced = false;
}

document.addEventListener('visibilitychange', () => {
  syncLogsAutoRefresh();
  if (!document.hidden && logsPageVisible()) loadLogs();
});

registerActions({
  loadLogs: function () { loadLogs(); },
  toggleAutoRefreshLogs: function () { toggleAutoRefreshLogs(); },
});

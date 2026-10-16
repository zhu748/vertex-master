// ---- 运行日志页 ----
//
// /api/admin/log 返回原始日志文本（Go 标准库 log 格式：YYYY/MM/DD HH:MM:SS 消息），
// 解析与展示全部在前端完成：
//   1. 多行归组 —— 无时间戳的续行（上游多行报错体、panic 堆栈）并入上一条日志；
//   2. 级别判定 —— [ERROR]/[WARN]/[DEBUG] 显式标记优先，其次按中英文报错关键词，
//      并剔除成功路径里的空错误占位（error=""），避免把正常日志误标成报错；
//   3. 级别筛选 + 关键词搜索 + 单条复制 + 超长报错折叠展开；
//   4. 智能滚动 —— 只有用户本来就在底部时才跟随新日志，翻阅历史报错不再被拉走。

let logsRefreshTimer = null;
let logsETag = '';
let logsLoadPromise = null;
let logsPreferenceSynced = false;
let renderedLogsContent = null;

// 解析后的日志条目：{ time, msg, raw, level, tag, displayMsg, startup }
let logEntries = [];
let logLevelFilter = 'all';
let logSearchQuery = '';
let logFollowTail = true;
let logNewSinceDetach = 0;
let logsScrollBound = false;

const LOG_TIME_RE = /^(\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2})\s?/;
// 无时间戳但必须独立成条的首行：panic / fatal / 启动分隔线。
// goroutine、[signal]、堆栈帧等续行仍然并入上一条，保证堆栈完整。
const LOG_STANDALONE_RE = /^(panic: |fatal error: |={5,} STARTUP)/;

const LOG_LEVEL_TEXT = {
  error: 'ERRO',
  warn: 'WARN',
  debug: 'DEBU',
  info: 'INFO',
};

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
      toast('拉取日志失败');
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

// ---- 解析：行 -> 条目 ----

function parseLogEntries(content) {
  const entries = [];
  const lines = content.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].replace(/\r$/, '');
    if (line.trim() === '') continue;
    const m = line.match(LOG_TIME_RE);
    if (m) {
      entries.push({ time: m[1], msg: line.slice(m[0].length), raw: line });
      continue;
    }
    if (entries.length > 0 && !LOG_STANDALONE_RE.test(line)) {
      // 无时间戳的续行：多行报错体 / 堆栈，并入上一条，保留原始换行。
      const prev = entries[entries.length - 1];
      prev.msg += '\n' + line;
      prev.raw += '\n' + line;
      continue;
    }
    entries.push({ time: '', msg: line, raw: line });
  }
  for (let i = 0; i < entries.length; i++) {
    const e = entries[i];
    e.level = classifyLogLevel(e.msg);
    e.startup = /^={5,} STARTUP/.test(e.msg);
    const tagMatch = e.msg.match(/^\[([^\]]{1,24})\]/);
    e.tag = tagMatch ? tagMatch[1] : '';
    e.displayMsg = tagMatch ? e.msg.slice(tagMatch[0].length).replace(/^\s/, '') : e.msg;
  }
  return entries;
}

// ---- 级别判定 ----
//
// 优先级：显式 [ERROR] 标记 > panic/fatal > 显式 [WARN]/警告 > 报错关键词
// > 警告关键词 > [DEBUG] > info。级别只看消息头部（多行报错体往往很长）。
function classifyLogLevel(msg) {
  const head = msg.length > 600 ? msg.slice(0, 600) : msg;
  if (head.indexOf('[ERROR]') !== -1) return 'error';
  if (/^panic: |^fatal error: /.test(head)) return 'error';
  if (head.indexOf('[WARN]') !== -1 || head.indexOf('警告') !== -1) return 'warn';
  // error="" 是成功路径的空错误占位（如节点测试 ok=true），先剔除再匹配报错关键词。
  const cleaned = head.replace(/error=""/g, ' ');
  if (/失败|错误|报错|超时|异常/.test(cleaned) ||
      /\b(?:error|fail(?:ed|ure|s)?|timeout|timed out|refused|unreachable|denied)\b/i.test(cleaned)) {
    // 例行汇总（如"巡检完成：检查 N，可用 N，失败 N"）提到失败计数时降级为警告。
    return /完成[:：]/.test(cleaned) ? 'warn' : 'error';
  }
  if (/\b429\b|无效/.test(cleaned) || /\bwarn(?:ing)?\b/i.test(cleaned)) return 'warn';
  if (head.indexOf('[DEBUG]') !== -1) return 'debug';
  return 'info';
}

// ---- 渲染 ----

function renderLogs(content) {
  const prevCount = logEntries.length;
  logEntries = parseLogEntries(content);
  updateLogChips();
  // 用户正在上方翻阅时，新到的日志只累计计数提示，不打断阅读。
  if (!logFollowTail && logEntries.length > prevCount) {
    logNewSinceDetach += logEntries.length - prevCount;
  }
  applyLogFilters(false);
  updateLogJumpButton();
}

function buildLogRow(entry, idx) {
  const levelText = LOG_LEVEL_TEXT[entry.level] || 'INFO';
  const long = entry.displayMsg.length > 400 || entry.displayMsg.split('\n').length > 10;
  const rowCls = entry.startup ? 'log-row-startup' : 'log-row-' + entry.level;
  let html = '<tr class="' + rowCls + '">' +
    '<td>' + esc(entry.time) + '</td>' +
    '<td><span class="log-level-badge log-level-' + entry.level + '">' + levelText + '</span></td>' +
    '<td>' + (entry.tag !== '' ? '<span class="log-tag">' + esc(entry.tag) + '</span>' : '') + '</td>' +
    '<td class="log-msg-cell">' +
      '<div class="log-msg' + (long ? ' log-msg-clamped' : '') + '">' + esc(entry.displayMsg) + '</div>';
  if (long) html += '<button type="button" class="log-expand-btn" data-click-action="toggleLogExpand">展开全部</button>';
  html += '<button type="button" class="log-copy-btn" data-click-action="copyLogEntry" data-idx="' + idx + '">复制</button></td></tr>';
  return html;
}

function applyLogFilters(forceTail) {
  const tbody = $('#logUITbody');
  if (!tbody) return;
  const q = logSearchQuery.toLowerCase();
  let html = '';
  let shown = 0;
  logEntries.forEach(function (entry, idx) {
    if (logLevelFilter !== 'all' && entry.level !== logLevelFilter) return;
    if (q !== '' && entry.raw.toLowerCase().indexOf(q) === -1) return;
    shown++;
    html += buildLogRow(entry, idx);
  });
  tbody.innerHTML = html;
  const emptyHint = $('#logEmptyHint');
  if (emptyHint) emptyHint.classList.toggle('hidden', shown > 0);
  syncLogScroll(forceTail);
}

function updateLogChips() {
  const counts = { all: logEntries.length, error: 0, warn: 0, debug: 0, info: 0 };
  logEntries.forEach(function (entry) {
    if (counts[entry.level] !== undefined) counts[entry.level]++;
  });
  document.querySelectorAll('#logLevelChips .log-chip-count').forEach(function (el) {
    const key = el.dataset.count;
    if (counts[key] !== undefined) el.textContent = String(counts[key]);
  });
  const errChip = document.querySelector('#logLevelChips .log-chip-error');
  if (errChip) errChip.classList.toggle('log-chip-has', counts.error > 0);
  const hint = $('#logsHint');
  if (hint) {
    hint.textContent = logEntries.length === 0
      ? '暂无日志'
      : '最近 ' + logEntries.length + ' 条日志 · 错误 ' + counts.error + ' · 警告 ' + counts.warn;
  }
}

// ---- 智能滚动 ----

function logsAtBottom(el) {
  return el.scrollHeight - el.scrollTop - el.clientHeight < 28;
}

function bindLogsScroll() {
  const el = $('#logContentUI');
  if (!el || logsScrollBound) return;
  logsScrollBound = true;
  el.addEventListener('scroll', function () {
    if (logsAtBottom(el)) {
      logFollowTail = true;
      if (logNewSinceDetach !== 0) {
        logNewSinceDetach = 0;
        updateLogJumpButton();
      }
    } else {
      logFollowTail = false;
    }
  }, { passive: true });
}

function syncLogScroll(forceTail) {
  const el = $('#logContentUI');
  if (!el) return;
  if (forceTail) logFollowTail = true;
  if (logFollowTail) {
    el.scrollTop = el.scrollHeight;
    logNewSinceDetach = 0;
    updateLogJumpButton();
  }
}

function updateLogJumpButton() {
  const btn = $('#logJumpBtn');
  if (!btn) return;
  const show = !logFollowTail && logNewSinceDetach > 0;
  btn.classList.toggle('hidden', !show);
  const countEl = $('#logJumpCount');
  if (countEl) countEl.textContent = ' · ' + logNewSinceDetach + ' 条新日志';
}

function jumpToLogBottom() {
  const el = $('#logContentUI');
  logFollowTail = true;
  logNewSinceDetach = 0;
  if (el) el.scrollTop = el.scrollHeight;
  updateLogJumpButton();
}

// ---- 交互动作 ----

function filterLogLevel(el) {
  logLevelFilter = el.dataset.level || 'all';
  const chips = document.querySelectorAll('#logLevelChips .log-chip');
  for (let i = 0; i < chips.length; i++) chips[i].classList.toggle('active', chips[i] === el);
  applyLogFilters(true);
}

function searchLogs(el) {
  logSearchQuery = el.value.trim();
  applyLogFilters(false);
}

function toggleLogExpand(el) {
  const cell = el.closest('td');
  if (!cell) return;
  const msgEl = cell.querySelector('.log-msg');
  if (!msgEl) return;
  const clamped = msgEl.classList.toggle('log-msg-clamped');
  el.textContent = clamped ? '展开全部' : '收起';
}

function copyLogEntry(el) {
  const entry = logEntries[Number(el.dataset.idx)];
  if (!entry) return;
  copyLogText(entry.raw).then(function (ok) {
    toast(ok ? '已复制本条日志' : '复制失败，请手动选择文本复制');
  });
}

function copyLogText(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    return navigator.clipboard.writeText(text).then(
      function () { return true; },
      function () { return legacyCopyLogText(text); }
    );
  }
  return Promise.resolve(legacyCopyLogText(text));
}

function legacyCopyLogText(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
  document.body.removeChild(ta);
  return ok;
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
  filterLogLevel: function (el) { filterLogLevel(el); },
  searchLogs: function (el) { searchLogs(el); },
  jumpToLogBottom: function () { jumpToLogBottom(); },
  copyLogEntry: function (el) { copyLogEntry(el); },
  toggleLogExpand: function (el) { toggleLogExpand(el); },
});

bindLogsScroll();

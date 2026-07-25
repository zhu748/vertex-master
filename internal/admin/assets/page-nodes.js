document.getElementById('nodesBody').addEventListener('click', function (e) {
  var btn = e.target.closest('[data-action]');
  if (!btn) return;
  var uri = btn.dataset.uri;
  var action = btn.dataset.action;
  if (action === 'use-node') useNode(uri, btn);
  else if (action === 'unuse-node') unuseNode(uri, btn);
  else if (action === 'delete-node') delNode(uri, btn);
  else if (action === 'test-node') testSingleNode(uri, btn);
  else if (action === 'enable-node') enableNode(uri, btn);
});

var curNodePage = 1;
var nodePageSize = 50;
var totalNodePages = 1;
var totalNodeCount = 0;
var filteredNodeCount = 0;
var cachedNodesList = [];
window.selectedNodeURIs = window.selectedNodeURIs || new Set();
var testProgressTimer = null;
var cachedProxySubscriptions = [];
var nodesLoadSequence = 0;
var proxySubscriptionsLoadSequence = 0;

function setActionBusy(button, busy, busyText) {
  if (!button) return;
  if (busy) {
    if (!button.dataset.originalText) button.dataset.originalText = button.textContent;
    button.disabled = true;
    button.textContent = busyText || '处理中...';
  } else {
    button.disabled = false;
    button.textContent = button.dataset.originalText || button.textContent;
    delete button.dataset.originalText;
  }
}

function maskedSubscriptionURL(rawURL) {
  try {
    var parsed = new URL(rawURL);
    return parsed.origin;
  } catch (e) {
    return '地址格式异常';
  }
}

var proxySubEnabledInput = document.getElementById('proxySubEnabled');
if (proxySubEnabledInput) {
  proxySubEnabledInput.addEventListener('change', updateProxySubscriptionSaveLabel);
}
var batchTestIncludeDisabledInput = document.getElementById('batchTestIncludeDisabled');
var batchTestRecoverDisabledInput = document.getElementById('batchTestRecoverDisabled');
if (batchTestIncludeDisabledInput && batchTestRecoverDisabledInput) {
  var updateBatchRecoveryState = function () {
    batchTestRecoverDisabledInput.disabled = !batchTestIncludeDisabledInput.checked;
    if (batchTestRecoverDisabledInput.disabled) batchTestRecoverDisabledInput.checked = false;
  };
  batchTestIncludeDisabledInput.addEventListener('change', updateBatchRecoveryState);
  updateBatchRecoveryState();
}

function currentNodeListOptions(extra) {
  var options = {
    query: document.getElementById('nodeSearch').value.trim(),
    type: document.getElementById('nodeTypeFilter').value,
    status: document.getElementById('nodeStatusFilter').value,
    source: document.getElementById('nodeSourceFilter').value
  };
  return Object.assign(options, extra || {});
}

function applyNodeFilters() {
  curNodePage = 1;
  window.selectedNodeURIs.clear();
  loadNodes();
}

var nodeSearchTimer = null;
var nodeSearchInput = document.getElementById('nodeSearch');
if (nodeSearchInput) {
  nodeSearchInput.addEventListener('input', function () {
    clearTimeout(nodeSearchTimer);
    nodeSearchTimer = setTimeout(applyNodeFilters, 250);
  });
}
['nodeTypeFilter', 'nodeStatusFilter', 'nodeSourceFilter'].forEach(function (id) {
  var element = document.getElementById(id);
  if (element) element.addEventListener('change', applyNodeFilters);
});
var nodePageSizeInput = document.getElementById('nodePageSize');
if (nodePageSizeInput) {
  nodePageSizeInput.addEventListener('change', function () {
    nodePageSize = Number(this.value) || 50;
    curNodePage = 1;
    loadNodes();
  });
}

function changeNodePage(p) {
  if (p < 1) p = 1;
  if (p > totalNodePages) p = totalNodePages;
  curNodePage = p;
  loadNodes();
}

function updateSelectHeaderAndBanner() {
  var mainCb = document.getElementById('selectAllNodesCheckbox');
  var banner = document.getElementById('crossPageSelectBanner');
  var bannerText = document.getElementById('crossPageSelectText');
  var bannerTotal = document.getElementById('crossPageSelectTotal');

  if (!cachedNodesList.length) {
    if (mainCb) mainCb.checked = false;
    if (banner) banner.style.display = 'none';
    return;
  }

  var pageNodes = cachedNodesList;

  var allPageChecked = pageNodes.length > 0 && pageNodes.every(function (n) { return window.selectedNodeURIs.has(n.raw_uri); });
  if (mainCb) mainCb.checked = allPageChecked;

  if (allPageChecked && filteredNodeCount > pageNodes.length && window.selectedNodeURIs.size < filteredNodeCount) {
    if (banner) {
      banner.style.display = 'block';
      if (bannerText) bannerText.textContent = '当前已选择本页 ' + pageNodes.length + ' 个节点。';
      if (bannerTotal) bannerTotal.textContent = filteredNodeCount;
    }
  } else {
    if (banner) banner.style.display = 'none';
  }
}

async function selectAllNodesAcrossPages() {
  try {
    var data = await API.nodes.list(currentNodeListOptions({ uris_only: true }));
    (data.uris || []).forEach(function (uri) { window.selectedNodeURIs.add(uri); });
    var cbs = document.querySelectorAll('.node-select-cb');
    cbs.forEach(function (cb) { cb.checked = true; });
    var banner = document.getElementById('crossPageSelectBanner');
    if (banner) banner.style.display = 'none';
    var mainCb = document.getElementById('selectAllNodesCheckbox');
    if (mainCb) mainCb.checked = true;
    toast('已选择当前筛选结果中的全部 ' + window.selectedNodeURIs.size + ' 个节点');
  } catch (e) {
    toast('全选失败: ' + e.message);
  }
}

async function loadNodes() {
  var loadSequence = ++nodesLoadSequence;
  try {
    const sd = await API.settings.get();
    if (loadSequence !== nodesLoadSequence) return;
    if (typeof curSettings !== 'undefined') {
      curSettings = sd.settings || sd;
    }
    const gpEl = document.getElementById('globalProxy');
    if (gpEl && (sd.settings || sd).proxy_url !== undefined) {
      gpEl.value = (sd.settings || sd).proxy_url;
    }
  } catch (e) { }

  let d;
  try {
    d = await API.nodes.list(currentNodeListOptions({
      page: curNodePage,
      page_size: nodePageSize
    }));
  } catch (e) {
    if (loadSequence === nodesLoadSequence) {
      toast('节点列表加载失败: ' + e.message);
    }
    return;
  }
  if (loadSequence !== nodesLoadSequence) return;
  const nodes = d.nodes || [];
  cachedNodesList = nodes;
  totalNodeCount = Number(d.overall_total !== undefined ? d.overall_total : d.total) || 0;
  filteredNodeCount = Number(d.total) || 0;
  curNodePage = Number(d.page) || 1;
  totalNodePages = Number(d.total_pages) || 1;

  try {
    const prog = await API.nodes.testProgress();
    if (prog && prog.running) {
      showTestProgressUI(prog);
      startTestProgressPolling();
    } else if (!testProgressTimer) {
      const progressEl = document.getElementById('testProgress');
      if (progressEl) progressEl.style.display = 'none';
      setActionBusy(document.getElementById('batchTestStartBtn'), false);
      currentTestPaused = false;
    }
  } catch (e) { }
  if (loadSequence !== nodesLoadSequence) return;

  const enabledCount = Number(d.enabled_count) || 0;
  const disabledCount = Number(d.disabled_count) || 0;
  var summary = '\u5F53\u524D\u5171 ' + totalNodeCount + ' \u4E2A\u8282\u70B9\uFF08\u542F\u7528 ' + enabledCount + ' / \u7981\u7528 ' + disabledCount + '\uFF09';
  var poolStats = d.pool_stats || {};
  summary += ' · 可用 ' + (poolStats.healthy || 0) +
    ' · 冷却 ' + (poolStats.cooling || 0) +
    ' · 待恢复 ' + (poolStats.unhealthy || 0) +
    ' · 未测试 ' + (poolStats.untested || 0);
  var scheduler = d.health_scheduler || {};
  if (scheduler.enabled) {
    summary += scheduler.running
      ? ' · 自动巡检中'
      : (scheduler.last_run_at
        ? ' · 上轮巡检 ' + (scheduler.checked || 0) + ' 个（可用 ' +
          (scheduler.succeeded || 0) + ' / 失败 ' + (scheduler.failed || 0) + '）'
        : ' · 等待首次自动巡检');
  }
  if (filteredNodeCount !== totalNodeCount) summary += '，筛选结果 ' + filteredNodeCount + ' 个';
  document.getElementById('nodesSummary').textContent = summary;

  const startIdx = (curNodePage - 1) * nodePageSize;
  const endIdx = startIdx + nodes.length;
  const pageNodes = nodes;

  const tbody = document.getElementById('nodesBody');
  const frag = document.createDocumentFragment();

  if (pageNodes.length === 0) {
    var tr = document.createElement('tr');
    var td = document.createElement('td');
    td.colSpan = 5;
    td.style.cssText = 'color:var(--text-dim); text-align:center;';
    td.textContent = '\u6682\u65E0\u8282\u70B9';
    tr.appendChild(td);
    frag.appendChild(tr);
  } else {
    for (var i = 0; i < pageNodes.length; i++) {
      var n = pageNodes[i];
      var tr = document.createElement('tr');

      var cbTd = document.createElement('td');
      cbTd.style.cssText = 'text-align:center;vertical-align:middle;';
      var cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.className = 'node-select-cb';
      cb.dataset.uri = n.raw_uri;
      cb.checked = window.selectedNodeURIs.has(n.raw_uri);
      cb.setAttribute('aria-label', '选择节点 ' + n.name);
      cb.onchange = function () {
        if (this.checked) window.selectedNodeURIs.add(this.dataset.uri);
        else window.selectedNodeURIs.delete(this.dataset.uri);
        updateSelectHeaderAndBanner();
      };
      cbTd.appendChild(cb);
      tr.appendChild(cbTd);

      var nameTd = document.createElement('td');
      var nameDiv = document.createElement('div');
      nameDiv.style.cssText = 'font-weight:600;font-size:13.5px;color:var(--text);';
      nameDiv.textContent = n.name;
      var isLocked = n.raw_uri === curSettings.active_node_uri;
      if (isLocked) {
        var badge = document.createElement('span');
        badge.className = 'pill on';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;';
        badge.textContent = '\u9501\u5B9A\u4F7F\u7528\u4E2D';
        nameDiv.appendChild(badge);
      } else if (n.disabled) {
        var badge = document.createElement('span');
        badge.className = 'pill off';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;background:rgba(236,138,124,0.16);color:var(--red);';
        badge.textContent = '\u5DF2\u7981\u7528';
        nameDiv.appendChild(badge);
      } else {
        var badge = document.createElement('span');
        badge.className = 'pill off';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;background:rgba(143,208,232,0.15);color:var(--blue);';
        badge.textContent = '\u5019\u9009';
        nameDiv.appendChild(badge);
      }
      nameTd.appendChild(nameDiv);

      var serverInfo = '';
      try {
        if (n.raw_uri.startsWith('vmess://')) {
          var b64Str = n.raw_uri.slice(8).split(/[?#]/)[0];
          var b = atob(b64Str.replace(/-/g, '+').replace(/_/g, '/'));
          var info = JSON.parse(b);
          serverInfo = (info.add || 'unknown') + ':' + (info.port || '');
        } else {
          var urlObj = new URL(n.raw_uri);
          serverInfo = urlObj.hostname + ':' + (urlObj.port || '443');
        }
      } catch (e) {
        serverInfo = '\u914D\u7F6E\u683C\u5F0F\u590D\u6742';
      }

      var addrDiv = document.createElement('div');
      addrDiv.style.cssText = 'font-size:11px;color:var(--text-dim);margin-top:4px;';
      addrDiv.appendChild(document.createTextNode('\u5730\u5740: '));
      var code = document.createElement('code');
      code.style.cssText = 'font-size:11px;background:rgba(0,0,0,0.25);color:var(--blue);padding:1px 4px;border-radius:4px;';
      code.textContent = serverInfo;
      addrDiv.appendChild(code);
      nameTd.appendChild(addrDiv);
      tr.appendChild(nameTd);

      var typeTd = document.createElement('td');
      var typeCode = document.createElement('code');
      typeCode.textContent = _tmap[n.type.toLowerCase()] || n.type.toUpperCase();
      typeTd.appendChild(typeCode);
      tr.appendChild(typeTd);

      var statusTd = document.createElement('td');
      var health = d.health[n.raw_uri];
      if (!health || (!health.last_success_at && !health.last_fail_at)) {
        var pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(195,182,164,0.15);color:var(--text-dim);';
        pill.textContent = '\u672A\u6D4B\u8BD5';
        statusTd.appendChild(pill);
      } else if (health.consecutive_failures === 0) {
        var ms = health.last_test_ms ? Math.round(health.last_test_ms) + 'ms' : '';
        var pill = document.createElement('span');
        pill.className = 'pill on';
        pill.style.cssText = 'background:rgba(132,214,160,0.16);color:var(--green);margin-right:5px;';
        pill.textContent = '\u6D4B\u8BD5\u901A\u8FC7 ' + ms;
        statusTd.appendChild(pill);
        var avail = document.createElement('span');
        avail.style.cssText = 'color:var(--green);font-weight:600;font-size:11px;';
        avail.textContent = '\u53EF\u7528';
        statusTd.appendChild(avail);
      } else {
        var pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(236,138,124,0.16);color:var(--red);margin-right:5px;';
        var cooling = Number(health.cooldown_until) > Math.floor(Date.now() / 1000);
        pill.textContent = cooling ? '\u51B7\u5374\u4E2D' : '\u5F85\u6062\u590D';
        statusTd.appendChild(pill);

        if (health.last_test_error) {
          var errSpan = document.createElement('div');
          errSpan.className = 'node-err-msg';
          errSpan.textContent = health.last_test_error;
          statusTd.appendChild(errSpan);
        }
        if (cooling) {
          var cooldownSpan = document.createElement('div');
          cooldownSpan.className = 'node-err-msg';
          cooldownSpan.textContent = '冷却至 ' + new Date(Number(health.cooldown_until) * 1000).toLocaleTimeString();
          statusTd.appendChild(cooldownSpan);
        }
      }
      tr.appendChild(statusTd);

      var actionTd = document.createElement('td');
      actionTd.style.cssText = 'text-align:right;white-space:nowrap;';
      var testBtn = document.createElement('button');
      testBtn.className = 'btn ghost';
      testBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;';
      testBtn.dataset.action = 'test-node';
      testBtn.dataset.uri = n.raw_uri;
      testBtn.textContent = '\u6D4B\u8BD5';
      actionTd.appendChild(testBtn);
      if (n.disabled) {
        var enableBtn = document.createElement('button');
        enableBtn.className = 'btn ghost';
        enableBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;color:var(--green);';
        enableBtn.dataset.action = 'enable-node';
        enableBtn.dataset.uri = n.raw_uri;
        enableBtn.textContent = '\u542F\u7528';
        actionTd.appendChild(enableBtn);
      }
      if (isLocked) {
        var unuseBtn = document.createElement('button');
        unuseBtn.className = 'btn ghost';
        unuseBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;color:var(--gold);';
        unuseBtn.dataset.action = 'unuse-node';
        unuseBtn.dataset.uri = n.raw_uri;
        unuseBtn.textContent = '取消锁定';
        actionTd.appendChild(unuseBtn);
      } else {
        var useBtn = document.createElement('button');
        useBtn.className = 'btn ghost';
        useBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;';
        useBtn.dataset.action = 'use-node';
        useBtn.dataset.uri = n.raw_uri;
        useBtn.textContent = '\u9501\u5B9A\u4F7F\u7528';
        actionTd.appendChild(useBtn);
      }
      var delBtn = document.createElement('button');
      delBtn.className = 'btn danger';
      delBtn.style.cssText = 'padding:4px 10px;font-size:12px;';
      delBtn.dataset.action = 'delete-node';
      delBtn.dataset.uri = n.raw_uri;
      delBtn.textContent = '\u5220\u9664';
      actionTd.appendChild(delBtn);
      tr.appendChild(actionTd);
      frag.appendChild(tr);
    }
  }

  tbody.textContent = '';
  tbody.appendChild(frag);

  const pageNumDisplay = document.getElementById('nodesPageNumDisplay');
  if (pageNumDisplay) pageNumDisplay.textContent = curNodePage + ' / ' + totalNodePages;
  const pageInfo = document.getElementById('nodesPaginationInfo');
  if (pageInfo) pageInfo.textContent = filteredNodeCount > 0 ? ('显示第 ' + (startIdx + 1) + ' - ' + endIdx + ' 条，共 ' + filteredNodeCount + ' 条') : '共 0 条';

  const btnFirst = document.getElementById('btnPageFirst');
  const btnPrev = document.getElementById('btnPagePrev');
  const btnNext = document.getElementById('btnPageNext');
  const btnLast = document.getElementById('btnPageLast');
  if (btnFirst) btnFirst.disabled = curNodePage <= 1;
  if (btnPrev) btnPrev.disabled = curNodePage <= 1;
  if (btnNext) btnNext.disabled = curNodePage >= totalNodePages;
  if (btnLast) btnLast.disabled = curNodePage >= totalNodePages;

  updateSelectHeaderAndBanner();
  await loadProxySubscriptions();
}

async function addStandardProxy() {
  var address = document.getElementById('manualProxyAddress').value.trim();
  if (!address) return toast('请填写代理地址');
  var button = document.getElementById('addStandardProxyBtn');
  if (button && button.disabled) return;
  setActionBusy(button, true, '正在添加...');
  try {
    await API.nodes.addProxy({
      type: document.getElementById('manualProxyType').value,
      address: address,
      name: document.getElementById('manualProxyName').value.trim(),
      username: document.getElementById('manualProxyUsername').value.trim(),
      password: document.getElementById('manualProxyPassword').value
    });
    document.getElementById('manualProxyAddress').value = '';
    document.getElementById('manualProxyName').value = '';
    document.getElementById('manualProxyUsername').value = '';
    document.getElementById('manualProxyPassword').value = '';
    await loadNodes();
    toast('代理已加入节点池');
  } catch (e) {
    toast('添加失败: ' + e.message);
  } finally {
    setActionBusy(button, false);
  }
}

async function loadProxySubscriptions() {
  var tbody = document.getElementById('proxySubsBody');
  if (!tbody) return;
  var loadSequence = ++proxySubscriptionsLoadSequence;
  try {
    var data = await API.subscriptions.list();
    if (loadSequence !== proxySubscriptionsLoadSequence) return;
    cachedProxySubscriptions = data.subscriptions || [];
    tbody.textContent = '';
    if (!cachedProxySubscriptions.length) {
      var emptyRow = document.createElement('tr');
      var emptyCell = document.createElement('td');
      emptyCell.colSpan = 6;
      emptyCell.textContent = '暂无自动代理订阅';
      emptyCell.style.cssText = 'text-align:center;color:var(--text-dim);padding:18px;';
      emptyRow.appendChild(emptyCell);
      tbody.appendChild(emptyRow);
      return;
    }
    cachedProxySubscriptions.forEach(function (sub) {
      var tr = document.createElement('tr');
      var nameTd = document.createElement('td');
      var nameDiv = document.createElement('div');
      nameDiv.style.cssText = 'font-weight:600;';
      nameDiv.textContent = sub.name;
      if (sub.managed_key) {
        var managedBadge = document.createElement('span');
        managedBadge.className = 'pill on';
        managedBadge.style.cssText = 'margin-left:6px;font-size:10px;vertical-align:1px;';
        managedBadge.textContent = '环境托管';
        managedBadge.title = '该订阅由 Render 环境变量管理';
        nameDiv.appendChild(managedBadge);
      }
      var urlDiv = document.createElement('div');
      urlDiv.style.cssText = 'font-size:11px;color:var(--text-dim);margin-top:3px;word-break:break-all;';
      urlDiv.textContent = maskedSubscriptionURL(sub.url);
      nameTd.appendChild(nameDiv);
      nameTd.appendChild(urlDiv);
      tr.appendChild(nameTd);

      var typeTd = document.createElement('td');
      typeTd.textContent = (sub.proxy_type || 'auto').toUpperCase() + ' / ' + sub.refresh_interval_minutes + ' 分钟';
      tr.appendChild(typeTd);

      var countTd = document.createElement('td');
      countTd.textContent = String(sub.node_count || 0);
      tr.appendChild(countTd);

      var refreshTd = document.createElement('td');
      if (sub.last_refreshed_at) {
        refreshTd.textContent = new Date(sub.last_refreshed_at * 1000).toLocaleString();
      } else if (sub.last_attempt_at) {
        refreshTd.textContent = '尚未成功';
      } else {
        refreshTd.textContent = '尚未刷新';
      }
      tr.appendChild(refreshTd);

      var statusTd = document.createElement('td');
      var statusPill = document.createElement('span');
      statusPill.className = sub.last_error ? 'pill off' : (sub.enabled ? 'pill on' : 'pill off');
      statusPill.textContent = sub.last_error ? '刷新失败' : (sub.enabled ? '自动刷新' : '已停用');
      statusTd.appendChild(statusPill);
      if (sub.last_error) {
        var errorDiv = document.createElement('div');
        errorDiv.className = 'proxy-sub-error';
        errorDiv.textContent = sub.last_error;
        errorDiv.title = sub.last_error;
        statusTd.appendChild(errorDiv);
      }
      tr.appendChild(statusTd);

      var actionTd = document.createElement('td');
      actionTd.style.cssText = 'text-align:right;white-space:nowrap;';
      var actions = [
        ['刷新', 'ghost', function (button) { refreshProxySubscription(sub.id, button); }]
      ];
      if (!sub.managed_key) {
        actions.unshift(
          ['编辑', 'ghost', function () { editProxySubscription(sub.id); }],
          [sub.enabled ? '停用' : '启用', 'ghost', function (button) { toggleProxySubscriptionEnabled(sub.id, !sub.enabled, button); }]
        );
        actions.push(['删除', 'danger', function (button) { deleteProxySubscription(sub.id, button); }]);
      } else {
        actionTd.title = '请在 Render 环境变量中修改或移除该订阅';
      }
      actions.forEach(function (spec) {
        var btn = document.createElement('button');
        btn.className = 'btn ' + spec[1];
        btn.style.cssText = 'padding:4px 8px;font-size:12px;margin-left:4px;';
        btn.textContent = spec[0];
        btn.onclick = function () { spec[2](btn); };
        actionTd.appendChild(btn);
      });
      tr.appendChild(actionTd);
      tbody.appendChild(tr);
    });
  } catch (e) {
    tbody.textContent = '';
    var row = document.createElement('tr');
    var cell = document.createElement('td');
    cell.colSpan = 6;
    cell.textContent = '订阅加载失败: ' + e.message;
    row.appendChild(cell);
    tbody.appendChild(row);
  }
}

async function saveProxySubscription() {
  var url = document.getElementById('proxySubUrl').value.trim();
  if (!url) return toast('请填写文本代理订阅 URL');
  var interval = Number(document.getElementById('proxySubInterval').value);
  if (!Number.isFinite(interval) || interval < 1 || interval > 10080) {
    return toast('刷新间隔必须在 1 到 10080 分钟之间');
  }
  var button = document.getElementById('proxySubSaveBtn');
  if (button && button.disabled) return;
  var enabled = document.getElementById('proxySubEnabled').checked;
  setActionBusy(button, true, enabled ? '保存并拉取中...' : '正在保存...');
  try {
    var result = await API.subscriptions.save({
      id: Number(document.getElementById('proxySubId').value) || 0,
      name: document.getElementById('proxySubName').value.trim(),
      url: url,
      proxy_type: document.getElementById('proxySubType').value,
      refresh_interval_minutes: Math.round(interval),
      enabled: enabled,
      refresh_now: enabled
    });
    resetProxySubscriptionForm();
    await loadNodes();
    if (result.refresh_ok === false) {
      toast('订阅已保存，但本次拉取失败：' + (result.refresh_error || '未知错误'));
    } else if (!enabled) {
      toast('订阅已保存，自动刷新已停用');
    } else {
      toast('订阅已保存，当前导入 ' + (result.count || 0) + ' 个代理');
    }
  } catch (e) {
    await loadProxySubscriptions();
    toast('保存失败: ' + e.message);
  } finally {
    setActionBusy(button, false);
    updateProxySubscriptionSaveLabel();
  }
}

function editProxySubscription(id) {
  var sub = cachedProxySubscriptions.find(function (item) { return item.id === id; });
  if (!sub) return;
  document.getElementById('proxySubId').value = String(sub.id);
  document.getElementById('proxySubName').value = sub.name || '';
  document.getElementById('proxySubUrl').value = sub.url || '';
  document.getElementById('proxySubType').value = sub.proxy_type || 'auto';
  document.getElementById('proxySubInterval').value = String(sub.refresh_interval_minutes || 60);
  document.getElementById('proxySubEnabled').checked = !!sub.enabled;
  updateProxySubscriptionSaveLabel();
  document.getElementById('proxySubUrl').focus();
}

function resetProxySubscriptionForm() {
  document.getElementById('proxySubId').value = '';
  document.getElementById('proxySubName').value = '';
  document.getElementById('proxySubUrl').value = '';
  document.getElementById('proxySubType').value = 'auto';
  document.getElementById('proxySubInterval').value = '60';
  document.getElementById('proxySubEnabled').checked = true;
  updateProxySubscriptionSaveLabel();
}

function updateProxySubscriptionSaveLabel() {
  var button = document.getElementById('proxySubSaveBtn');
  if (!button || button.disabled) return;
  var editing = Number(document.getElementById('proxySubId').value) > 0;
  var enabled = document.getElementById('proxySubEnabled').checked;
  button.textContent = enabled
    ? (editing ? '保存修改并刷新' : '保存并立即拉取')
    : (editing ? '保存修改' : '保存订阅');
}

async function toggleProxySubscriptionEnabled(id, enabled, button) {
  var sub = cachedProxySubscriptions.find(function (item) { return item.id === id; });
  if (!sub || (button && button.disabled)) return;
  setActionBusy(button, true, enabled ? '启用中...' : '停用中...');
  try {
    await API.subscriptions.save({
      id: sub.id,
      name: sub.name,
      url: sub.url,
      proxy_type: sub.proxy_type,
      refresh_interval_minutes: sub.refresh_interval_minutes,
      enabled: enabled,
      refresh_now: false
    });
    await loadProxySubscriptions();
    toast(enabled ? '已启用自动刷新' : '已停用自动刷新');
  } catch (e) {
    toast('操作失败: ' + e.message);
  } finally {
    setActionBusy(button, false);
  }
}

async function refreshProxySubscription(id, button) {
  if (button && button.disabled) return;
  setActionBusy(button, true, '刷新中...');
  toast('正在拉取代理列表...');
  try {
    var result = await API.subscriptions.refresh(id);
    await loadNodes();
    toast('刷新成功，代理池现有 ' + (result.count || 0) + ' 个该订阅节点');
  } catch (e) {
    await loadProxySubscriptions();
    toast('刷新失败: ' + e.message);
  } finally {
    setActionBusy(button, false);
  }
}

async function deleteProxySubscription(id, button) {
  if (!confirm('删除该订阅及其导入的代理节点？')) return;
  if (button && button.disabled) return;
  setActionBusy(button, true, '删除中...');
  try {
    await API.subscriptions.del(id);
    resetProxySubscriptionForm();
    await loadNodes();
    toast('订阅及其节点已删除');
  } catch (e) {
    toast('删除失败: ' + e.message);
  } finally {
    setActionBusy(button, false);
  }
}

async function addAndFetchSub() {
  const u = $('#subUrl').value.trim();
  if (!u) return toast('请填订阅 URL');
  toast('正在拉取...');
  try {
    const res = await API.subscriptions.fetch(u);
    $('#subUrl').value = '';
    await loadNodes();
    toast('拉取成功，导入了 ' + (res.count || 0) + ' 个节点');
  } catch (e) {
    toast('拉取失败: ' + e.message);
  }
}

async function testAllNodes() {
  var button = document.getElementById('batchTestStartBtn');
  if (button && button.disabled) return;
  var includeDisabled = document.getElementById('batchTestIncludeDisabled').checked;
  var recoverDisabled = document.getElementById('batchTestRecoverDisabled').checked;
  var maxNodes = Number(document.getElementById('batchTestMaxNodes').value);
  var concurrency = Number(document.getElementById('batchTestConcurrency').value);
  var timeoutSeconds = Number(document.getElementById('batchTestTimeoutSeconds').value);
  if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 20) {
    return toast('测速并发数必须是 1 到 20 的整数');
  }
  if (!Number.isInteger(timeoutSeconds) || timeoutSeconds < 3 || timeoutSeconds > 60) {
    return toast('单节点超时必须是 3 到 60 秒的整数');
  }
  var taskCapacity = Math.min(1000, Math.floor(3540 / timeoutSeconds) * concurrency);
  if (!Number.isInteger(maxNodes) || maxNodes < 1 || maxNodes > taskCapacity) {
    return toast('当前并发和超时下，本轮节点数必须是 1 到 ' + taskCapacity + ' 的整数');
  }

  setActionBusy(button, true, '测速进行中...');
  try {
    var result = await API.nodes.testAll({
      include_disabled: includeDisabled,
      recover_disabled: includeDisabled && recoverDisabled,
      max_nodes: maxNodes,
      concurrency: concurrency,
      timeout_seconds: timeoutSeconds
    });
    toast('后台测速任务已启动，本轮 ' + (result.count || 0) + ' 个节点');
    startTestProgressPolling();
  } catch (e) {
    setActionBusy(button, false);
    toast('启动批量测速失败: ' + e.message);
  }
}

let currentTestPaused = false;

function showTestProgressUI(prog) {
  const progressEl = document.getElementById('testProgress');
  const progressText = document.getElementById('testProgressText');
  const progressFill = document.getElementById('testProgressFill');
  const progressDetail = document.getElementById('testProgressDetail');
  const btnPause = document.getElementById('btnTestPauseResume');
  if (!progressEl) return;
  progressEl.style.display = 'block';
  setActionBusy(document.getElementById('batchTestStartBtn'), true, '测速进行中...');
  currentTestPaused = !!prog.paused;
  if (btnPause) {
    btnPause.textContent = currentTestPaused ? '恢复' : '暂停';
    btnPause.className = 'btn ghost';
    btnPause.disabled = !!prog.terminated;
  }
  const done = prog.done || 0;
  const total = prog.total || 1;
  const ok = prog.ok_count || 0;
  const failed = prog.fail_count || 0;
  progressFill.style.width = Math.min(100, Math.round(done / total * 100)) + '%';
  const statusStr = prog.terminated ? '正在终止' : (currentTestPaused ? '已暂停' : '测试中');
  progressText.textContent = statusStr + ' ' + done + '/' + total + ' \u00B7 \u901A\u8FC7 ' + ok + ' \u00B7 \u5931\u8D25 ' + failed;
  progressDetail.textContent = '当前状态: ' + (prog.current_node || '');
}

async function toggleTestPauseResume() {
  try {
    if (currentTestPaused) {
      await API.nodes.testResume();
      currentTestPaused = false;
      const btnPause = document.getElementById('btnTestPauseResume');
      if (btnPause) {
        btnPause.textContent = '暂停';
        btnPause.className = 'btn ghost';
      }
      const progressText = document.getElementById('testProgressText');
      if (progressText && progressText.textContent.startsWith('已暂停')) {
        progressText.textContent = progressText.textContent.replace(/^已暂停/, '测试中');
      }
      startTestProgressPolling();
      toast('已恢复批量测速');
    } else {
      await API.nodes.testPause();
      currentTestPaused = true;
      const btnPause = document.getElementById('btnTestPauseResume');
      if (btnPause) {
        btnPause.textContent = '恢复';
        btnPause.className = 'btn ghost';
      }
      const progressText = document.getElementById('testProgressText');
      if (progressText && progressText.textContent.startsWith('测试中')) {
        progressText.textContent = progressText.textContent.replace(/^测试中/, '已暂停');
      }
      toast('批量测速已暂停');
    }
  } catch (e) {
    toast(e.message || '操作失败');
  }
}

async function terminateTestAll() {
  try {
    await API.nodes.testTerminate();
    currentTestPaused = false;
    const progressText = document.getElementById('testProgressText');
    if (progressText) progressText.textContent = '正在终止批量测速...';
    startTestProgressPolling();
    toast('正在终止批量测速...');
  } catch (e) {
    toast(e.message || '操作失败');
  }
}

async function pollTestProgress() {
  const nodesPage = document.getElementById('page-nodes');
  if (nodesPage && nodesPage.classList.contains('hidden')) {
    if (testProgressTimer) {
      clearInterval(testProgressTimer);
      testProgressTimer = null;
    }
    return;
  }
  try {
    const prog = await API.nodes.testProgress();
    if (prog && prog.running) {
      showTestProgressUI(prog);
      return;
    }
    if (testProgressTimer) {
      clearInterval(testProgressTimer);
      testProgressTimer = null;
    }
    const progressEl = document.getElementById('testProgress');
    if (progressEl) progressEl.style.display = 'none';
    setActionBusy(document.getElementById('batchTestStartBtn'), false);
    var incomplete = Number(prog && prog.incomplete) || 0;
    toast(incomplete
      ? '批量测速结束，已测试 ' + (prog.done || 0) + '，未完成 ' + incomplete
      : '批量测速完成，共测试 ' + (prog.done || 0) + ' 个节点');
    loadNodes();
  } catch (e) { }
}

function startTestProgressPolling() {
  if (testProgressTimer) return;
  testProgressTimer = setInterval(pollTestProgress, 1000);
  pollTestProgress();
}

async function dedupNodes() { await API.nodes.dedup(); loadNodes(); toast('去重完成'); }
async function deleteDisabledNodes() { await API.nodes.deleteDisabled(); loadNodes(); toast('清理完成'); }
async function sortNodesByLatency() { await API.nodes.sort(false); await loadNodes(); toast('已按延迟顺序重排节点'); }
async function sortNodesByLatencyDesc() { await API.nodes.sort(true); await loadNodes(); toast('已按延迟降序重排节点'); }

async function exportNodes() {
  try {
    const d = await API.nodes.list({ uris_only: true });
    const uris = d.uris || [];
    if (uris.length === 0) {
      toast('没有可导出的节点');
      return;
    }
    const text = uris.join('\n');
    const blob = new Blob([text], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'nodes.txt';
    a.click();
    URL.revokeObjectURL(url);
    toast('已导出 ' + uris.length + ' 个节点');
  } catch (e) {
    toast('导出失败: ' + e.message);
  }
}

async function testSingleNode(uri, button) {
  if (button && button.disabled) return;
  setActionBusy(button, true, '测试中...');
  try {
    const result = await API.nodes.test(uri, { auto_disable: false });
    const msg = result.ok
      ? '测试通过 ' + Math.round(result.elapsed_ms) + 'ms'
      : '测试失败 ' + (result.error || '');
    toast(msg);
    await loadNodes();
  } catch (e) {
    toast('测试出错: ' + e.message);
  } finally {
    setActionBusy(button, false);
  }
}

async function enableNode(uri, button) {
  if (button && button.disabled) return;
  setActionBusy(button, true, '启用中...');
  try {
    await API.nodes.enable(uri);
    await loadNodes();
    toast('已启用该节点');
  } catch (e) {
    toast('启用失败: ' + e.message);
  } finally {
    setActionBusy(button, false);
  }
}

async function useNode(uri, button) {
  if (button && button.disabled) return;
  setActionBusy(button, true, '锁定中...');
  try {
    await API.useNode(uri);
    await loadSettings();
    await loadNodes();
    toast('已锁定使用该节点，并关闭并发池');
  } catch (e) {
    toast('锁定失败: ' + e.message);
  } finally {
    setActionBusy(button, false);
  }
}

async function unuseNode(uri, button) {
  if (button && button.disabled) return;
  setActionBusy(button, true, '取消中...');
  try {
    await API.useNode('');
    await loadSettings();
    await loadNodes();
    toast('已取消锁定，并恢复并发池');
  } catch (e) {
    toast('取消锁定失败: ' + e.message);
  } finally {
    setActionBusy(button, false);
  }
}

async function delNode(uri, button) {
  if (!confirm('删除该节点？') || (button && button.disabled)) return;
  setActionBusy(button, true, '删除中...');
  try {
    await API.nodes.delete(uri);
    await loadNodes();
    toast('已删除');
  } catch (e) {
    toast('删除失败: ' + e.message);
  } finally {
    setActionBusy(button, false);
  }
}

function getSelectedNodeURIs() {
  return Array.from(window.selectedNodeURIs);
}

async function toggleSelectAllNodes() {
  if (window.selectedNodeURIs.size >= filteredNodeCount && filteredNodeCount > 0) {
    window.selectedNodeURIs.clear();
    document.querySelectorAll('.node-select-cb').forEach(function (cb) { cb.checked = false; });
    updateSelectHeaderAndBanner();
    toast('已取消选择');
  } else {
    await selectAllNodesAcrossPages();
  }
}

function toggleSelectAllNodesCheckbox(mainCb) {
  cachedNodesList.forEach(function (n) {
    if (mainCb.checked) window.selectedNodeURIs.add(n.raw_uri);
    else window.selectedNodeURIs.delete(n.raw_uri);
  });
  document.querySelectorAll('.node-select-cb').forEach(function (cb) { cb.checked = mainCb.checked; });
  updateSelectHeaderAndBanner();
}

async function batchEnableSelectedNodes() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要批量操作的节点');
  toast('批量启用中...');
  try {
    await API.nodes.batchEnable(uris);
    window.selectedNodeURIs.clear();
    await loadNodes();
    toast('已成功启用 ' + uris.length + ' 个节点');
  } catch (e) { toast('操作失败: ' + e.message); }
}

async function batchDisableSelectedNodes() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要批量操作的节点');
  toast('批量禁用中...');
  try {
    await API.nodes.batchDisable(uris);
    window.selectedNodeURIs.clear();
    await loadNodes();
    toast('已成功禁用 ' + uris.length + ' 个节点');
  } catch (e) { toast('操作失败: ' + e.message); }
}

async function batchDeleteSelectedNodes() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要批量操作的节点');
  if (!confirm('确定要批量删除选中 ' + uris.length + ' 个节点吗？')) return;
  toast('批量删除中...');
  try {
    await API.nodes.batchDelete(uris);
    window.selectedNodeURIs.clear();
    await loadNodes();
    toast('已成功删除 ' + uris.length + ' 个节点');
  } catch (e) { toast('操作失败: ' + e.message); }
}

function importFileNodes(replace) {
  const fileInput = document.getElementById('nodeImportFile');
  if (!fileInput.files.length) return toast('请先选择一个节点配置文件');
  if (replace && !confirm('替换模式会原子替换所有手动节点（订阅节点会保留），是否继续？')) return;
  const file = fileInput.files[0];
  const reader = new FileReader();
  toast('正在读取配置文件并解析...');
  reader.onload = async function (e) {
    const text = e.target.result;
    try {
      const res = await API.nodes.import(text, replace);
      await loadNodes();
      fileInput.value = '';
      toast(replace ? '替换成功，导入了 ' + res.count + ' 个节点' : '导入成功，追加了 ' + res.count + ' 个节点');
    } catch (err) {
      toast('文件导入解析失败: ' + err.message);
    }
  };
  reader.readAsText(file);
}

function importJsonNodes(replace) {
  const fileInput = document.getElementById('nodeJsonImportFile');
  if (!fileInput.files.length) return toast('请先选择一个 nodes.json 配置文件');
  if (replace && !confirm('替换模式会原子替换所有手动节点（订阅节点会保留），是否继续？')) return;
  const file = fileInput.files[0];
  const reader = new FileReader();
  toast('正在读取配置文件并解析...');
  reader.onload = async function (e) {
    const text = e.target.result;
    try {
      const res = await API.nodes.importJson(text, replace);
      await loadNodes();
      fileInput.value = '';
      toast(replace ? '替换成功，导入了 ' + res.count + ' 个节点' : '导入成功，追加了 ' + res.count + ' 个节点');
    } catch (err) {
      toast('nodes.json 导入解析失败: ' + err.message);
    }
  };
  reader.readAsText(file);
}

async function saveGlobalProxy() { await API.settings.put({ proxy_url: $('#globalProxy').value }); loadSettings(); toast('全局代理已保存'); }

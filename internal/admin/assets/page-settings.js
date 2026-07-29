const SETTINGS_FIELDS = [
  // 🚀 Group: pool (并发与 Token 池管理)
  { k: 'parallel_pool_enabled', label: '并发请求池', type: 'bool', group: 'pool', desc: '同时请求多个健康节点，首包到达即采纳，降低延迟' },
  { k: 'parallel_pool_retry_enabled', label: '并发池单点重试', type: 'bool', group: 'pool', desc: '默认开启；节点遇到429等可重试错误时允许节点内再次尝试' },
  { k: 'sticky_node_priority', label: '优先复用成功节点', type: 'bool', group: 'pool', desc: '优先选择近期请求成功的节点，同时保留少量未测试节点用于探索。' },
  { k: 'parallel_pool_size', label: '最大同时并发', type: 'number', max: 20, min: 1, group: 'pool', desc: '同一请求最多同时运行的代理数量（默认 5，最大 20）' },
  { k: 'proxy_failover_max_attempts', label: '单请求最多尝试代理', type: 'number', max: 100, min: 1, group: 'pool', desc: '失败或超时后可继续接力的候选总数（默认 30）' },
  { k: 'parallel_pool_delay_dynamic', label: '动态对冲延迟', type: 'bool', group: 'pool', desc: '根据节点平均响应时间动态调整并发启动间隔，平衡延迟与流量消耗' },
  { k: 'parallel_pool_delay_ms', label: '固定对冲延迟时间 (毫秒)', type: 'number', max: 10000, min: 100, group: 'pool', desc: '当前代理尚未响应时，启动下一个后备代理的间隔（默认 1000ms）' },
  { k: 'proxy_health_check_enabled', label: '自动健康巡检', type: 'bool', group: 'health', desc: '后台限量检查代理连通性；失败仅进入冷却，不会永久禁用。' },
  { k: 'proxy_health_check_interval_minutes', label: '巡检间隔（分钟）', type: 'number', max: 1440, min: 1, group: 'health', desc: '每轮巡检之间的间隔（默认 15）' },
  { k: 'proxy_health_check_batch_size', label: '每轮巡检数量', type: 'number', max: 500, min: 1, group: 'health', desc: '优先检查未测试和冷却到期节点（默认 50）' },
  { k: 'proxy_health_check_concurrency', label: '巡检并发数', type: 'number', max: 20, min: 1, group: 'health', desc: 'Render Free 建议 5～10（默认 5）' },
  { k: 'proxy_health_check_timeout_seconds', label: '单节点巡检超时（秒）', type: 'number', max: 60, min: 2, group: 'health', desc: '超过该时间即记录失败并进入冷却（默认 8）' },

  // 🛠 Group: core (核心控制与基础参数)
  { k: 'max_retries', label: '上游重试次数', type: 'number', group: 'core', desc: '上游请求失败时的重试次数；总尝试 = 此值 + 1' },
  { k: 'max_n', label: '最大候选数 (max_n)', type: 'number', group: 'core', desc: '限制客户端一次生成回答的条数上限，防滥用刷量 (默认 8)' },
  { k: 'max_spill_mb', label: '最大内存缓冲 (MB)', type: 'number', group: 'core', desc: '上传大文件时，超过此大小将写入磁盘，防爆内存 (默认 2048)' },
  { k: 'max_request_mb', label: '最大请求体 (MB)', type: 'number', min: 1, max: 1024, group: 'core', desc: '限制客户端请求体大小（默认 64 MB）' },
  { k: 'max_concurrent_requests', label: '全局请求并发上限', type: 'number', min: 1, max: 1000, group: 'core', desc: '限制同时执行的上游请求数量，Render Free 建议 8～16（默认 16）' },
  { k: 'request_timeout', label: '请求超时', type: 'number', max: 1800, min: 1, group: 'core', desc: '单次请求的最大连接时间 (默认 180 秒，最大 1800 秒)' },
  { k: 'aggregate_stream', label: '聚合流式', type: 'bool', group: 'core', desc: '拦截流式请求，改为一次性返回完整结果的单块流（解决部分客户端单字流式卡顿问题）' },
  { k: 'debug_mode', label: 'Debug 日志', type: 'bool', group: 'core', desc: '开启更详细的错误与负载调试日志' },

  // 🛡 Group: security (安全增强与模型策略)
  { k: 'drop_max_tokens', label: '移除 maxOutputTokens', type: 'bool', group: 'security', desc: '移除输出 token 上限，让模型自由输出' },
];

let curSettings = {};
let managedSettings = {};
async function loadSettings() {
  let d;
  try {
    d = await API.settings.get();
  } catch (e) {
    toast('设置加载失败: ' + e.message);
    return;
  }
  curSettings = d.settings || d;
  managedSettings = d.managed_fields || {};

  const gpEl = $('#globalProxy');
  if (gpEl && curSettings.proxy_url !== undefined) {
    gpEl.value = curSettings.proxy_url;
  }

  const fld = (f) => {
    const v = curSettings[f.k];
    const managedBy = managedSettings[f.k] || '';
    const managedHint = managedBy ? `<div class="desc mt-4px"><span class="pill on">环境托管</span> ${managedBy}</div>` : '';
    const disabled = managedBy ? 'disabled' : '';
    if (f.type === 'bool') return `<div class="field bool ${managedBy ? 'managed-setting' : ''}"><div class="min-w-0"><label for="set_${f.k}">${f.label}</label>${f.desc ? `<div class="desc mt-4px">${f.desc}</div>` : ''}${managedHint}</div><label class="toggle"><input type="checkbox" id="set_${f.k}" ${v ? 'checked' : ''} ${disabled}><span class="track"></span></label></div>`;
    let input;
    if (f.type === 'select') input = `<select id="set_${f.k}" ${disabled}>${f.opts.map(o => `<option ${o === v ? 'selected' : ''}>${o}</option>`).join('')}</select>`;
    else input = `<input type="${f.type}" id="set_${f.k}" value="${v ?? ''}" ${f.max !== undefined ? `max="${f.max}" data-input-action="clamp-max" data-clamp-max="${f.max}"` : ''} ${f.min !== undefined ? `min="${f.min}"` : ''} ${disabled}>`;
    return `<div class="field ${managedBy ? 'managed-setting' : ''}"><label for="set_${f.k}">${f.label}</label>${input}${f.desc ? `<div class="desc">${f.desc}</div>` : ''}${managedHint}</div>`;
  };

  // 【核心修改：定义视觉功能分组】
  const groups = {
    pool: { title: '🚀 并发与 Token 池管理', fields: [] },
    health: { title: '🩺 代理健康巡检', fields: [] },
    core: { title: '🛠 核心控制与基础参数', fields: [] },
    security: { title: '🛡 安全增强与模型策略', fields: [] }
  };

  SETTINGS_FIELDS.forEach(f => {
    if (groups[f.group]) {
      groups[f.group].fields.push(f);
    }
  });

  let sectionsHtml = '';
  for (const [key, g] of Object.entries(groups)) {
    const numFields = g.fields.filter(f => f.type !== 'bool');
    const boolFields = g.fields.filter(f => f.type === 'bool');

    let extraHtml = '';
    if (key === 'security') {
      extraHtml = `
        <div class="field" style="margin-top:12px; display:flex; justify-content:space-between; align-items:center; background:rgba(255,255,255,0.03); padding:14px; border-radius:10px; border:1px solid var(--stroke);">
          <div>
            <div style="font-weight:600; font-size:14px;">管理后台密码</div>
            <div class="desc" style="margin-top:4px;">定期修改密码有助于保障管理后台及节点会话安全</div>
          </div>
          <button type="button" class="btn ghost" style="padding:8px 16px;" data-click-action="showChangePasswordModal" ${managedSettings.admin_password ? 'disabled title="请在 Render Environment 中修改 VPROXY_ADMIN_PASSWORD"' : ''}>${managedSettings.admin_password ? '环境托管' : '修改密码'}</button>
        </div>
      `;
    }

    sectionsHtml += `
      <div class="settings-section-title">${g.title}</div>
      ${numFields.length ? `<div class="grid grid-2">${numFields.map(fld).join('')}</div>` : ''}
      ${boolFields.length ? `<div class="grid grid-2" style="margin-top:10px;">${boolFields.map(fld).join('')}</div>` : ''}
      ${extraHtml}
    `;
  }

  $('#settingsForm').innerHTML =
    sectionsHtml +
    '<button class="btn mt-14px" data-click-action="saveSettings">保存设置</button>';

  $('#settingsForm').addEventListener('input', () => window.hasUnsavedSettings = true);
  $('#settingsForm').addEventListener('change', () => window.hasUnsavedSettings = true);
  window.hasUnsavedSettings = false;

  if (!window._hasSettingsUnloadListener) {
    window.addEventListener('beforeunload', (e) => {
      if (window.hasUnsavedSettings) {
        e.preventDefault();
        e.returnValue = '';
      }
    });
    window._hasSettingsUnloadListener = true;
  }

  const stickyEl = $('#set_sticky_node_priority');
  const parallelRetryEl = $('#set_parallel_pool_retry_enabled');
  const parallelEl = $('#set_parallel_pool_enabled');
  if (stickyEl && parallelEl && parallelRetryEl) {
    const updateStickyDisabled = () => {
      const disabled = !parallelEl.checked;

      stickyEl.disabled = disabled;
      if (disabled) stickyEl.checked = false;
      const container = stickyEl.closest('.field');
      if (container) {
        container.style.opacity = disabled ? '0.5' : '1';
        container.style.pointerEvents = disabled ? 'none' : '';
        const desc = container.querySelector('.desc');
        if (desc) {
          desc.textContent = disabled ? '需先启用并发请求池' : '优先选择近期请求成功的节点，同时保留少量未测试节点用于探索。';
        }
      }

      parallelRetryEl.disabled = disabled;
      if (disabled) parallelRetryEl.checked = false;
      const retryContainer = parallelRetryEl.closest('.field');
      if (retryContainer) {
        retryContainer.style.opacity = disabled ? '0.5' : '1';
        retryContainer.style.pointerEvents = disabled ? 'none' : '';
        const retryDesc = retryContainer.querySelector('.desc');
        if (retryDesc) {
          retryDesc.textContent = disabled ? '需先启用并发请求池' : '默认开启；节点遇到429等可重试错误时允许节点内再次尝试';
        }
      }
    };
    updateStickyDisabled();
    parallelEl.addEventListener('change', updateStickyDisabled);
  }
}

async function saveSettings() {
  const out = {};
  for (const f of SETTINGS_FIELDS) {
    if (managedSettings[f.k]) continue;
    const el = $('#set_' + f.k);
    if (!el) continue;
    if (f.type === 'bool') out[f.k] = el.checked;
    else if (f.type === 'number') {
      const value = Number(el.value);
      if (!Number.isInteger(value)) {
        toast(f.label + '必须是有效整数');
        el.focus();
        return;
      }
      if (f.min !== undefined && value < f.min) {
        toast(f.label + '不能小于 ' + f.min);
        el.focus();
        return;
      }
      if (f.max !== undefined && value > f.max) {
        toast(f.label + '不能大于 ' + f.max);
        el.focus();
        return;
      }
      out[f.k] = Math.trunc(value);
    }
    else out[f.k] = el.value;
  }
  if (out.proxy_failover_max_attempts < out.parallel_pool_size) {
    toast('单请求最多尝试代理不能小于最大同时并发');
    $('#set_proxy_failover_max_attempts').focus();
    return;
  }
  if (!out['parallel_pool_enabled']) {
    out['sticky_node_priority'] = false;
    out['parallel_pool_retry_enabled'] = false;
  }
  try {
    await API.settings.put(out);
  } catch (e) {
    toast('设置保存失败: ' + e.message);
    return;
  }
  toast('设置已保存');
  window.hasUnsavedSettings = false;
  await loadSettings();
}

function showChangePasswordModal() {
  $('#oldPwInput').value = '';
  $('#newPwInput').value = '';
  $('#confirmPwInput').value = '';
  $('#changePwErr').textContent = '';
  $('#changePasswordModal').classList.remove('hidden');
}

function closeChangePasswordModal() {
  $('#changePasswordModal').classList.add('hidden');
}

async function submitChangePassword() {
  const oldPw = $('#oldPwInput').value;
  const newPw = $('#newPwInput').value;
  const confirmPw = $('#confirmPwInput').value;
  const errEl = $('#changePwErr');

  if (!oldPw) { errEl.textContent = '请输入原密码'; return; }
  if (newPw.length < 6) { errEl.textContent = '新密码长度至少需要 6 个字符'; return; }
  if (newPw !== confirmPw) { errEl.textContent = '两次输入的新密码不一致'; return; }

  errEl.textContent = '';
  try {
    await API.changePassword(oldPw, newPw);
    closeChangePasswordModal();
    toast('密码修改成功，请使用新密码重新登录！');
    setTimeout(() => { logout(); }, 1500);
  } catch (e) {
    errEl.textContent = e.message || '修改密码失败';
  }
}

registerActions({
  saveSettings: function () { saveSettings(); },
  showChangePasswordModal: function () { showChangePasswordModal(); },
  closeChangePasswordModal: function () { closeChangePasswordModal(); },
  submitChangePassword: function () { submitChangePassword(); },
  'clamp-max': function (el) {
    // 原内联 oninput 的上限钳制逻辑。
    var max = Number(el.dataset.clampMax);
    if (el.value !== '' && parseInt(el.value, 10) > max) el.value = String(max);
  },
});

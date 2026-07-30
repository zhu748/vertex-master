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

const CLAUDE_PROMPT_MAX_BYTES = 1024 * 1024;
const CLAUDE_PROMPT_MAX_RULES = 32;
const CLAUDE_PROMPT_MAX_RULE_MODELS = 16;
let latestClaudePrompt = null;
let claudeReplacementRules = [];

function escapeSettingsHTML(value) {
  return String(value ?? '').replace(/[&<>"']/g, function (character) {
    return {
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[character];
  });
}

function configuredClaudeReplacementRules() {
  if (Array.isArray(curSettings.claude_prompt_replacements)) {
    return curSettings.claude_prompt_replacements.map(rule => {
      const from = String(rule?.from ?? '');
      const to = String(rule?.to ?? '');
      return {
        from,
        to,
        action: from !== '' && to === '' ? 'delete' : 'replace',
        disabled: !!rule?.disabled,
        models: Array.isArray(rule?.models) ? rule.models.map(model => String(model)) : []
      };
    });
  }
  const legacyFrom = String(curSettings.claude_prompt_replace_from ?? '');
  if (legacyFrom !== '') {
    const legacyTo = String(curSettings.claude_prompt_replace_to ?? '');
    return [{
      from: legacyFrom,
      to: legacyTo,
      action: legacyTo === '' ? 'delete' : 'replace',
      disabled: false,
      models: []
    }];
  }
  return [];
}

function claudeReplacementRowsHTML() {
  return claudeReplacementRules.map((rule, index) => `
    <div class="claude-replacement-row" data-rule-index="${index}">
      <div class="claude-replacement-row-head">
        <span class="pill">规则 ${index + 1}</span>
        <div class="toolbar m-0 gap-8">
          <label class="claude-rule-enabled"><input type="checkbox" data-rule-field="enabled" ${rule.disabled ? '' : 'checked'}> 启用</label>
          <button type="button" class="btn ghost compact" data-click-action="moveClaudeReplacementRule" data-rule-index="${index}" data-direction="-1" title="上移" ${index === 0 ? 'disabled' : ''}>↑</button>
          <button type="button" class="btn ghost compact" data-click-action="moveClaudeReplacementRule" data-rule-index="${index}" data-direction="1" title="下移" ${index === claudeReplacementRules.length - 1 ? 'disabled' : ''}>↓</button>
          <button type="button" class="btn danger compact" data-click-action="removeClaudeReplacementRule" data-rule-index="${index}">移除规则</button>
        </div>
      </div>
      <div class="field">
        <label for="set_claude_prompt_rule_action_${index}">匹配后操作</label>
        <select id="set_claude_prompt_rule_action_${index}" data-rule-field="action">
          <option value="replace" ${rule.action === 'delete' ? '' : 'selected'}>替换为指定内容</option>
          <option value="delete" ${rule.action === 'delete' ? 'selected' : ''}>删除匹配片段</option>
        </select>
        <div class="desc">删除模式会将匹配内容替换为空字符串；保存格式与旧版本完全兼容。</div>
      </div>
      <div class="field">
        <div class="claude-rule-model-head">
          <label for="set_claude_prompt_rule_models_${index}">仅应用于这些模型（可选）</label>
          <button type="button" class="btn ghost compact" data-click-action="useLatestClaudeModelForRule" data-rule-index="${index}">使用最近模型</button>
        </div>
        <input id="set_claude_prompt_rule_models_${index}" data-rule-field="models" value="${escapeSettingsHTML((rule.models || []).join(', '))}" placeholder="例如 fake-gemini-3.6-flash；多个模型用逗号分隔">
        <div class="desc">默认留空并应用于所有 Claude 兼容模型；只有主动填写时才限制范围，同时匹配客户端模型名和解析后的实际模型名。</div>
      </div>
      <div class="grid grid-2">
        <div class="field">
          <label for="set_claude_prompt_rule_from_${index}">查找内容</label>
          <textarea id="set_claude_prompt_rule_from_${index}" data-rule-field="from" rows="6" maxlength="1048576" class="font-mono" placeholder="需要被替换的原文片段，可包含多行">${escapeSettingsHTML(rule.from)}</textarea>
        </div>
        <div class="field">
          <label for="set_claude_prompt_rule_to_${index}">替换为</label>
          <textarea id="set_claude_prompt_rule_to_${index}" data-rule-field="to" rows="6" maxlength="1048576" class="font-mono" placeholder="新的提示词片段">${escapeSettingsHTML(rule.to)}</textarea>
        </div>
      </div>
    </div>
  `).join('');
}

function claudePromptSettingsHTML() {
  const injectionEnabled = !!curSettings.claude_prompt_injection_enabled;
  const replacementEnabled = !!curSettings.claude_prompt_replacement_enabled;
  const stripPromotions = curSettings.claude_prompt_strip_claude_code_promotions !== false;
  const position = curSettings.claude_prompt_injection_position === 'prepend' ? 'prepend' : 'append';
  claudeReplacementRules = configuredClaudeReplacementRules();
  if (!claudeReplacementRules.length) {
    claudeReplacementRules.push({ from: '', to: '', action: 'replace', disabled: false, models: [] });
  }
  return `
    <div class="settings-section-title">🧩 Claude 系统提示词处理</div>
    <div class="claude-prompt-config">
      <div class="claude-prompt-rule">
        <div class="field bool">
          <div class="min-w-0">
            <label for="set_claude_prompt_strip_claude_code_promotions">移除 Claude Code 推广提示</label>
            <div class="desc mt-4px">默认开启，精确删除 Claude Code 注入的 Claude 5 模型推荐、产品入口及 Fast mode 推广三行；其他 system 内容不受影响。</div>
          </div>
          <label class="toggle"><input type="checkbox" id="set_claude_prompt_strip_claude_code_promotions" ${stripPromotions ? 'checked' : ''}><span class="track"></span></label>
        </div>
      </div>

      <div class="claude-prompt-rule">
        <div class="field bool">
          <div class="min-w-0">
            <label for="set_claude_prompt_replacement_enabled">启用片段替换</label>
            <div class="desc mt-4px">可配置多条字面量规则，按列表顺序逐条执行；不使用正则表达式。</div>
          </div>
          <label class="toggle"><input type="checkbox" id="set_claude_prompt_replacement_enabled" ${replacementEnabled ? 'checked' : ''}><span class="track"></span></label>
        </div>
        <div class="claude-rule-fields" id="claudeReplacementFields">
          <div class="claude-replacement-toolbar">
            <div class="desc">区分大小写、空白和换行；每条规则会替换所有真正的 system/developer 内容。role=user 中名字类似 &lt;system-reminder&gt; 的文本仍属于用户消息，不会被改写。前一条规则的结果可继续被后一条匹配，最多 ${CLAUDE_PROMPT_MAX_RULES} 条。</div>
            <button type="button" class="btn ghost" data-click-action="addClaudeReplacementRule">添加规则</button>
          </div>
          <div class="claude-replacement-list" id="claudeReplacementRules">${claudeReplacementRowsHTML()}</div>
          <div class="desc">所有替换完成后才执行提示词注入，所以规则不会改写新注入的内容。顶层与中途 system 会尽量保持为独立片段及原有先后顺序；只有规则跨片段匹配时才合并。</div>
        </div>
      </div>

      <div class="claude-prompt-rule">
        <div class="field bool">
          <div class="min-w-0">
            <label for="set_claude_prompt_injection_enabled">启用系统提示词注入</label>
            <div class="desc mt-4px">仅处理 Claude Messages 格式；OpenAI、Responses 和 Gemini 原生请求不受影响。</div>
          </div>
          <label class="toggle"><input type="checkbox" id="set_claude_prompt_injection_enabled" ${injectionEnabled ? 'checked' : ''}><span class="track"></span></label>
        </div>
        <div class="claude-rule-fields" id="claudeInjectionFields">
          <div class="field">
            <label for="set_claude_prompt_injection_position">注入位置</label>
            <select id="set_claude_prompt_injection_position">
              <option value="prepend" ${position === 'prepend' ? 'selected' : ''}>作为独立片段，前置到全部原 system 之前</option>
              <option value="append" ${position === 'append' ? 'selected' : ''}>作为独立片段，后置到全部原 system 之后</option>
            </select>
          </div>
          <div class="field">
            <label for="set_claude_prompt_injection_text">注入内容</label>
            <textarea id="set_claude_prompt_injection_text" rows="8" maxlength="1048576" class="font-mono" placeholder="要额外注入给上游的系统提示词">${escapeSettingsHTML(curSettings.claude_prompt_injection_text)}</textarea>
          </div>
        </div>
      </div>

      <div class="claude-prompt-rule claude-prompt-latest">
          <div class="claude-prompt-latest-head">
          <div>
            <div class="field-heading label-gold">最近一次 Claude 请求</div>
            <div class="desc">只保存在当前进程内，不写日志或配置文件；最多保留原始与最终提示词各 1 MiB。</div>
          </div>
          <div class="toolbar m-0 gap-8">
            <select id="claudePromptLatestEndpoint" title="记录类型">
              <option value="messages">生成请求</option>
              <option value="count_tokens">Token 计数</option>
            </select>
            <button type="button" class="btn ghost" data-click-action="refreshLatestClaudePrompt">刷新</button>
            <button type="button" class="btn ghost" id="useLatestClaudePromptBtn" data-click-action="useLatestClaudePromptAsFind" disabled>添加为替换规则</button>
            <button type="button" class="btn ghost" id="previewClaudePromptBtn" data-click-action="previewClaudePrompt" disabled>预览当前设置</button>
            <button type="button" class="btn ghost" id="copyLatestClaudePromptBtn" data-click-action="copyLatestClaudePrompt" disabled>复制原始</button>
            <button type="button" class="btn danger" id="clearLatestClaudePromptBtn" data-click-action="clearLatestClaudePrompt" disabled>清除</button>
          </div>
        </div>
        <div class="claude-prompt-meta" id="claudePromptLatestMeta">尚未收到 Claude Messages 请求</div>
        <div class="grid grid-2">
          <div class="field">
            <label for="claudePromptLatestOriginal">客户端原始 system 提示词</label>
            <textarea id="claudePromptLatestOriginal" rows="10" class="font-mono" readonly placeholder="收到请求后显示"></textarea>
          </div>
          <div class="field">
            <label for="claudePromptLatestEffective">处理后发给上游的 system 提示词</label>
            <textarea id="claudePromptLatestEffective" rows="10" class="font-mono" readonly placeholder="收到请求后显示"></textarea>
          </div>
        </div>
      </div>
    </div>
  `;
}

function updateClaudePromptControls() {
  const replacementEnabled = $('#set_claude_prompt_replacement_enabled')?.checked;
  const injectionEnabled = $('#set_claude_prompt_injection_enabled')?.checked;
  const replacementFields = $('#claudeReplacementFields');
  const injectionFields = $('#claudeInjectionFields');
  if (replacementFields) {
    replacementFields.classList.toggle('claude-rule-disabled', !replacementEnabled);
    replacementFields.querySelectorAll('textarea,select,input,button').forEach(el => { el.disabled = !replacementEnabled; });
    updateClaudeReplacementActionControls();
  }
  if (injectionFields) {
    injectionFields.classList.toggle('claude-rule-disabled', !injectionEnabled);
    injectionFields.querySelectorAll('textarea,select,input').forEach(el => { el.disabled = !injectionEnabled; });
  }
}

function updateClaudeReplacementActionControls() {
  const replacementEnabled = $('#set_claude_prompt_replacement_enabled')?.checked;
  document.querySelectorAll('#claudeReplacementRules .claude-replacement-row').forEach(row => {
    const action = row.querySelector('[data-rule-field="action"]')?.value || 'replace';
    const target = row.querySelector('[data-rule-field="to"]');
    if (!target) return;
    const deleting = action === 'delete';
    target.disabled = !replacementEnabled || deleting;
    target.closest('.field')?.classList.toggle('claude-rule-disabled', deleting);
  });
}

function syncClaudeReplacementRulesFromDOM() {
  const rows = document.querySelectorAll('#claudeReplacementRules .claude-replacement-row');
  claudeReplacementRules = Array.from(rows).map(row => ({
    from: row.querySelector('[data-rule-field="from"]')?.value || '',
    to: row.querySelector('[data-rule-field="to"]')?.value || '',
    action: row.querySelector('[data-rule-field="action"]')?.value === 'delete' ? 'delete' : 'replace',
    disabled: !(row.querySelector('[data-rule-field="enabled"]')?.checked ?? true),
    models: (row.querySelector('[data-rule-field="models"]')?.value || '')
      .split(/[，,\n]/).map(model => model.trim()).filter(Boolean)
  }));
}

function renderClaudeReplacementRules(focusIndex) {
  const container = $('#claudeReplacementRules');
  if (!container) return;
  if (!claudeReplacementRules.length) {
    claudeReplacementRules.push({ from: '', to: '', action: 'replace', disabled: false, models: [] });
  }
  container.innerHTML = claudeReplacementRowsHTML();
  updateClaudePromptControls();
  if (Number.isInteger(focusIndex)) {
    $('#set_claude_prompt_rule_from_' + focusIndex)?.focus();
  }
}

function addClaudeReplacementRule(rule) {
  syncClaudeReplacementRulesFromDOM();
  if (claudeReplacementRules.length >= CLAUDE_PROMPT_MAX_RULES) {
    return toast('Claude 提示词替换规则最多 32 条');
  }
  const next = Object.assign({ from: '', to: '', action: 'replace', disabled: false, models: [] }, rule || {});
  if (claudeReplacementRules.length === 1 &&
      claudeReplacementRules[0].from === '' && claudeReplacementRules[0].to === '') {
    claudeReplacementRules[0] = next;
  } else {
    claudeReplacementRules.push(next);
  }
  window.hasUnsavedSettings = true;
  renderClaudeReplacementRules(claudeReplacementRules.length - 1);
}

function removeClaudeReplacementRule(element) {
  syncClaudeReplacementRulesFromDOM();
  const index = Number(element.dataset.ruleIndex);
  if (!Number.isInteger(index) || index < 0 || index >= claudeReplacementRules.length) return;
  claudeReplacementRules.splice(index, 1);
  window.hasUnsavedSettings = true;
  renderClaudeReplacementRules(Math.min(index, claudeReplacementRules.length - 1));
}

function moveClaudeReplacementRule(element) {
  syncClaudeReplacementRulesFromDOM();
  const index = Number(element.dataset.ruleIndex);
  const target = index + Number(element.dataset.direction);
  if (!Number.isInteger(index) || target < 0 || target >= claudeReplacementRules.length) return;
  [claudeReplacementRules[index], claudeReplacementRules[target]] =
    [claudeReplacementRules[target], claudeReplacementRules[index]];
  window.hasUnsavedSettings = true;
  renderClaudeReplacementRules(target);
}

async function loadLatestClaudePrompt() {
  const original = $('#claudePromptLatestOriginal');
  const effective = $('#claudePromptLatestEffective');
  const meta = $('#claudePromptLatestMeta');
  if (!original || !effective || !meta) return;
  const buttons = [
    $('#useLatestClaudePromptBtn'),
    $('#previewClaudePromptBtn'),
    $('#copyLatestClaudePromptBtn'),
    $('#clearLatestClaudePromptBtn')
  ];
  try {
    const endpoint = $('#claudePromptLatestEndpoint')?.value || 'messages';
    const data = await API.claudePrompt.latest(endpoint);
    latestClaudePrompt = data.available ? data : null;
  } catch (e) {
    latestClaudePrompt = null;
    original.value = '';
    effective.value = '';
    buttons.forEach(button => { if (button) button.disabled = true; });
    meta.textContent = '最近提示词读取失败：' + e.message;
    return;
  }

  buttons.forEach(button => { if (button) button.disabled = !latestClaudePrompt; });
  if (!latestClaudePrompt) {
    original.value = '';
    effective.value = '';
    meta.textContent = '尚未收到 Claude Messages 请求';
    return;
  }

  const cannotReuse = !latestClaudePrompt.original_prompt || latestClaudePrompt.original_truncated;
  if ($('#useLatestClaudePromptBtn')) $('#useLatestClaudePromptBtn').disabled = cannotReuse;
  if ($('#previewClaudePromptBtn')) $('#previewClaudePromptBtn').disabled = cannotReuse;

  original.value = latestClaudePrompt.original_prompt || '';
  effective.value = latestClaudePrompt.effective_prompt || '';
  const received = latestClaudePrompt.received_at
    ? new Date(latestClaudePrompt.received_at).toLocaleString()
    : '未知时间';
  const actions = [];
  if (latestClaudePrompt.promotion_removal_count) {
    actions.push('已移除 Claude Code 推广片段×' + latestClaudePrompt.promotion_removal_count);
  }
  if (latestClaudePrompt.replacement_count) {
    let replacementSummary = '替换 ' + latestClaudePrompt.replacement_count + ' 处';
    if (latestClaudePrompt.replacement_rules) {
      replacementSummary += '（命中 ' + (latestClaudePrompt.matched_rules || 0) +
        '/' + (latestClaudePrompt.applicable_rules ?? latestClaudePrompt.replacement_rules) + ' 条适用规则）';
    }
    actions.push(replacementSummary);
    if (Array.isArray(latestClaudePrompt.rule_match_counts)) {
      const hitDetails = latestClaudePrompt.rule_match_counts
        .map((count, index) => count && (!Array.isArray(latestClaudePrompt.rule_applicable) || latestClaudePrompt.rule_applicable[index])
          ? ('#' + (index + 1) + '×' + count) : '')
        .filter(Boolean);
      if (hitDetails.length) actions.push('规则命中 ' + hitDetails.join('、'));
    }
  } else if (latestClaudePrompt.applicable_rules || latestClaudePrompt.replacement_rules) {
    actions.push('适用替换规则均未命中（0/' +
      (latestClaudePrompt.applicable_rules ?? latestClaudePrompt.replacement_rules) + '）');
  }
  if (latestClaudePrompt.injection_applied) actions.push('已注入');
  if (!actions.length) actions.push('未改写');
  const truncated = latestClaudePrompt.original_truncated || latestClaudePrompt.effective_truncated
    ? ' · 记录已截断'
    : '';
  meta.textContent =
    (latestClaudePrompt.model || '未知模型') + ' · ' +
    (latestClaudePrompt.endpoint || 'messages') + ' · ' +
    received + ' · 原始 ' + (latestClaudePrompt.original_bytes || 0) +
    'B → 最终 ' + (latestClaudePrompt.effective_bytes || 0) +
    'B · ' + actions.join('，') + truncated;
}

function useLatestClaudePromptAsFind() {
  if (!latestClaudePrompt) return toast('暂无最近 Claude 提示词');
  if (!latestClaudePrompt.original_prompt) return toast('最近请求没有可用的原始 system 提示词');
  if (latestClaudePrompt.original_truncated) return toast('最近原始提示词已截断，不能直接创建精确替换规则');
  $('#set_claude_prompt_replacement_enabled').checked = true;
  addClaudeReplacementRule({
    from: latestClaudePrompt.original_prompt || '',
    to: '',
    action: 'replace',
    disabled: false,
    models: []
  });
  toast('已添加适用于所有模型的替换规则；如需限制范围，请主动填写模型');
}

function useLatestClaudeModelForRule(element) {
  if (!latestClaudePrompt?.model) return toast('暂无可用的最近请求模型');
  const index = Number(element.dataset.ruleIndex);
  if (!Number.isInteger(index) || index < 0) return;
  const input = $('#set_claude_prompt_rule_models_' + index);
  if (!input) return;
  input.value = latestClaudePrompt.model;
  window.hasUnsavedSettings = true;
  input.focus();
  toast('已将该规则限定为最近请求模型：' + latestClaudePrompt.model);
}

function currentClaudeReplacementRulesForRequest() {
  syncClaudeReplacementRulesFromDOM();
  return claudeReplacementRules
    .filter(rule => rule.from !== '' || rule.to !== '' || (rule.models || []).length)
    .map(rule => ({
      from: rule.from,
      to: rule.action === 'delete' ? '' : rule.to,
      disabled: !!rule.disabled,
      models: rule.models || []
    }));
}

async function previewClaudePrompt() {
  if (!latestClaudePrompt) return toast('暂无最近 Claude 提示词');
  if (latestClaudePrompt.original_truncated) return toast('最近原始提示词已截断，无法精确预览');
  const effective = $('#claudePromptLatestEffective');
  const meta = $('#claudePromptLatestMeta');
  try {
    const result = await API.claudePrompt.preview({
      original_prompt: latestClaudePrompt.original_prompt || '',
      model: latestClaudePrompt.model || '',
      strip_claude_code_promotions:
        $('#set_claude_prompt_strip_claude_code_promotions').checked,
      replacement_enabled: $('#set_claude_prompt_replacement_enabled').checked,
      replacements: currentClaudeReplacementRulesForRequest(),
      injection_enabled: $('#set_claude_prompt_injection_enabled').checked,
      injection_position: $('#set_claude_prompt_injection_position').value,
      injection_text: $('#set_claude_prompt_injection_text').value
    });
    effective.value = result.effective_prompt || '';
    meta.textContent = '当前页面设置预览（尚未保存） · 默认清理 ' +
      (result.promotion_removal_count || 0) + ' 处 · 自定义替换 ' +
      (result.replacement_count || 0) + ' 处 · 命中 ' +
      (result.matched_rules || 0) + '/' + (result.applicable_rules || 0) +
      ' 条适用规则 · 最终 ' + (result.effective_bytes || 0) + 'B';
    toast('预览完成；右侧显示按当前页面设置处理后的结果');
  } catch (e) {
    toast('预览失败: ' + e.message);
  }
}

async function copyLatestClaudePrompt() {
  if (!latestClaudePrompt) return toast('暂无最近 Claude 提示词');
  try {
    await navigator.clipboard.writeText(latestClaudePrompt.original_prompt || '');
    toast('已复制最近原始提示词');
  } catch (e) {
    toast('复制失败，请手动选择文本');
  }
}

async function clearLatestClaudePrompt() {
  if (!latestClaudePrompt || !confirm('清除当前进程内记录的最近 Claude 提示词？')) return;
  try {
    await API.claudePrompt.clear($('#claudePromptLatestEndpoint')?.value || 'messages');
    await loadLatestClaudePrompt();
    toast('最近 Claude 提示词已清除');
  } catch (e) {
    toast('清除失败: ' + e.message);
  }
}

function settingsUTF8Bytes(value) {
  if (typeof TextEncoder !== 'undefined') return new TextEncoder().encode(value).length;
  return new Blob([value]).size;
}

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
  sectionsHtml += claudePromptSettingsHTML();

  $('#settingsForm').innerHTML =
    sectionsHtml +
    '<button class="btn mt-14px" data-click-action="saveSettings">保存设置</button>';

  $('#settingsForm').addEventListener('input', () => window.hasUnsavedSettings = true);
  $('#settingsForm').addEventListener('change', event => {
    window.hasUnsavedSettings = true;
    if (event.target?.matches('[data-rule-field="action"]')) {
      updateClaudeReplacementActionControls();
    }
  });
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

  $('#set_claude_prompt_replacement_enabled').addEventListener('change', updateClaudePromptControls);
  $('#set_claude_prompt_injection_enabled').addEventListener('change', updateClaudePromptControls);
  $('#claudePromptLatestEndpoint')?.addEventListener('change', loadLatestClaudePrompt);
  updateClaudePromptControls();
  await loadLatestClaudePrompt();

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
  const replacementEnabled = $('#set_claude_prompt_replacement_enabled').checked;
  const injectionEnabled = $('#set_claude_prompt_injection_enabled').checked;
  const injectionText = $('#set_claude_prompt_injection_text').value;
  syncClaudeReplacementRulesFromDOM();
  const replacements = [];
  const seenReplacementSources = new Set();
  let replacementBytes = 0;
  for (let index = 0; index < claudeReplacementRules.length; index++) {
    const rule = claudeReplacementRules[index];
    const replacementTarget = rule.action === 'delete' ? '' : rule.to;
    if (rule.from === '' && replacementTarget === '') continue;
    if (rule.from === '') {
      toast('替换规则 ' + (index + 1) + ' 的查找内容不能为空');
      $('#set_claude_prompt_rule_from_' + index)?.focus();
      return;
    }
    if ((rule.models || []).length > CLAUDE_PROMPT_MAX_RULE_MODELS) {
      toast('替换规则 ' + (index + 1) + ' 最多指定 ' + CLAUDE_PROMPT_MAX_RULE_MODELS + ' 个模型');
      $('#set_claude_prompt_rule_models_' + index)?.focus();
      return;
    }
    const normalizedModels = new Set();
    for (const model of (rule.models || [])) {
      const normalized = model.toLowerCase();
      if (normalizedModels.has(normalized)) {
        toast('替换规则 ' + (index + 1) + ' 包含重复模型');
        $('#set_claude_prompt_rule_models_' + index)?.focus();
        return;
      }
      normalizedModels.add(normalized);
      replacementBytes += settingsUTF8Bytes(model);
    }
    if (seenReplacementSources.has(rule.from)) {
      toast('替换规则 ' + (index + 1) + ' 的查找内容重复');
      $('#set_claude_prompt_rule_from_' + index)?.focus();
      return;
    }
    seenReplacementSources.add(rule.from);
    replacementBytes += settingsUTF8Bytes(rule.from) + settingsUTF8Bytes(replacementTarget);
    replacements.push({
      from: rule.from,
      to: replacementTarget,
      disabled: !!rule.disabled,
      models: rule.models || []
    });
  }
  if (replacementEnabled && !replacements.some(rule => !rule.disabled)) {
    toast('启用 Claude 提示词替换时，至少需要一条已启用规则');
    $('#set_claude_prompt_rule_from_0')?.focus();
    return;
  }
  if (replacementBytes > CLAUDE_PROMPT_MAX_BYTES) {
    toast('Claude 提示词替换规则文本总计不能超过 1 MiB');
    return;
  }
  if (injectionEnabled && injectionText.trim() === '') {
    toast('启用 Claude 系统提示词注入时，注入内容不能为空');
    $('#set_claude_prompt_injection_text').focus();
    return;
  }
  if (settingsUTF8Bytes(injectionText) > CLAUDE_PROMPT_MAX_BYTES) {
    toast('注入内容不能超过 1 MiB');
    $('#set_claude_prompt_injection_text').focus();
    return;
  }
  out.claude_prompt_replacement_enabled = replacementEnabled;
  out.claude_prompt_replacements = replacements;
  out.claude_prompt_strip_claude_code_promotions =
    $('#set_claude_prompt_strip_claude_code_promotions').checked;
  out.claude_prompt_injection_enabled = injectionEnabled;
  out.claude_prompt_injection_position = $('#set_claude_prompt_injection_position').value;
  out.claude_prompt_injection_text = injectionText;
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
  addClaudeReplacementRule: function () { addClaudeReplacementRule(); },
  removeClaudeReplacementRule: function (element) { removeClaudeReplacementRule(element); },
  moveClaudeReplacementRule: function (element) { moveClaudeReplacementRule(element); },
  refreshLatestClaudePrompt: function () { loadLatestClaudePrompt(); },
  useLatestClaudePromptAsFind: function () { useLatestClaudePromptAsFind(); },
  useLatestClaudeModelForRule: function (element) { useLatestClaudeModelForRule(element); },
  previewClaudePrompt: function () { previewClaudePrompt(); },
  copyLatestClaudePrompt: function () { copyLatestClaudePrompt(); },
  clearLatestClaudePrompt: function () { clearLatestClaudePrompt(); },
  showChangePasswordModal: function () { showChangePasswordModal(); },
  closeChangePasswordModal: function () { closeChangePasswordModal(); },
  submitChangePassword: function () { submitChangePassword(); },
  'clamp-max': function (el) {
    // 原内联 oninput 的上限钳制逻辑。
    var max = Number(el.dataset.clampMax);
    if (el.value !== '' && parseInt(el.value, 10) > max) el.value = String(max);
  },
});

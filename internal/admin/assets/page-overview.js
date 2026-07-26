async function loadOverview() {
  const card = (label, value, cls, sub) => `<div class="card glass hoverable stat"><div class="label">${label}</div><div class="value ${cls||''}">${value}</div>${sub?`<div class="sub">${sub}</div>`:''}</div>`;
  const [keysD, modelsD, nodesD] = await Promise.all([
    API.keys.list().catch(() => ({keys:[]})),
    API.models.get().catch(() => ({models:[]})),
    API.nodes.list({ page: 1, page_size: 1 }).catch(() => ({nodes:[]})),
  ]);
  const keys = (keysD.keys || []).length;
  const models = (modelsD.models || []).length;
  const nodes = nodesD.overall_total || (nodesD.nodes || []).length;
  const spAvail = nodesD.sticky_pool_available || 0;
  const poolStats = nodesD.pool_stats || {};
  const stickySub = `近期成功 ${spAvail} / 冷却 ${poolStats.cooling || 0}`;
  $('#ovCards').innerHTML =
    card('服务状态', '运行中', 'green', 'OpenAI / Gemini 兼容') +
    card('API 密钥', keys, 'gold') +
    card('模型', models, 'blue') +
    card('代理节点', nodes, '') +
    card('健康代理', poolStats.healthy || 0, 'gold', stickySub);
}

registerActions({ loadOverview: function () { loadOverview(); } });

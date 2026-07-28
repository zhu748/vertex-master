async function loadOverview() {
  const card = (label, value, cls, sub) => `<div class="card glass hoverable stat"><div class="label">${label}</div><div class="value ${cls||''}">${value}</div>${sub?`<div class="sub">${sub}</div>`:''}</div>`;
  const [keysD, modelsD, nodesD, statsD] = await Promise.all([
    API.keys.list().catch(() => ({keys:[]})),
    API.models.get().catch(() => ({models:[]})),
    API.nodes.list({ page: 1, page_size: 1 }).catch(() => ({nodes:[]})),
    API.stats.get().catch(() => ({token_count:{}})),
  ]);
  const keys = (keysD.keys || []).length;
  const models = (modelsD.models || []).length;
  const nodes = nodesD.overall_total || (nodesD.nodes || []).length;
  const spAvail = nodesD.sticky_pool_available || 0;
  const poolStats = nodesD.pool_stats || {};
  const stickySub = `近期成功 ${spAvail} / 冷却 ${poolStats.cooling || 0}`;
  const tokenStats = statsD.token_count || {};
  const tokenSub = `命中 ${tokenStats.cache_hits || 0} · 共享 ${tokenStats.shared_waits || 0} · 上游 ${tokenStats.upstream_queries || 0}`;
  $('#ovCards').innerHTML =
    card('服务状态', '运行中', 'green', 'OpenAI / Gemini 兼容') +
    card('API 密钥', keys, 'gold') +
    card('模型', models, 'blue') +
    card('代理节点', nodes, '') +
    card('健康代理', poolStats.healthy || 0, 'gold', stickySub) +
    card('Token 缓存', tokenStats.cache_entries || 0, 'blue', tokenSub);
}

registerActions({ loadOverview: function () { loadOverview(); } });

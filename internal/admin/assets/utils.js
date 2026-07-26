function $(s) { return document.querySelector(s); }

// ---- 事件委派 ----
//
// 内容安全策略禁止内联事件处理器（script-src 不含 'unsafe-inline'），
// 因此所有交互统一通过 data-action 属性声明，在此按 action 名派发。
// 委派挂在 document 上，动态渲染出来的元素无需重新绑定即可生效。
const ACTIONS = {};

// registerActions 注册一批 action 处理函数：{ 'action-name': fn }。
// 处理函数签名为 (element, event)，element 是带 data-action 的那个元素。
function registerActions(map) {
  Object.keys(map).forEach(function (name) { ACTIONS[name] = map[name]; });
}

// dispatchAction 从事件目标向外逐层查找带 data-<type>-action 的元素并依次派发，
// 模拟原生冒泡：内层动作调用 stopPropagation() 即可阻止外层动作触发
// （例如删除渐变色标时，不应同时选中该色标）。
//
// 注意：委派统一挂在 document 上，事件到达这里时冒泡已经结束，
// 因此必须自己遍历祖先链，而不能依赖浏览器的冒泡机制。
function dispatchAction(eventType, e) {
  const attr = 'data-' + eventType + '-action';
  const key = eventType + 'Action';
  let el = e.target.closest('[' + attr + ']');
  let stopped = false;
  const stop = e.stopPropagation.bind(e);
  e.stopPropagation = function () { stopped = true; stop(); };
  while (el) {
    const fn = ACTIONS[el.dataset[key]];
    if (typeof fn === 'function') {
      fn(el, e);
      if (stopped) break;
    }
    el = el.parentElement && el.parentElement.closest('[' + attr + ']');
  }
  e.stopPropagation = stop;
}

document.addEventListener('click', function (e) { dispatchAction('click', e); });
document.addEventListener('change', function (e) { dispatchAction('change', e); });
document.addEventListener('input', function (e) { dispatchAction('input', e); });
function toast(msg) { const t = $('#toast'); t.textContent = msg; t.classList.add('show'); setTimeout(() => t.classList.remove('show'), 1900); }
function esc(s) { return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }
const _tmap = { vless: 'VLESS', vmess: 'VMess', trojan: 'Trojan', ss: 'Shadowsocks', shadowsocks: 'Shadowsocks', hysteria2: 'Hysteria2', hy2: 'Hysteria2', tuic: 'TUIC' };
const DEFAULT_BG = "url('background.jpg')";

function showConfirm(msg, onOk, onCancel, onSave) {
  const m = $('#confirmModal');
  if (!m) return;
  $('#confirmModalText').innerHTML = msg;
  m.classList.remove('hidden');
  
  const okBtn = $('#confirmOkBtn');
  const cancelBtn = $('#confirmCancelBtn');
  const saveBtn = $('#confirmSaveBtn');
  
  if (onSave) {
    saveBtn.classList.remove('hidden');
  } else if (saveBtn) {
    saveBtn.classList.add('hidden');
  }

  const cleanup = () => {
    m.classList.add('hidden');
    okBtn.onclick = null;
    cancelBtn.onclick = null;
    if (saveBtn) saveBtn.onclick = null;
  };
  
  okBtn.onclick = () => { cleanup(); if(onOk) onOk(); };
  cancelBtn.onclick = () => { cleanup(); if(onCancel) onCancel(); };
  if (saveBtn) {
    saveBtn.onclick = () => { cleanup(); if(onSave) onSave(); };
  }
}

'use strict';

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const icon = (name, cls = 'i') => `<svg class="${cls}" aria-hidden="true"><use href="#i-${name}"/></svg>`;

async function api(method, url, body) {
  const opt = { method, headers: { 'X-BatchGen': '1' } };
  if (body instanceof FormData) {
    opt.body = body;
  } else if (body !== undefined) {
    opt.body = JSON.stringify(body);
    opt.headers['Content-Type'] = 'application/json';
  }
  let res;
  try {
    res = await fetch(url, opt);
  } catch {
    // 浏览器层面的失败：本地 BatchGen 程序没在运行（不是生成接口的错误）
    setOffline(true);
    throw new Error('连接不到 BatchGen 程序，它可能已经关闭。请重新打开 BatchGen.exe，再刷新本页。');
  }
  setOffline(false);
  // 后台换成了新版本（例如重新打开了新编译的程序）：刷新页面加载新界面
  const ver = res.headers.get('X-BatchGen-Version');
  if (ver) {
    if (!serverVersion) serverVersion = ver;
    else if (ver !== serverVersion) location.reload();
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
  return data;
}

let serverVersion = '';
let offline = false;
function setOffline(v) {
  if (v === offline) return;
  offline = v;
  $('#offlineBanner').hidden = !v;
  $('#sideStatus').classList.toggle('offline-state', v);
  if (v) {
    $('#ssText').textContent = '程序未运行';
  } else if (!presets.length) {
    location.reload(); // 启动时就没连上，恢复后重新加载一次拿配置
  } else {
    updateSideStatus();
  }
}

// 弹层先播放退出动画再隐藏。
function leave(el, done) {
  el.classList.add('leaving');
  setTimeout(() => {
    el.hidden = true;
    el.classList.remove('leaving');
    if (done) done();
  }, 180);
}

// 分段控件和导航的选中项是一块滑动的底，这里算出它的位置。
function syncThumb(seg) {
  const on = $('input:checked', seg);
  const label = on && on.closest('label');
  if (!label || !seg.offsetWidth) return;
  const thumb = $('.seg-thumb', seg);
  thumb.style.setProperty('--x', label.offsetLeft + 'px');
  thumb.style.setProperty('--w', label.offsetWidth + 'px');
  requestAnimationFrame(() => seg.classList.add('thumb-on'));
}

function syncNav() {
  const nav = $('.nav');
  const on = $('.nav-item.active', nav);
  const thumb = $('.nav-thumb', nav);
  if (!on || !thumb) return;
  thumb.style.setProperty('--y', on.offsetTop + 'px');
  thumb.style.setProperty('--h', on.offsetHeight + 'px');
  requestAnimationFrame(() => nav.classList.add('thumb-on'));
}

function syncThumbs() {
  $$('.seg').forEach(syncThumb);
  syncNav();
}

function setupThumbs() {
  $('.nav').insertAdjacentHTML('afterbegin', '<i class="nav-thumb" aria-hidden="true"></i>');
  for (const seg of $$('.seg')) {
    seg.insertAdjacentHTML('afterbegin', '<i class="seg-thumb" aria-hidden="true"></i>');
    seg.classList.add('has-thumb');
    new ResizeObserver(() => syncThumb(seg)).observe(seg);
  }
  document.addEventListener('change', e => {
    const seg = e.target.closest && e.target.closest('.seg');
    if (seg) syncThumb(seg);
  });
  // 字体加载完宽度会变
  document.fonts.ready.then(syncThumbs);
  syncThumbs();
}

function setMsg(el, text, cls = '') {
  el.textContent = text;
  el.className = 'msg ' + cls;
}

function fmtBytes(n) {
  if (n < 1024) return `${n} B`;
  return n < 1024 * 1024 ? `${(n / 1024).toFixed(0)} KB` : `${(n / 1024 / 1024).toFixed(1)} MB`;
}

/* ---------- 下拉框 ---------- */

// 用自绘的下拉面板替换浏览器原生下拉。原生 <select> 隐藏保留，读写 .value 和 change 事件照旧，
// 代码里直接给 .value / .selectedIndex 赋值时，显示也会跟着更新。
const nativeValue = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value');
const nativeIndex = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'selectedIndex');
const CARET = '<svg class="dd-caret" viewBox="0 0 16 16" aria-hidden="true"><path d="M4 6l4 4 4-4" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>';
const CHECK = '<svg class="dd-check" viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 8.5l3 3 6-7" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>';
let ddSeq = 0;

function enhanceSelect(sel) {
  if (sel.dataset.enhanced) return;
  sel.dataset.enhanced = '1';
  const listId = 'dd-list-' + (++ddSeq);
  const wrap = document.createElement('div');
  wrap.className = 'dd';
  sel.before(wrap);
  wrap.append(sel);
  sel.hidden = true;
  wrap.insertAdjacentHTML('beforeend', `
    <button type="button" class="dd-btn" aria-haspopup="listbox" aria-expanded="false" aria-controls="${listId}">
      <span class="dd-label"></span>${CARET}
    </button>
    <div class="dd-list" id="${listId}" role="listbox" tabindex="-1" hidden></div>`);
  const btn = $('.dd-btn', wrap);
  const list = $('.dd-list', wrap);
  let active = -1;

  const refresh = () => {
    const o = sel.options[nativeIndex.get.call(sel)];
    $('.dd-label', wrap).textContent = o ? o.text : '';
    btn.title = o ? o.text : '';
  };
  Object.defineProperty(sel, 'value', {
    configurable: true,
    get() { return nativeValue.get.call(this); },
    set(v) { nativeValue.set.call(this, v); refresh(); },
  });
  Object.defineProperty(sel, 'selectedIndex', {
    configurable: true,
    get() { return nativeIndex.get.call(this); },
    set(v) { nativeIndex.set.call(this, v); refresh(); },
  });

  const render = () => {
    const cur = sel.selectedIndex;
    list.innerHTML = [...sel.options].map((o, i) => `
      ${o.dataset.sep ? '<div class="dd-sep" role="separator"></div>' : ''}
      <div class="dd-opt${i === cur ? ' selected' : ''}" role="option" data-i="${i}" aria-selected="${i === cur}">
        <span class="dd-text"><span class="dd-name">${esc(o.text)}</span>${o.dataset.desc ? `<span class="dd-desc">${esc(o.dataset.desc)}</span>` : ''}</span>
        ${CHECK}
      </div>`).join('');
  };
  const highlight = (i, scroll) => {
    active = Math.max(0, Math.min(sel.options.length - 1, i));
    $$('.dd-opt', list).forEach(el => el.classList.toggle('active', +el.dataset.i === active));
    const el = $(`.dd-opt[data-i="${active}"]`, list);
    if (scroll && el) el.scrollIntoView({ block: 'nearest' });
  };
  const open = () => {
    closeAllDropdowns(wrap);
    render();
    list.hidden = false;
    wrap.classList.add('open');
    btn.setAttribute('aria-expanded', 'true');
    // 下方空间不够时向上展开
    const r = btn.getBoundingClientRect();
    const below = innerHeight - r.bottom;
    wrap.classList.toggle('up', below < Math.min(list.scrollHeight + 12, 360) && r.top > below);
    highlight(sel.selectedIndex, true);
  };
  const close = () => {
    if (list.hidden) return;
    list.hidden = true;
    wrap.classList.remove('open', 'up');
    btn.setAttribute('aria-expanded', 'false');
  };
  const choose = i => {
    if (i !== sel.selectedIndex) {
      sel.selectedIndex = i;
      sel.dispatchEvent(new Event('change', { bubbles: true }));
    }
    close();
    btn.focus();
  };
  wrap.ddClose = close;

  btn.addEventListener('click', e => {
    e.preventDefault(); // 在 <label> 里时阻止点击转发给隐藏的原生 select
    list.hidden ? open() : close();
  });
  list.addEventListener('mousedown', e => e.preventDefault()); // 保持焦点在按钮上
  list.addEventListener('click', e => {
    const el = e.target.closest('.dd-opt');
    if (el) choose(+el.dataset.i);
  });
  list.addEventListener('mousemove', e => {
    const el = e.target.closest('.dd-opt');
    if (el && +el.dataset.i !== active) highlight(+el.dataset.i, false);
  });
  btn.addEventListener('keydown', e => {
    const isOpen = !list.hidden;
    switch (e.key) {
      case 'ArrowDown':
      case 'ArrowUp':
        e.preventDefault();
        if (!isOpen) open();
        else highlight(active + (e.key === 'ArrowDown' ? 1 : -1), true);
        break;
      case 'Home':
      case 'End':
        if (isOpen) { e.preventDefault(); highlight(e.key === 'Home' ? 0 : sel.options.length - 1, true); }
        break;
      case 'Enter':
      case ' ':
        e.preventDefault();
        isOpen ? choose(active) : open();
        break;
      case 'Escape':
        if (isOpen) { e.preventDefault(); close(); }
        break;
      case 'Tab':
        close();
        break;
    }
  });
  refresh();
}

/* ---------- 可输入的下拉框（模型、尺寸） ---------- */

// 输入框仍可自由输入；点右侧箭头列出全部选项，输入时按内容筛选。
let modelOptions = [];
const SIZE_CUSTOM = '自定义';
const SIZE_GROUPS = [
  ['比例', [['16:9', '横屏'], ['9:16', '竖屏'], ['1:1', '方形'], ['4:3', '横版'], ['3:4', '竖版'],
    ['3:2', '横版'], ['2:3', '竖版'], ['21:9', '宽银幕']]],
  ['常用尺寸', [['1024x1024', '1:1 方形'], ['1536x1024', '3:2 横版'], ['1024x1536', '2:3 竖版'],
    ['1280x720', '16:9 横屏 · 720p'], ['720x1280', '9:16 竖屏 · 720p'],
    ['1920x1080', '16:9 横屏 · 1080p'], ['1080x1920', '9:16 竖屏 · 1080p']]],
  ['2K', [['2K', '只指定分辨率'], ['2560x1440', '16:9 横屏'], ['1440x2560', '9:16 竖屏'], ['2048x2048', '1:1 方形']]],
  ['4K', [['4K', '只指定分辨率'], ['3840x2160', '16:9 横屏'], ['2160x3840', '9:16 竖屏'], ['2880x2880', '1:1 方形']]],
  ['其它', [['720p', '只指定分辨率'], ['1080p', '只指定分辨率'], [SIZE_CUSTOM, '自己输入，比如 1600x900']]],
];
// 按名字粗略判断模型用途，把更可能适用的排在前面（只影响排序，不限制选择）。
const MODEL_HINTS = {
  image: /image|dall|flux|seedream|imagen|banana|midjourney|^mj|kolors|recraft|ideogram|cogview|stable|sdxl|jimeng|t2i/i,
  video: /video|sora|veo|kling|seedance|hailuo|vidu|runway|luma|pika|pixverse|wan2|wanx|t2v|i2v|minimax-h|cogvideo/i,
};
const KIND_LABEL = { image: '图片', video: '视频' };

function modelKindFor(input) {
  if (input.dataset.kind) return input.dataset.kind;
  return cf.elements.kind.value === 'video' ? 'video' : 'image'; // 新建页的模型框跟随所选类型
}

const COMBO_SOURCES = {
  model: {
    empty: '还没有模型列表：到「设置」里点「测试连接」获取，或者直接输入模型名',
    groups: input => {
      const kind = modelKindFor(input);
      const fit = modelOptions.filter(m => MODEL_HINTS[kind].test(m));
      const rest = modelOptions.filter(m => !MODEL_HINTS[kind].test(m));
      if (!fit.length) return [['', rest]];
      return [[`像是${KIND_LABEL[kind]}模型`, fit], ['其它模型', rest]];
    },
  },
  size: { empty: '', groups: () => SIZE_GROUPS },
};

function highlightMatch(text, q) {
  const i = q ? text.toLowerCase().indexOf(q) : -1;
  if (i < 0) return esc(text);
  return esc(text.slice(0, i)) + '<mark>' + esc(text.slice(i, i + q.length)) + '</mark>' + esc(text.slice(i + q.length));
}

function enhanceCombo(input) {
  if (input.dataset.enhanced) return;
  input.dataset.enhanced = '1';
  const src = COMBO_SOURCES[input.dataset.combo];
  const listId = 'dd-list-' + (++ddSeq);
  const wrap = document.createElement('div');
  wrap.className = 'dd combo';
  input.before(wrap);
  wrap.append(input);
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-expanded', 'false');
  input.setAttribute('aria-controls', listId);
  input.autocomplete = 'off';
  wrap.insertAdjacentHTML('beforeend', `
    <button type="button" class="combo-toggle" tabindex="-1" aria-label="展开选项">${CARET}</button>
    <div class="dd-list" id="${listId}" role="listbox" hidden></div>`);
  const list = $('.dd-list', wrap);
  const toggle = $('.combo-toggle', wrap);
  let items = [];
  let active = -1;

  const render = filter => {
    const q = filter.trim().toLowerCase();
    const norm = x => (Array.isArray(x) ? x : [x, '']);
    const groups = src.groups(input).map(([title, list]) => [title, list.map(norm)]);
    const total = groups.reduce((n, [, l]) => n + l.length, 0);
    const match = ([v, d]) => !q || v.toLowerCase().includes(q) || d.toLowerCase().includes(q);
    items = [];
    let html = '';
    for (const [title, list] of groups) {
      const hits = list.filter(match);
      if (!hits.length) continue;
      if (title) html += `<div class="dd-group">${esc(title)}<span class="mono">${hits.length}</span></div>`;
      const cur = input.value.trim();
      for (const [v, d] of hits) {
        const i = items.push([v, d]) - 1;
        html += `
          <div class="dd-opt${v === cur ? ' selected' : ''}" role="option" data-i="${i}" aria-selected="${v === cur}">
            <span class="dd-text"><span class="dd-name mono">${highlightMatch(v, q)}</span>${d ? `<span class="dd-desc">${esc(d)}</span>` : ''}</span>
            ${CHECK}
          </div>`;
      }
    }
    if (!items.length) {
      list.innerHTML = `<div class="dd-empty">${esc(total ? '没有匹配的选项，会直接使用你输入的内容' : src.empty)}</div>`;
      return;
    }
    const count = q && total > 8 ? `<div class="dd-count">匹配 ${items.length} / ${total}</div>` : '';
    list.innerHTML = count + html;
  };
  const highlight = (i, scroll) => {
    active = items.length ? Math.max(0, Math.min(items.length - 1, i)) : -1;
    $$('.dd-opt', list).forEach(el => el.classList.toggle('active', +el.dataset.i === active));
    const el = $(`.dd-opt[data-i="${active}"]`, list);
    if (scroll && el) el.scrollIntoView({ block: 'nearest' });
  };
  const open = filter => {
    closeAllDropdowns(wrap);
    render(filter);
    if (!src.empty && !items.length && !filter) return;
    list.hidden = false;
    wrap.classList.add('open');
    input.setAttribute('aria-expanded', 'true');
    const r = input.getBoundingClientRect();
    const below = innerHeight - r.bottom;
    wrap.classList.toggle('up', below < Math.min(list.scrollHeight + 12, 360) && r.top > below);
    const sel = items.findIndex(([v]) => v === input.value.trim());
    highlight(sel >= 0 ? sel : (filter ? 0 : -1), true);
  };
  const close = () => {
    if (list.hidden) return;
    list.hidden = true;
    wrap.classList.remove('open', 'up');
    input.setAttribute('aria-expanded', 'false');
  };
  const choose = i => {
    const custom = items[i][0] === SIZE_CUSTOM;
    input.value = custom ? '' : items[i][0];
    if (custom) input.placeholder = '输入尺寸或比例，比如 1600x900';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.dispatchEvent(new Event('change', { bubbles: true }));
    close();
    input.focus();
  };
  wrap.ddClose = close;

  toggle.addEventListener('mousedown', e => e.preventDefault());
  toggle.addEventListener('click', () => {
    if (list.hidden) { input.focus(); open(''); } else close();
  });
  input.addEventListener('click', () => { if (list.hidden) open(''); });
  input.addEventListener('input', e => { if (e.isTrusted) open(input.value); });
  input.addEventListener('keydown', e => {
    const isOpen = !list.hidden;
    switch (e.key) {
      case 'ArrowDown':
      case 'ArrowUp':
        e.preventDefault();
        if (!isOpen) open('');
        else highlight(active + (e.key === 'ArrowDown' ? 1 : -1), true);
        break;
      case 'Enter':
        if (isOpen && active >= 0) { e.preventDefault(); choose(active); }
        break;
      case 'Escape':
        if (isOpen) { e.preventDefault(); close(); }
        break;
      case 'Tab':
        close();
        break;
    }
  });
  list.addEventListener('mousedown', e => e.preventDefault());
  list.addEventListener('click', e => {
    const el = e.target.closest('.dd-opt');
    if (el) choose(+el.dataset.i);
  });
  list.addEventListener('mousemove', e => {
    const el = e.target.closest('.dd-opt');
    if (el && +el.dataset.i !== active) highlight(+el.dataset.i, false);
  });
}

// 接口已配置时，后台拉一次模型列表，供模型输入框下拉选择。
async function loadModels() {
  if (!config || !config.base_url || !(config.api_keys || []).length) return;
  try {
    const r = await api('POST', '/api/test', { base_url: config.base_url, api_keys: config.api_keys, models_path: config.models_path });
    if (r.ok) modelOptions = r.models;
  } catch { /* 拉不到就算了，用户仍可手动输入 */ }
}

function closeAllDropdowns(except) {
  $$('.dd.open').forEach(w => { if (w !== except) w.ddClose(); });
}
document.addEventListener('mousedown', e => {
  if (!e.target.closest('.dd')) closeAllDropdowns(null);
});
window.addEventListener('resize', () => closeAllDropdowns(null));

/* ---------- 导航 ---------- */

let currentTab = 'create';
function showTab(name) {
  if (name === currentTab) return;
  currentTab = name;
  $$('.nav-item').forEach(b => b.classList.toggle('active', b.dataset.tab === name));
  $$('.view').forEach(t => t.classList.toggle('active', t.id === 'tab-' + name));
  $('.main').scrollTo({ top: 0 });
  syncThumbs();
  if (name === 'tasks') refreshBatches();
}
$$('.nav-item').forEach(b => { b.onclick = () => showTab(b.dataset.tab); });
document.addEventListener('click', e => {
  const g = e.target.closest('[data-goto]');
  if (g) { e.preventDefault(); showTab(g.dataset.goto); }
});

/* ---------- 设置 ---------- */

let config = null;
let presets = [];
const sf = $('#settingsForm');

// 接口详情编辑器的字段：[路径, 标签, 类型, 选项]
const AUTH_OPTIONS = [
  ['bearer', 'Authorization: Bearer <Key>'],
  ['x-goog-api-key', 'x-goog-api-key: <Key>（Gemini / Veo）'],
  ['x-api-key', 'x-api-key: <Key>'],
  ['kling_jwt', '可灵 JWT（Key 填 AccessKey:SecretKey；普通 Key 按 Bearer 发送）'],
  ['none', '不加鉴权'],
];
const BODY_TYPES = [['json', 'JSON'], ['form', '表单 multipart（可上传参考图文件）']];
const SPEC_GROUPS = [
  ['连接（可选，留空使用通用接口地址和 Key）', [
    ['base_url', '专用接口地址', 'text'],
    ['api_keys', '专用 API Key（一行一个）', 'lines'],
    ['auth', '鉴权方式', 'select', AUTH_OPTIONS],
    ['headers', '额外请求头（一行一个，如 X-DashScope-Async: enable）', 'headers'],
  ]],
  ['提交任务', [
    ['image_delivery', '本地参考图发送方式', 'select', [['inline', '直接发送图片数据（base64）'], ['upload', '先上传到图床，再发送网址（见「参考图上传」）']]],
    ['submit.method', '请求方法', 'select', [['POST', 'POST'], ['PUT', 'PUT'], ['GET', 'GET']]],
    ['submit.path', '路径（可用 {model}）', 'text'],
    ['submit.body_type', '请求体格式', 'select', BODY_TYPES],
    ['submit.file_field', '参考图文件字段名（仅表单格式）', 'text'],
    ['submit.body', '请求体模板', 'code'],
  ]],
  ['带参考图时改用的请求（可选，路径留空则沿用上面的请求）', [
    ['submit_ref.method', '请求方法', 'select', [['POST', 'POST'], ['PUT', 'PUT']]],
    ['submit_ref.path', '路径', 'text'],
    ['submit_ref.body_type', '请求体格式', 'select', BODY_TYPES],
    ['submit_ref.file_field', '参考图文件字段名（仅表单格式）', 'text'],
    ['submit_ref.body', '请求体模板', 'code'],
  ]],
  ['返回结果', [
    ['task_id', '任务 ID 字段（留空 = 同步接口，提交后直接返回结果）', 'text'],
    ['result', '结果字段（文件网址或 base64，多个候选用 | 分隔；留空自动识别）', 'text'],
  ]],
  ['查询任务（异步接口）', [
    ['poll.path', '查询路径（{id} 为任务 ID）', 'text'],
    ['poll.path_ref', '带参考图提交时的查询路径（可选）', 'text'],
    ['poll.status', '状态字段', 'text'],
    ['poll.success', '成功状态值（逗号分隔，留空用常见值）', 'text'],
    ['poll.fail', '失败状态值（逗号分隔，留空用常见值）', 'text'],
    ['poll.progress', '进度字段（可选）', 'text'],
    ['poll.error', '错误信息字段（可选）', 'text'],
    ['poll.file_id', '文件 ID 字段（可选，供下载路径的 {file_id} 使用）', 'text'],
    ['poll.content', '下载路径（可选，结果里没有文件地址时使用，可用 {id} {file_id}）', 'text'],
  ]],
];
const VAR_HELP = `
  <p><b>请求体变量</b>：<code>{{prompt}}</code> <code>{{model}}</code> <code>{{size}}</code>
  <code>{{size_star}}</code>（1280*720） <code>{{width}}</code> <code>{{height}}</code>
  <code>{{aspect_ratio}}</code>（16:9） <code>{{resolution}}</code>（720p） <code>{{resolution_upper}}</code>（720P）
  <code>{{duration}}</code>（数字） <code>{{duration_str}}</code>（字符串）
  <code>{{image}}</code>（参考图网址或 data URL） <code>{{image_b64}}</code>（纯 base64） <code>{{image_mime}}</code>
  <code>{{image_url_or_b64}}</code>（有网址用网址，否则纯 base64）
  <code>{{images}}</code>（所有参考图组成的数组）</p>
  <p><b>规则</b>：变量为空时，所在的键或数组元素会被去掉；<code>[[ ... ]]</code> 是可选片段，里面变量为空就整段删掉；
  对象里写 <code>"$if": "image"</code> 表示只有带参考图时才保留这个对象；
  数组里的对象写 <code>"$each": "refs"</code> 表示每张参考图生成一个元素，里面用 <code>{{item}}</code> <code>{{item_b64}}</code> <code>{{item_mime}}</code>。
  <b>字段路径</b>写法如 <code>data.task_result.videos.*.url</code>：<code>*</code> 表示所有元素，<code>0</code> 表示第一个。</p>`;

function getPath(obj, path) {
  return path.split('.').reduce((o, k) => (o == null ? undefined : o[k]), obj);
}
function setPath(obj, path, val) {
  const keys = path.split('.');
  let o = obj;
  keys.slice(0, -1).forEach(k => {
    if (typeof o[k] !== 'object' || o[k] === null) o[k] = {};
    o = o[k];
  });
  o[keys[keys.length - 1]] = val;
}

function fieldHTML([path, label, type, extra]) {
  const name = `spec.${path}`;
  let input;
  if (type === 'select') {
    input = `<select data-spec="${name}">${extra.map(([v, l]) => `<option value="${v}">${esc(l)}</option>`).join('')}</select>`;
  } else if (type === 'code') {
    input = `<textarea data-spec="${name}" rows="10" class="mono" spellcheck="false"></textarea>`;
  } else if (type === 'lines' || type === 'headers') {
    input = `<textarea data-spec="${name}" data-type="${type}" rows="2" class="mono" spellcheck="false"></textarea>`;
  } else {
    input = `<input data-spec="${name}" class="mono" spellcheck="false">`;
  }
  return `<label class="field${type === 'code' ? ' wide' : ''}"><span class="lbl">${esc(label)}</span>${input}</label>`;
}

function buildSpecBox(box) {
  const kind = box.dataset.kind;
  const opts = presets.filter(p => p.kind === kind)
    .map(p => `<option value="${esc(p.id)}" data-desc="${esc(p.desc)}"${p.id === 'custom' ? ' data-sep="1"' : ''}>${esc(p.name)}</option>`).join('');
  const groups = SPEC_GROUPS.map(([title, fields]) =>
    `<fieldset><legend>${esc(title)}</legend><div class="spec-grid">${fields.map(fieldHTML).join('')}</div></fieldset>`).join('');
  box.innerHTML = `
    <div class="field"><span class="lbl">接口格式</span><select class="preset-select">${opts}</select></div>
    <p class="preset-desc"></p>
    <details class="spec-details">
      <summary>接口详情 <span class="muted">· 可修改，改动后自动变为「自定义接口」</span></summary>
      <div class="spec-inner">
        <div class="var-help">${VAR_HELP}</div>
        ${groups}
        <div class="preview-actions">
          <button type="button" class="btn" data-preview="0">预览请求</button>
          <button type="button" class="btn" data-preview="1">预览带参考图的请求</button>
        </div>
        <pre class="preview" hidden></pre>
      </div>
    </details>`;

  const select = $('.preset-select', box);
  select.onchange = () => {
    const p = presets.find(x => x.kind === kind && x.id === select.value);
    if (!p) return;
    const keep = readSpec(box); // 切换预设时保留专用地址和 Key
    fillSpec(box, { ...structuredClone(p.spec), base_url: keep.base_url, api_keys: keep.api_keys });
    $('.preview', box).hidden = true;
    if (p.id === 'custom') $('.spec-details', box).open = true;
  };
  box.addEventListener('input', e => {
    if (e.target.dataset.spec && !['spec.base_url', 'spec.api_keys'].includes(e.target.dataset.spec)) markCustom(box);
  });
  box.addEventListener('change', e => {
    if (e.target.dataset.spec && e.target.tagName === 'SELECT') markCustom(box);
  });
  $$('[data-preview]', box).forEach(btn => {
    btn.onclick = async () => {
      const pre = $('.preview', box);
      pre.hidden = false;
      pre.textContent = '生成中…';
      try {
        const r = await api('POST', '/api/preview', {
          kind, spec: readSpec(box), base_url: sf.elements.base_url.value,
          model: sf.elements[kind + '_model'].value, with_ref: btn.dataset.preview === '1', extra_params: readXP(),
        });
        pre.textContent = r.ok ? r.text : '错误：' + r.error;
      } catch (err) {
        pre.textContent = '错误：' + err.message;
      }
    };
  });
}

function markCustom(box) {
  const select = $('.preset-select', box);
  if (select.value !== 'custom') {
    select.value = 'custom';
    updatePresetDesc(box);
  }
}

function presetName(kind, id) {
  const p = presets.find(x => x.kind === kind && x.id === id);
  return p ? p.name : '自定义接口';
}

function updatePresetDesc(box) {
  const p = presets.find(x => x.kind === box.dataset.kind && x.id === $('.preset-select', box).value);
  let text = p ? p.desc : '';
  if (p && p.base_hint) text += `　官方地址：${p.base_hint}`;
  $('.preset-desc', box).textContent = text;
  $('[data-spec="spec.base_url"]', box).placeholder =
    p && p.base_hint ? `留空使用通用接口地址；官方地址 ${p.base_hint}` : '留空使用通用接口地址';
}

function fillSpec(box, spec) {
  for (const el of $$('[data-spec]', box)) {
    const v = getPath({ spec }, el.dataset.spec);
    if (el.dataset.type === 'lines') el.value = (v || []).join('\n');
    else if (el.dataset.type === 'headers') el.value = Object.entries(v || {}).map(([k, x]) => `${k}: ${x}`).join('\n');
    else if (el.tagName === 'SELECT') el.value = v || el.options[0].value;
    else el.value = v ?? '';
  }
  const known = presets.some(p => p.kind === box.dataset.kind && p.id === spec.preset);
  $('.preset-select', box).value = known ? spec.preset : 'custom';
  updatePresetDesc(box);
}

function readSpec(box) {
  const holder = { spec: {} };
  for (const el of $$('[data-spec]', box)) {
    let v = el.value;
    if (el.dataset.type === 'lines') {
      v = v.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
    } else if (el.dataset.type === 'headers') {
      v = {};
      for (const line of el.value.split(/\r?\n/)) {
        const i = line.indexOf(':');
        if (i > 0) v[line.slice(0, i).trim()] = line.slice(i + 1).trim();
      }
    } else if (el.tagName !== 'TEXTAREA') {
      v = v.trim();
    }
    setPath(holder, el.dataset.spec, v);
  }
  holder.spec.preset = $('.preset-select', box).value;
  return holder.spec;
}

function configReady() {
  if (!config) return false;
  const has = spec => (spec.base_url || config.base_url) &&
    ((spec.api_keys || []).length || (config.api_keys || []).length);
  return !!(has(config.image_spec || {}) || has(config.video_spec || {}));
}

async function loadConfig() {
  config = await api('GET', '/api/config');
  for (const [k, v] of Object.entries(config)) {
    const el = sf.elements[k];
    if (el && !el.dataset.spec) el.value = Array.isArray(v) ? v.join('\n') : (v ?? '');
  }
  for (const box of $$('.spec-box')) {
    fillSpec(box, config[box.dataset.kind + '_spec'] || {});
    $('.preview', box).hidden = true;
  }
  renderXP(config.extra_params);
  $('#autoFix').checked = !config.manual_fix;
  fillIH(config.image_host);
  $('#outputRoot').textContent = '当前保存位置：' + config.output_root;
  $('#outRootPath').textContent = config.output_root;
  $('#needConfig').hidden = configReady();
  applyKindDefaults(true);
  updateSideStatus();
}

function readSettings() {
  const o = {};
  for (const el of sf.elements) {
    if (!el.name || el.dataset.spec) continue;
    if (el.name === 'api_keys') o.api_keys = el.value.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
    else if (el.type === 'number') o[el.name] = Number(el.value) || 0;
    else o[el.name] = el.value.trim();
  }
  for (const box of $$('.spec-box')) o[box.dataset.kind + '_spec'] = readSpec(box);
  o.extra_params = readXP();
  o.manual_fix = !$('#autoFix').checked;
  o.image_host = readIH();
  return o;
}

sf.onsubmit = async e => {
  e.preventDefault();
  const msg = $('#saveMsg');
  try {
    await api('PUT', '/api/config', readSettings());
    await loadConfig();
    if (!modelOptions.length) loadModels();
    setMsg(msg, '已保存', 'ok');
    setTimeout(() => { if (msg.textContent === '已保存') setMsg(msg, ''); }, 2500);
  } catch (err) {
    setMsg(msg, err.message, 'err');
  }
};

$('#testBtn').onclick = async () => {
  const msg = $('#testMsg');
  const btn = $('#testBtn');
  setMsg(msg, '正在连接…');
  btn.disabled = true;
  try {
    const s = readSettings();
    const r = await api('POST', '/api/test', { base_url: s.base_url, api_keys: s.api_keys, models_path: s.models_path });
    if (r.ok) {
      setMsg(msg, `连接成功，可用模型 ${r.models.length} 个。点模型输入框右侧的箭头就能选择。`, 'ok');
      modelOptions = r.models;
    } else {
      setMsg(msg, r.error, 'err');
    }
  } catch (err) {
    setMsg(msg, err.message, 'err');
  } finally {
    btn.disabled = false;
  }
};

/* ---------- 参考图上传（图床） ---------- */

function fillIH(h = {}) {
  for (const el of $$('[data-ih]')) {
    const v = h[el.dataset.ih];
    if (el.type === 'checkbox') el.checked = !!v;
    else el.value = v ?? (el.tagName === 'SELECT' ? el.options[0].value : '');
  }
  if (!h.expiry) $('[data-ih=expiry]').value = '24h';
  $('#ihBox').dataset.mode = $('#ihMode').value;
}

function readIH() {
  const o = {};
  for (const el of $$('[data-ih]')) o[el.dataset.ih] = el.type === 'checkbox' ? el.checked : el.value.trim();
  return o;
}

$('#ihMode').addEventListener('change', () => { $('#ihBox').dataset.mode = $('#ihMode').value; });
$('#ihTest').onclick = async () => {
  const msg = $('#ihMsg');
  const btn = $('#ihTest');
  setMsg(msg, '正在上传测试图片…');
  btn.disabled = true;
  try {
    const r = await api('POST', '/api/test-upload', readIH());
    if (r.ok) setMsg(msg, `上传成功，公开网址可以访问：${r.url}`, 'ok');
    else setMsg(msg, r.error + (r.url ? `（${r.url}）` : ''), 'err');
  } catch (err) {
    setMsg(msg, err.message, 'err');
  } finally {
    btn.disabled = false;
  }
};

/* ---------- 附加参数 ---------- */

function xpRow(p = {}) {
  const kind = p.kind || 'video';
  const row = document.createElement('div');
  row.className = 'xp-row';
  row.innerHTML = `
    <select data-xp="kind" aria-label="类型"><option value="video">视频</option><option value="image">图片</option></select>
    <input data-xp="model" data-combo="model" data-kind="${kind}" class="mono" placeholder="全部模型" aria-label="模型">
    <input data-xp="key" class="mono" placeholder="如 mode" aria-label="参数名" spellcheck="false">
    <input data-xp="value" class="mono" placeholder="如 references" aria-label="值" spellcheck="false">
    <button type="button" class="icon-btn danger" data-xp-del title="删除这条" aria-label="删除这条">${icon('trash')}</button>`;
  $('[data-xp=kind]', row).value = kind;
  $('[data-xp=model]', row).value = p.model || '';
  $('[data-xp=key]', row).value = p.key || '';
  $('[data-xp=value]', row).value = p.value ?? '';
  $('#xpRows').append(row);
  enhanceSelect($('[data-xp=kind]', row));
  enhanceCombo($('[data-xp=model]', row));
  return row;
}

function renderXP(list) {
  $('#xpRows').innerHTML = '';
  (list || []).forEach(p => xpRow(p));
  $('#xpEmpty').hidden = (list || []).length > 0;
}

function readXP() {
  return $$('#xpRows .xp-row').map(row => ({
    kind: $('[data-xp=kind]', row).value,
    model: $('[data-xp=model]', row).value.trim(),
    key: $('[data-xp=key]', row).value.trim(),
    value: $('[data-xp=value]', row).value.trim(),
  })).filter(p => p.key);
}

$('#xpAdd').onclick = () => {
  const row = xpRow({});
  $('#xpEmpty').hidden = true;
  $('[data-xp=key]', row).focus();
};
$('#xpRows').addEventListener('click', e => {
  const del = e.target.closest('[data-xp-del]');
  if (!del) return;
  del.closest('.xp-row').remove();
  $('#xpEmpty').hidden = $$('#xpRows .xp-row').length > 0;
});
$('#xpRows').addEventListener('change', e => {
  if (e.target.dataset.xp === 'kind') {
    $('[data-xp=model]', e.target.closest('.xp-row')).dataset.kind = e.target.value;
  }
});

/* ---------- 侧边栏状态 ---------- */

function updateSideStatus(activeCount = 0) {
  const box = $('#sideStatus');
  const ready = configReady();
  box.classList.toggle('ready', ready);
  box.classList.toggle('busy', activeCount > 0);
  $('#ssText').textContent = activeCount > 0 ? `${activeCount} 个任务进行中` : ready ? '接口已配置' : '未配置接口';
  if (!updateInfo.current) $('#ssVer').textContent = serverVersion ? '版本 ' + serverVersion.slice(0, 16) : '';
  if (!config) return;
  let host = '—';
  try { if (config.base_url) host = new URL(config.base_url).host; } catch { host = config.base_url; }
  $('#ssHost').textContent = host;
  $('#ssImage').textContent = presetName('image', (config.image_spec || {}).preset);
  $('#ssVideo').textContent = presetName('video', (config.video_spec || {}).preset);
}

/* ---------- 新建批次 ---------- */

// 「同时生成」：当前类型（图片/视频）的并发数，改了立即保存，对所有批次生效。
const concInput = $('#concurrency');
function concKey() {
  return cf.elements.kind.value === 'image' ? 'image_concurrency' : 'video_concurrency';
}
function syncConcurrency() {
  if (!config) return;
  const n = config[concKey()];
  concInput.value = n;
  $('#concHint').textContent = `最多同时提交 ${n} 个${cf.elements.kind.value === 'image' ? '图片' : '视频'}任务，其余排队。`;
}
let concTimer = 0;
function saveConcurrency() {
  const n = Math.max(1, Math.min(50, Math.round(Number(concInput.value) || 1)));
  concInput.value = n;
  clearTimeout(concTimer);
  concTimer = setTimeout(async () => {
    const key = concKey();
    try {
      config = await api('PUT', '/api/config', { [key]: n });
      sf.elements[key].value = config[key];
      syncConcurrency();
    } catch (err) {
      setMsg($('#createMsg'), err.message, 'err');
    }
  }, 350);
}
concInput.addEventListener('change', saveConcurrency);
$$('[data-cstep]').forEach(b => {
  b.onclick = () => {
    concInput.value = (Number(concInput.value) || 1) + Number(b.dataset.cstep);
    saveConcurrency();
  };
});

const cf = $('#createForm');
let lastKind = 'image';
let csvRows = 0;

const SIZE_CHIPS = {
  image: [['方形', '1024x1024'], ['横版', '1536x1024'], ['竖版', '1024x1536']],
  video: [['横屏', '1280x720'], ['竖屏', '720x1280'], ['1080p', '1920x1080']],
  pipeline: [['横屏', '16:9'], ['竖屏', '9:16'], ['方形', '1:1'], ['横版', '4:3'], ['竖版', '3:4'], ['宽银幕', '21:9']],
};

function defaultModel(kind) {
  if (!config) return '';
  return (kind === 'video' ? config.video_model : config.image_model) || '';
}

// 切换类型时，如果模型还是上一个类型的默认值（或空），就换成新类型的默认模型。
function applyKindDefaults(force) {
  const kind = cf.elements.kind.value;
  const pipeline = kind === 'pipeline';
  cf.classList.toggle('is-video', kind !== 'image');
  cf.classList.toggle('is-pipeline', pipeline);
  $('#modelLabel').textContent = pipeline ? '图片模型' : '模型';
  $('#sizeLabel').textContent = pipeline ? '比例（图片和视频共用）' : '尺寸';
  $('#countLabel').textContent = pipeline ? '每条出图' : '每条生成';
  if (pipeline && !cf.elements.video_model.value) cf.elements.video_model.value = defaultModel('video');
  if (pipeline && !cf.elements.size.value) cf.elements.size.value = '16:9';
  if (!pipeline && SIZE_CHIPS.pipeline.some(([, v]) => v === cf.elements.size.value)) cf.elements.size.value = '';
  const m = cf.elements.model;
  if (force || !m.value || m.value === defaultModel(lastKind)) m.value = defaultModel(kind);
  lastKind = kind;
  renderSizeChips();
  updateMode();
  syncConcurrency();
  updateSummary();
}

function renderSizeChips() {
  const kind = cf.elements.kind.value;
  const cur = cf.elements.size.value.trim();
  $('#sizeChips').innerHTML = SIZE_CHIPS[kind].map(([label, v]) =>
    `<button type="button" class="chip-btn${cur === v ? ' on' : ''}" data-size="${v}">${label}<small>${v}</small></button>`).join('') +
    `<button type="button" class="chip-btn${cur === '' ? ' on' : ''}" data-size="">接口默认</button>`;
}
$('#sizeChips').addEventListener('click', e => {
  const b = e.target.closest('[data-size]');
  if (!b) return;
  cf.elements.size.value = b.dataset.size;
  renderSizeChips();
});

const clampCount = n => Math.max(1, Math.min(20, Math.round(n) || 1));

function promptCount() {
  return cf.elements.prompts.value.split('\n').map(s => s.trim()).filter(s => s && !s.startsWith('#')).length;
}

let lastSum = -1;
function updateSummary() {
  const isCSV = cf.elements.src.value === 'csv';
  $('.src-text', cf).hidden = isCSV;
  $('.src-csv', cf).hidden = !isCSV;
  const count = Math.max(1, Number(cf.elements.count.value) || 1);
  const n = isCSV ? csvRows : promptCount();
  const total = n * count;

  const pill = $('#lineCount');
  pill.textContent = isCSV ? (csvRows ? `${csvRows} 行` : '未选择') : `${n} 条`;
  pill.classList.toggle('has', n > 0);

  const num = $('#sumNum');
  num.textContent = total;
  num.classList.toggle('zero', total === 0);
  if (total !== lastSum && lastSum !== -1) {
    num.classList.remove('bump');
    void num.offsetWidth;
    num.classList.add('bump');
  }
  lastSum = total;
  const perImage = Math.max(1, Number(cf.elements.per_image.value) || 1);
  const pipeline = cf.elements.kind.value === 'pipeline';
  $('#sumUnit').textContent = pipeline ? '张图片' : '个任务';
  $('#sumDetail').textContent = total === 0
    ? (isCSV ? '请选择 CSV 文件' : '还没有提示词')
    : pipeline ? (cf.elements.auto.checked
      ? `完成后自动生成 ${total * perImage} 个视频（每张 ${perImage} 个）`
      : `选图后，每张选中的图生成 ${perImage} 个视频`)
    : isCSV ? `CSV ${csvRows} 行 × ${count}（CSV 填了数量的以 CSV 为准）` : `${n} 条提示词 × 每条 ${count} 个`;
}

cf.addEventListener('change', e => {
  if (e.target.name === 'kind') applyKindDefaults(false);
  else if (e.target.name === 'mode') updateMode();
  else updateSummary();
});
cf.addEventListener('input', e => {
  if (e.target.name === 'size') renderSizeChips();
  updateSummary();
});
cf.elements.prompts.addEventListener('keydown', e => {
  if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
    e.preventDefault();
    cf.requestSubmit();
  }
});

// 数量步进
$$('.stepper [data-step]', cf).forEach(b => {
  b.onclick = () => {
    const input = cf.elements.count;
    input.value = Math.max(1, Math.min(20, (Number(input.value) || 1) + Number(b.dataset.step)));
    updateSummary();
  };
});

$$('[data-pstep]', cf).forEach(b => {
  b.onclick = () => {
    const input = cf.elements.per_image;
    input.value = clampCount(Number(input.value) + Number(b.dataset.pstep));
    updateSummary();
  };
});

// 拖放文件
// 文件落在投放区以外时默认会把整个页面换成该文件，而应用窗口没有后退按钮。
for (const type of ['dragover', 'drop']) document.addEventListener(type, e => e.preventDefault());

function setupDrop(zone, onFile) {
  const input = $('input[type=file]', zone);
  zone.addEventListener('dragover', e => { e.preventDefault(); zone.classList.add('over'); });
  zone.addEventListener('dragleave', () => zone.classList.remove('over'));
  zone.addEventListener('drop', e => {
    e.preventDefault();
    zone.classList.remove('over');
    if (e.dataTransfer.files.length) {
      input.files = e.dataTransfer.files;
      input.dispatchEvent(new Event('change', { bubbles: true }));
    }
  });
  input.addEventListener('change', () => onFile(input.files[0] || null, input.files));
}

setupDrop($('#csvDrop'), async file => {
  csvRows = 0;
  $('#csvDrop').classList.toggle('has', !!file);
  if (file) {
    const text = await file.text();
    csvRows = Math.max(0, text.split(/\r?\n/).filter(l => l.trim()).length - 1);
    $('#csvName').textContent = file.name;
    $('#csvInfo').textContent = `${fmtBytes(file.size)} · 约 ${csvRows} 行数据`;
  } else {
    $('#csvName').textContent = '拖入 CSV 文件，或点击选择';
    $('#csvInfo').textContent = '第一行是表头，必须有 prompt（或「提示词」）列';
  }
  updateSummary();
});

// 参考图上传位：拖放或点击选择，显示缩略图、文件名和大小。
function refSlot(zone, clearBtn) {
  let url = null;
  const show = file => {
    if (url) URL.revokeObjectURL(url);
    url = file ? URL.createObjectURL(file) : null;
    const img = $('img', zone);
    img.hidden = !file;
    if (file) img.src = url;
    zone.classList.toggle('has', !!file);
    $('.drop-empty', zone).hidden = !!file;
    $('.drop-file', zone).hidden = !file;
    clearBtn.hidden = !file;
    if (file) {
      $('.drop-file b', zone).textContent = file.name;
      $('.drop-file .muted', zone).textContent = fmtBytes(file.size);
    }
  };
  setupDrop(zone, show);
  clearBtn.onclick = () => {
    $('input[type=file]', zone).value = '';
    show(null);
  };
}
refSlot($('#lastDrop'), $('#lastClear'));

// 参考图：可以一次选多张，也可以分几次加，超过设置的上限时多余的不加入。
let refFiles = []; // { file, url }

function manyRefs() {
  if (!config) return true;
  return cf.elements.kind.value === 'video' ? config.video_many_refs : config.image_many_refs;
}

function refLimit() {
  if (cf.classList.contains('is-frames') || !manyRefs()) return 1;
  return (config && config.max_ref_images) || 4;
}

function renderRefs() {
  const limit = refLimit();
  const box = $('#refThumbs');
  box.hidden = !refFiles.length;
  box.innerHTML = refFiles.map((r, i) => `
    <div class="ref-thumb" title="${esc(r.file.name)}（${fmtBytes(r.file.size)}）">
      <img src="${r.url}" alt=""><span class="ref-no mono">${i + 1}</span>
      <button type="button" data-ref-del="${i}" aria-label="移除第 ${i + 1} 张">${icon('x')}</button>
    </div>`).join('');
  $('#refDrop').hidden = refFiles.length >= limit;
  $('#refClear').hidden = refFiles.length < 2;
  $('#refDropText').textContent = refFiles.length ? `继续添加（${refFiles.length}/${limit}）` : '拖入或点击上传，可以多选';
}

function setRefs(list) {
  const keep = new Set(list);
  for (const r of refFiles) if (!keep.has(r)) URL.revokeObjectURL(r.url);
  refFiles = list;
  renderRefs();
}

function addRefs(files) {
  const limit = refLimit();
  const room = Math.max(0, limit - refFiles.length);
  const incoming = [...files].filter(f => f.type.startsWith('image/'));
  setMsg($('#createMsg'), incoming.length > room ? `最多上传 ${limit} 张参考图，多余的没有加入。可以在「设置 → 运行与存储」里调整。` : '', 'err');
  setRefs([...refFiles, ...incoming.slice(0, room).map(file => ({ file, url: URL.createObjectURL(file) }))]);
}

setupDrop($('#refDrop'), (_, files) => {
  addRefs(files);
  cf.elements.ref.value = '';
});
$('#refClear').onclick = () => setRefs([]);
$('#refThumbs').addEventListener('click', e => {
  const b = e.target.closest('[data-ref-del]');
  if (b) setRefs(refFiles.filter((_, i) => i !== Number(b.dataset.refDel)));
});

const MODE_HINTS = {
  '': '接口要求 mode 时自动填：一般用全能参考，CSV 里填了「尾帧」列的用首尾帧。接口不要求就不发送，对其它接口没有影响。',
  references: '文生视频或参考图生视频，提交时带上 mode=references。',
  frames: '用首帧（和可选的尾帧）控制画面，提交时带上 mode=frames。需要上传首帧。',
};

const PIPELINE_MODE_HINTS = {
  '': '接口要求 mode 时自动填，一般用参考图模式。',
  references: '生成的图片作为参考图，提交时带上 mode=references。',
  frames: '生成的图片作为视频首帧，提交时带上 mode=frames。需要模型支持首尾帧。',
};

// 根据类型和生成模式切换参考图区域：首尾帧模式显示「首帧 + 尾帧」两个上传位。
function updateMode() {
  const video = cf.elements.kind.value === 'video';
  const mode = video ? cf.elements.mode.value : '';
  const frames = mode === 'frames';
  cf.classList.toggle('is-frames', frames);
  const pipeline = cf.elements.kind.value === 'pipeline';
  $('#modeHint').textContent = pipeline ? PIPELINE_MODE_HINTS[cf.elements.mode.value] : MODE_HINTS[mode] || '';
  $$('.seg', cf).forEach(syncThumb);
  $('#refLabel').innerHTML = frames
    ? '首帧和尾帧 <em>首帧必填，尾帧可选</em>'
    : manyRefs()
      ? `参考图 <em>可选 · 最多 ${refLimit()} 张 · ${video ? '有参考图时走图生视频' : '有参考图时走图生图'}</em>`
      : `参考图 <em>可选 · 当前接口格式只支持 1 张，可在设置里更换</em>`;
  if (refFiles.length > refLimit()) setRefs(refFiles.slice(0, refLimit()));
  renderRefs();
  if (!frames && cf.elements.ref_last.files.length) $('#lastClear').click();
}

cf.onsubmit = async e => {
  e.preventDefault();
  const msg = $('#createMsg');
  if (!configReady()) {
    setMsg(msg, '请先在「设置」里填写接口地址和 API Key', 'err');
    return;
  }
  const isVideo = cf.elements.kind.value === 'video';
  if (isVideo && cf.elements.mode.value === 'frames' && cf.elements.src.value === 'text' && !refFiles.length) {
    setMsg(msg, '首尾帧模式需要上传首帧图片', 'err');
    return;
  }
  const fd = new FormData(cf);
  if (cf.elements.src.value === 'csv') fd.delete('prompts'); else fd.delete('csv');
  fd.delete('ref');
  for (const r of refFiles) fd.append('ref', r.file);
  if (!cf.elements.ref_last.files.length || !cf.classList.contains('is-frames')) fd.delete('ref_last');
  if (cf.elements.kind.value === 'image') fd.delete('mode');
  if (!cf.elements.csv.files.length) fd.delete('csv');

  const btn = $('#createBtn');
  btn.disabled = true;
  setMsg(msg, '');
  try {
    const r = await api('POST', '/api/batches', fd);
    cf.elements.prompts.value = '';
    cf.elements.name.value = '';
    updateSummary();
    setMsg(msg, `已创建 ${r.count} 个任务`, 'ok');
    setTimeout(() => setMsg(msg, ''), 3000);
    openState.set(r.id, true);
    showTab('tasks');
  } catch (err) {
    setMsg(msg, err.message, 'err');
  } finally {
    btn.disabled = false;
  }
};

/* ---------- 任务列表 ---------- */

const STATUS = {
  pending: ['排队中', 's-wait'],
  submitting: ['生成中', 's-run'],
  running: ['生成中', 's-run'],
  downloading: ['下载中', 's-run'],
  success: ['完成', 's-ok'],
  failed: ['失败', 's-err'],
  canceled: ['已取消', 's-cancel'],
};
const ACTIVE = new Set(['pending', 'submitting', 'running', 'downloading']);
const batchEls = new Map();
const cardSigs = new Map();
const openState = new Map();
const picks = new Map(); // 批次 id → 勾选的图片任务 id
let firstLoad = true;

function fmtTime(s) {
  const d = new Date(s);
  const p = n => String(n).padStart(2, '0');
  return `${d.getMonth() + 1}月${d.getDate()}日 ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function countJobs(b) {
  const c = { success: 0, failed: 0, canceled: 0, active: 0, pending: 0 };
  for (const j of b.jobs) {
    if (j.status === 'pending') c.pending++;
    else if (ACTIVE.has(j.status)) c.active++;
    else c[j.status] = (c[j.status] || 0) + 1;
  }
  return c;
}

function createBatchEl(b) {
  const el = document.createElement('article');
  el.className = 'batch';
  el.dataset.id = b.id;
  el.innerHTML = `
    <header class="batch-head">
      <span class="batch-lead">
        <input type="checkbox" class="batch-check" data-act="batch-check" aria-label="选择这个批次">
        <button class="batch-toggle" data-act="toggle" aria-label="展开或收起">${icon('chevron')}</button>
      </span>
      <div class="batch-title" data-act="toggle"><h3></h3><p></p></div>
      <div class="batch-stats"></div>
      <div class="batch-actions">
        <button class="icon-btn" data-act="again" title="整批再次生成" aria-label="整批再次生成">${icon('repeat')}</button>
        <button class="icon-btn" data-act="edit" title="重新编辑：套用这批的设置" aria-label="重新编辑">${icon('edit')}</button>
        <button class="icon-btn" data-act="retry" title="重试失败的任务" aria-label="重试失败的任务">${icon('retry')}</button>
        <button class="icon-btn" data-act="cancel" title="取消未完成的任务" aria-label="取消未完成的任务">${icon('x')}</button>
        <button class="icon-btn danger" data-act="delete" title="删除记录（文件保留）" aria-label="删除记录">${icon('trash')}</button>
      </div>
    </header>
    <div class="batch-path">
      ${icon('folder')}<span class="bp-label">保存到</span>
      <code class="mono bp-dir"></code>
      <button class="btn btn-sm" data-act="open">打开文件夹</button>
      <button class="btn btn-sm" data-act="copy">复制路径</button>
    </div>
    <div class="segbar" aria-hidden="true"><i class="ok"></i><i class="run"></i><i class="err"></i><i class="rest"></i></div>
    <div class="batch-body"><div class="batch-inner">
    <div class="pick-bar" hidden>
      <span class="pb-text"></span>
      <button class="btn btn-sm" data-act="pick-all">全选</button>
      <button class="btn btn-sm" data-act="pick-none">清空</button>
      <button class="btn btn-sm btn-danger" data-act="pick-delete">${icon('trash')}删除</button>
      <button class="btn btn-sm btn-primary" data-act="make-videos">${icon('film')}生成视频</button>
    </div>
    <div class="grid"></div>
    </div></div>`;
  $('.batch-title h3', el).textContent = b.name;
  $('.bp-dir', el).textContent = b.out_dir;
  el.dataset.dir = b.out_dir;
  return el;
}

// 视频排在来源图片后面，图片和它的视频成组显示。
function orderJobs(b) {
  const videos = new Map();
  for (const j of b.jobs) {
    if (!j.parent_id) continue;
    if (!videos.has(j.parent_id)) videos.set(j.parent_id, []);
    videos.get(j.parent_id).push(j);
  }
  const list = [];
  for (const j of b.jobs) {
    if (j.parent_id) continue;
    list.push(j);
    if (videos.has(j.id)) list.push(...videos.get(j.id));
  }
  return { list, videos };
}

const pickable = j => j.kind === 'image' && j.status === 'success' && j.outputs && j.outputs.length > 0;
const selectable = j => !ACTIVE.has(j.status); // 能勾选（删除）的任务

function pickSet(id) {
  if (!picks.has(id)) picks.set(id, new Set());
  return picks.get(id);
}

// 等待挑图的图片：挑图模式下已完成、还没有生成视频的。
function waitingImages(b, videos) {
  if (!b.pipeline || b.pipeline.auto) return [];
  return b.jobs.filter(j => pickable(j) && !videos.has(j.id));
}

function updatePickBar(el, b, videos) {
  const sel = pickSet(b.id);
  const waiting = waitingImages(b, videos);
  const bar = $('.pick-bar', el);
  const all = b.jobs.filter(selectable);
  const images = all.filter(j => sel.has(j.id) && pickable(j)).length;
  bar.hidden = !all.length;
  el.classList.toggle('picking', sel.size > 0 || waiting.length > 0);
  bar.classList.toggle('idle', !sel.size && !waiting.length);
  $('.pb-text', bar).textContent = sel.size
    ? `已选 ${sel.size} 个任务`
    : waiting.length ? `${waiting.length} 张图片已完成，勾选满意的继续生成视频` : '勾选任务可以批量操作';
  $('[data-act=pick-all]', bar).hidden = sel.size === all.length;
  $('[data-act=pick-none]', bar).hidden = !sel.size;
  $('[data-act=pick-delete]', bar).hidden = !sel.size;
  const make = $('[data-act=make-videos]', bar);
  make.hidden = !images && !waiting.length;
  make.disabled = !images;
  for (const card of $('.grid', el).children) card.classList.toggle('picked', sel.has(card.dataset.id));
}

function updateBatch(el, b, c) {
  const total = b.jobs.length;
  const images = b.jobs.filter(j => j.kind !== 'video').length;
  const videoJobs = total - images;
  const kinds = [images ? `${images} 张图片` : '', videoJobs ? `${videoJobs} 段视频` : ''].filter(Boolean).join('、');
  $('.batch-title p', el).textContent = `${fmtTime(b.created_at)} · ${kinds}`;
  const { list, videos } = orderJobs(b);
  const sel = pickSet(b.id);
  for (const id of sel) {
    const j = b.jobs.find(x => x.id === id);
    if (!j || !selectable(j)) sel.delete(id);
  }
  const stat = (cls, label, n) => `<span class="${cls}"><i></i>${label} <b>${n}</b></span>`;
  const parts = [stat('s-ok', '完成', c.success)];
  if (c.active) parts.push(stat('s-run', '进行中', c.active));
  if (c.pending) parts.push(stat('s-wait', '排队', c.pending));
  if (c.failed + c.canceled) parts.push(stat('s-err', '失败', c.failed + c.canceled));
  parts.push(`<span>共 <b>${total}</b></span>`);
  $('.batch-stats', el).innerHTML = parts.join('');

  const seg = $('.segbar', el).children;
  seg[0].style.flexGrow = c.success;
  seg[1].style.flexGrow = c.active;
  seg[2].style.flexGrow = c.failed + c.canceled;
  seg[3].style.flexGrow = c.pending;

  $('[data-act=retry]', el).hidden = !(c.failed + c.canceled);
  $('[data-act=cancel]', el).hidden = !(c.active + c.pending);
  $('[data-act=delete]', el).hidden = !!c.active;

  // 进行中或有失败的批次默认展开，方便看到进度和失败原因
  if (!openState.has(b.id)) openState.set(b.id, c.active + c.pending + c.failed + waitingImages(b, videos).length > 0);
  el.classList.toggle('open', openState.get(b.id));

  const grid = $('.grid', el);
  list.forEach((j, i) => {
    let card = grid.children[i];
    if (!card || card.dataset.id !== j.id) {
      // 新视频插进来后顺序会变，已有的卡片要挪过去，不能重建（重建会闪、视频要重新加载）。
      card = [...grid.children].find(x => x.dataset.id === j.id);
      if (!card) {
        card = document.createElement('div');
        card.className = 'job';
        card.dataset.id = j.id;
        card.style.setProperty('--i', i);
      }
      grid.insertBefore(card, grid.children[i] || null);
    }
    const count = (videos.get(j.id) || []).length;
    card.classList.toggle('shot-start', count > 0);
    const sig = [j.status, j.progress, j.error, j.note, (j.outputs || []).join('|'), JSON.stringify(j.fix || ''), count,
      config && config[j.kind === 'video' ? 'video_concurrency' : 'image_concurrency']].join('\u0001');
    if (cardSigs.get(j.id) !== sig) {
      cardSigs.set(j.id, sig);
      card.innerHTML = cardHTML(b, j, count);
    }
  });
  while (grid.children.length > list.length) grid.lastChild.remove();
  updatePickBar(el, b, videos);
}

function cardHTML(b, j, videoCount) {
  const [label, cls] = STATUS[j.status] || [j.status, ''];
  let media;
  if (j.status === 'success' && j.outputs && j.outputs.length) {
    const src = `/files/${encodeURIComponent(b.id)}/${encodeURIComponent(j.outputs[0])}`;
    const path = joinPath(b.out_dir, j.outputs[0]);
    const open = `<button class="open-btn" data-act="preview" data-src="${src}" data-kind="${j.kind}" data-id="${j.id}"
      data-path="${esc(path)}" data-caption="${esc(j.prompt)}" title="${esc(path)}" aria-label="预览"></button>`;
    media = j.kind === 'video'
      ? `<video src="${src}#t=0.1" preload="metadata" muted playsinline></video>${open}<span class="badge-play">${icon('film')}</span>`
      : `<img src="${src}" loading="lazy" alt="">${open}`;
  } else if (j.status === 'pending') {
    const limit = config ? config[j.kind === 'video' ? 'video_concurrency' : 'image_concurrency'] : '';
    media = `<div class="ph ph-wait">${icon('clock')}排队中${limit ? `<small>同时最多生成 ${limit} 个</small>` : ''}</div>`;
  } else if (ACTIVE.has(j.status)) {
    const pct = (j.status === 'running' || j.status === 'downloading') && j.progress ? `<span class="pct">${j.progress}%</span>` : '';
    media = `<div class="ph ph-run">${pct}<span>${label}</span></div><span class="progress-line" style="width:${j.progress || 0}%"></span>`;
  } else if (j.status === 'failed') {
    media = `<div class="ph ph-err">${icon('alert')}生成失败</div>`;
  } else {
    media = `<div class="ph">${icon('x')}${label}</div>`;
  }
  if (selectable(j)) media += `<button class="pick-btn" data-act="pick" data-id="${j.id}" title="勾选" aria-label="勾选这个任务">${icon('check')}</button>`;
  const fix = j.status === 'failed' && j.fix ? fixHTML(j) : '';
  const actions = [];
  if (j.status === 'success' && j.outputs && j.outputs.length) {
    actions.push(`<button class="btn" data-act="job-reveal" data-id="${j.id}" title="${esc(joinPath(b.out_dir, j.outputs[0]))}">${icon('folder')}打开位置</button>`);
  }
  if (b.pipeline && pickable(j) && !videoCount) actions.push(`<button class="btn" data-act="job-regen" data-id="${j.id}">${icon('retry')}重新生成</button>`);
  if ((j.status === 'failed' && !j.fix) || j.status === 'canceled') actions.push(`<button class="btn" data-act="job-retry" data-id="${j.id}">${icon('retry')}重试</button>`);
  if (ACTIVE.has(j.status)) actions.push(`<button class="btn" data-act="job-cancel" data-id="${j.id}">${icon('x')}取消</button>`);
  else {
    actions.push(`<button class="btn" data-act="job-again" data-id="${j.id}" title="用相同的设置再生成一个，原结果保留">${icon('repeat')}再次生成</button>`);
    actions.push(`<button class="btn" data-act="job-edit" data-id="${j.id}" title="把模型、提示词、比例等填回新建批次">${icon('edit')}重新编辑</button>`);
    actions.push(`<button class="btn job-del" data-act="job-delete" data-id="${j.id}" title="删除这个任务（文件保留）" aria-label="删除这个任务">${icon('trash')}</button>`);
  }
  const modeName = { references: '全能参考', frames: '首尾帧' }[j.mode] || '';
  const parent = j.parent_id && b.jobs.find(x => x.id === j.parent_id);
  const seq = n => `#${String(n).padStart(3, '0')}`;
  const info = [parent ? `来自图 ${seq(parent.index)}` : seq(j.index), j.model, modeName, j.size, j.duration ? j.duration + 's' : ''].filter(Boolean).join(' · ');
  return `
    <div class="media">${media}</div>
    <div class="job-body">
      <div class="job-meta"><span class="chip ${cls}">${label}</span><span class="info mono" title="${esc(info)}">${esc(info)}</span></div>
      <p class="prompt" title="${esc(j.prompt)}">${esc(j.prompt)}</p>
      ${j.error && j.status !== 'success' ? `<p class="err" title="${esc(j.error)}">${esc(j.error)}</p>` : ''}
      ${videoCount ? `<p class="note">${icon('film')}已生成 ${videoCount} 个视频任务</p>` : ''}
      ${j.note ? `<p class="note">${icon('check')}${esc(j.note)}</p>` : ''}
      ${fix}
      <div class="job-actions">${actions.join('')}</div>
    </div>`;
}

// 常见参数值的中文说明，只用于按钮上的小字
const FIX_LABELS = {
  references: '全能参考 · 文生/参考图', frames: '首尾帧', first_frame: '首帧', last_frame: '尾帧',
  text2video: '文生视频', image2video: '图生视频', std: '标准', pro: '高质量', fast: '快速',
};

function fixHTML(j) {
  const f = j.fix;
  const opts = f.options || [];
  const attrs = `data-act="job-fix" data-id="${j.id}" data-param="${esc(f.param)}"`;
  const body = opts.length
    ? opts.map(o => `<button class="btn btn-sm btn-fix" ${attrs} data-value="${esc(o)}">${esc(o)}${FIX_LABELS[o] ? `<small>${FIX_LABELS[o]}</small>` : ''}</button>`).join('')
    : `<button class="btn btn-sm btn-fix" ${attrs} data-value="{{uuid}}">自动生成唯一值<small>每次提交都不同，适合各种 ID</small></button>
       <input class="mono fix-input" placeholder="或填写 ${esc(f.param)} 的值" spellcheck="false"><button class="btn btn-sm btn-fix" ${attrs}>填入并重试</button>`;
  return `
    <div class="fix">
      <p>接口要求参数 <code>${esc(f.param)}</code>，${opts.length ? '选一个值' : '填一个值'}后自动重试，以后这个模型都会带上：</p>
      <div class="fix-actions">${body}</div>
    </div>`;
}

// 批次多选：全选只针对当前筛选下看得到的批次。
const batchSel = new Set();
let visibleBatches = [];

function updateBatchTools() {
  const shown = visibleBatches.filter(id => batchSel.has(id)).length;
  $('#batchTools').hidden = !visibleBatches.length;
  const all = $('#batchAll');
  all.checked = shown > 0 && shown === visibleBatches.length;
  all.indeterminate = shown > 0 && shown < visibleBatches.length;
  $('#batchSelText').textContent = batchSel.size ? `已选 ${batchSel.size} 个批次` : `共 ${visibleBatches.length} 个批次`;
  $('#batchDelete').hidden = !batchSel.size;
  for (const [id, el] of batchEls) $('.batch-check', el).checked = batchSel.has(id);
}

$('#batchAll').addEventListener('change', e => {
  for (const id of visibleBatches) if (e.target.checked) batchSel.add(id); else batchSel.delete(id);
  updateBatchTools();
});
$('#batchDelete').onclick = async () => {
  const ids = [...batchSel];
  if (!confirm(`删除选中的 ${ids.length} 个批次的记录？已生成的文件会保留在磁盘上。`)) return;
  try {
    const r = await api('POST', '/api/batches/delete', { ids });
    batchSel.clear();
    toast(r.skipped ? `已删除 ${r.deleted} 个批次，${r.skipped} 个还有任务在运行，已跳过` : `已删除 ${r.deleted} 个批次`);
    refreshBatches();
  } catch (err) {
    alert(err.message);
  }
};

// 重新编辑：把任务的设置填回新建批次，改完再提交。jobs 是要套用的任务，整批时每条提示词只填一次。
async function editFrom(b, jobs) {
  const first = jobs[0];
  const pipeline = b.pipeline && !first.parent_id;
  const el = cf.elements;
  el.kind.value = pipeline ? 'pipeline' : first.kind;
  el.src.value = 'text';
  applyKindDefaults(false);
  el.name.value = '';
  el.model.value = first.model;
  el.size.value = pipeline ? first.video_size || b.pipeline.size || '' : first.size || '';
  el.mode.value = first.mode || (pipeline && b.pipeline.mode) || '';
  if (first.kind === 'video' || pipeline) el.duration.value = first.duration || (pipeline && b.pipeline.duration) || el.duration.value;
  if (pipeline) {
    el.video_model.value = b.pipeline.model;
    el.per_image.value = b.pipeline.per_image;
    el.auto.checked = !!b.pipeline.auto;
    el.video_prompt.value = first.video_prompt || b.pipeline.prompt || '';
  }
  const prompts = [...new Set(jobs.map(j => j.prompt))];
  el.prompts.value = prompts.join('\n');
  el.count.value = clampCount(Math.floor(jobs.length / prompts.length));

  setRefs([]);
  updateMode();
  renderSizeChips();
  updateSummary();
  showTab('create');

  // 参考图放回上传区，本地文件被移动或删除时跳过。
  const srcs = [first.image, ...(first.refs || [])].filter(Boolean);
  const files = [];
  let lost = 0;
  for (let i = 0; i < srcs.length; i++) {
    const res = await fetch(`/api/jobs/${first.id}/refs/${i}`);
    if (!res.ok) { lost++; continue; }
    const blob = await res.blob();
    files.push(new File([blob], `reference${i + 1}.${blob.type.split('/')[1] || 'png'}`, { type: blob.type }));
  }
  addRefs(files);
  if (first.last_image && cf.classList.contains('is-frames')) {
    const res = await fetch(`/api/jobs/${first.id}/refs/last`);
    if (res.ok) {
      const blob = await res.blob();
      const dt = new DataTransfer();
      dt.items.add(new File([blob], `last.${blob.type.split('/')[1] || 'png'}`, { type: blob.type }));
      el.ref_last.files = dt.files;
      el.ref_last.dispatchEvent(new Event('change', { bubbles: true }));
    } else lost++;
  }
  toast(lost ? `已套用设置，有 ${lost} 张参考图找不到了` : '已套用之前的设置，修改后点「开始生成」');
}

let toastTimer = 0;
function toast(text) {
  const el = $('#toast');
  el.textContent = text;
  el.hidden = false;
  clearTimeout(toastTimer);
  el.classList.remove('leaving');
  toastTimer = setTimeout(() => leave(el), 4500);
}

function renderBatches(list) {
  const root = $('#batches');
  const ids = new Set(list.map(b => b.id));
  for (const [id, el] of batchEls) {
    if (!ids.has(id)) { el.remove(); batchEls.delete(id); }
  }
  const filter = $('#taskFilter input:checked').value;
  let active = 0, visible = 0;
  [...list].reverse().forEach((b, i) => {
    let el = batchEls.get(b.id);
    if (!el) { el = createBatchEl(b); batchEls.set(b.id, el); }
    if (root.children[i] !== el) root.insertBefore(el, root.children[i] || null);
    const c = countJobs(b);
    updateBatch(el, b, c);
    active += c.active + c.pending;
    const show = filter === 'all' || (filter === 'active' && c.active + c.pending > 0) || (filter === 'failed' && c.failed + c.canceled > 0);
    el.hidden = !show;
    if (show) visible++;
  });

  if (firstLoad) { $('#tasksSkeleton').hidden = true; firstLoad = false; }
  visibleBatches = list.filter(b => !batchEls.get(b.id).hidden).map(b => b.id);
  for (const id of batchSel) if (!ids.has(id)) batchSel.delete(id);
  updateBatchTools();
  const empty = $('#emptyTasks');
  empty.hidden = visible > 0;
  if (!visible) {
    const none = list.length === 0;
    $('#emptyTitle').textContent = none ? '还没有任务' : '没有符合筛选条件的批次';
    $('#emptyText').textContent = none ? '到「新建批次」粘贴几行提示词，结果会按批次显示在这里。' : '换一个筛选条件看看。';
    $('#emptyBtn').hidden = !none;
  }
  const badge = $('#activeBadge');
  badge.hidden = !active;
  badge.textContent = active;
  updateSideStatus(active);
}

let refreshing = false;
let lastList = [];
async function refreshBatches() {
  if (refreshing) return;
  refreshing = true;
  try {
    lastList = (await api('GET', '/api/batches')) || [];
    renderBatches(lastList);
  } catch (err) {
    console.error(err);
  } finally {
    refreshing = false;
  }
}
$('#taskFilter').addEventListener('change', () => renderBatches(lastList));

$('#batches').addEventListener('click', async e => {
  const t = e.target.closest('[data-act]');
  if (!t) return;
  const batchEl = t.closest('.batch');
  const bid = batchEl.dataset.id;
  try {
    switch (t.dataset.act) {
      case 'toggle':
        openState.set(bid, !batchEl.classList.contains('open'));
        batchEl.classList.toggle('open');
        return;
      case 'preview':
        openLightbox(t.dataset);
        return;
      case 'batch-check':
        if (t.checked) batchSel.add(bid); else batchSel.delete(bid);
        updateBatchTools();
        return;
      case 'again': {
        const n = lastList.find(x => x.id === bid).jobs.filter(j => !j.parent_id).length;
        if (!confirm(`用相同的设置再生成这批的 ${n} 个任务？\n会重新调用接口并计费，原来的结果保留。`)) return;
        const r = await api('POST', `/api/batches/${bid}/again`);
        toast(`已添加 ${r.count} 个任务`);
        break;
      }
      case 'edit': {
        const b = lastList.find(x => x.id === bid);
        await editFrom(b, b.jobs.filter(j => !j.parent_id));
        return;
      }
      case 'job-again':
        await api('POST', `/api/jobs/${t.dataset.id}/again`);
        toast('已添加 1 个任务，原结果保留');
        break;
      case 'job-edit': {
        const b = lastList.find(x => x.id === bid);
        await editFrom(b, [b.jobs.find(j => j.id === t.dataset.id)]);
        return;
      }
      case 'job-delete': {
        const job = lastList.find(x => x.id === bid).jobs.find(j => j.id === t.dataset.id);
        if (job.outputs && job.outputs.length && !confirm('删除这个任务的记录？已生成的文件会保留在磁盘上。')) return;
        await api('POST', `/api/batches/${bid}/jobs/delete`, { job_ids: [job.id] });
        pickSet(bid).delete(job.id);
        break;
      }
      case 'pick-delete': {
        const ids = [...pickSet(bid)];
        if (!confirm(`删除选中的 ${ids.length} 个任务记录？已生成的文件会保留在磁盘上。`)) return;
        const r = await api('POST', `/api/batches/${bid}/jobs/delete`, { job_ids: ids });
        pickSet(bid).clear();
        toast(`已删除 ${r.deleted} 个任务`);
        break;
      }
      case 'pick':
      case 'pick-all':
      case 'pick-none':
      case 'make-videos': {
        const b = lastList.find(x => x.id === bid);
        if (!b) return;
        const sel = pickSet(bid);
        const { videos } = orderJobs(b);
        if (t.dataset.act === 'make-videos') { openVideoDlg(b); return; }
        if (t.dataset.act === 'pick') {
          if (!sel.delete(t.dataset.id)) sel.add(t.dataset.id);
        } else if (t.dataset.act === 'pick-none') {
          sel.clear();
        } else {
          const waiting = waitingImages(b, videos);
          for (const j of waiting.length && !sel.size ? waiting : b.jobs.filter(selectable)) sel.add(j.id);
        }
        updatePickBar(batchEl, b, videos);
        return;
      }
      case 'copy':
        copyText(batchEl.dataset.dir, t);
        return;
      case 'job-reveal':
        await api('POST', `/api/jobs/${t.dataset.id}/reveal`);
        return;
      case 'job-fix': {
        const input = $('.fix-input', t.closest('.fix'));
        const value = t.dataset.value ?? (input ? input.value.trim() : '');
        if (!value) { if (input) input.focus(); return; }
        t.disabled = true;
        const r = await api('POST', `/api/jobs/${t.dataset.id}/fix`, { param: t.dataset.param, value });
        toast(`已记住：${r.model} 提交时带上 ${t.dataset.param} = ${value}。已重试 ${r.count} 个任务。可在「设置 → 附加参数」里修改。`);
        config = await api('GET', '/api/config');
        renderXP(config.extra_params);
        break;
      }
      case 'retry':
        await api('POST', `/api/batches/${bid}/retry`);
        break;
      case 'cancel':
        if (!confirm('取消本批次中所有未完成的任务？\n已经提交到远端的任务可能仍会继续生成并计费。')) return;
        await api('POST', `/api/batches/${bid}/cancel`);
        break;
      case 'open':
        await api('POST', `/api/batches/${bid}/open`);
        return;
      case 'delete':
        if (!confirm('删除这条批次记录？已生成的文件会保留在磁盘上。')) return;
        await api('DELETE', `/api/batches/${bid}`);
        break;
      case 'job-retry':
        await api('POST', `/api/jobs/${t.dataset.id}/retry`);
        break;
      case 'job-regen':
        await api('POST', `/api/jobs/${t.dataset.id}/regenerate`);
        break;
      case 'job-cancel':
        await api('POST', `/api/jobs/${t.dataset.id}/cancel`);
        break;
      default:
        return;
    }
    refreshBatches();
  } catch (err) {
    alert(err.message);
  }
});

/* ---------- 用选中的图片生成视频 ---------- */

const vd = $('#videoDlg');
const vf = $('#videoForm');
let vdBatch = null;

const chosenImages = b => b.jobs.filter(j => pickSet(b.id).has(j.id) && pickable(j));

function updateVideoSum() {
  const images = chosenImages(vdBatch).length;
  const per = clampCount(Number(vf.elements.per_image.value));
  $('#vdSum').textContent = `${images} 张图 × 每张 ${per} 个 = ${images * per} 个视频`;
}

function openVideoDlg(b) {
  vdBatch = b;
  const p = b.pipeline || {};
  const chosen = chosenImages(b);
  const own = chosen.filter(j => j.video_prompt).length;
  vf.elements.prompt.value = p.prompt || '';
  vf.elements.model.value = p.model || defaultModel('video');
  vf.elements.size.value = p.size || '';
  vf.elements.duration.value = p.duration || cf.elements.duration.value || 5;
  vf.elements.per_image.value = p.per_image || 4;
  vf.elements.mode.value = p.mode || '';
  $('#vdSource').textContent = `共选了 ${chosen.length} 张图片，每张图作为视频的参考图。`;
  $('#vdPromptHint').textContent = own ? `其中 ${own} 张有自己的视频提示词，优先使用` : '留空时用图片的提示词';
  setMsg($('#vdMsg'), '');
  updateVideoSum();
  vd.hidden = false;
  vf.elements.prompt.focus();
}

$('#vdCancel').onclick = () => leave(vd);
vd.addEventListener('mousedown', e => { if (e.target === vd) leave(vd); });
vf.addEventListener('input', updateVideoSum);
$$('[data-vstep]', vf).forEach(b => {
  b.onclick = () => {
    const input = vf.elements.per_image;
    input.value = clampCount(Number(input.value) + Number(b.dataset.vstep));
    updateVideoSum();
  };
});
vf.onsubmit = async e => {
  e.preventDefault();
  const btn = $('#vdSubmit');
  btn.disabled = true;
  try {
    const sel = pickSet(vdBatch.id);
    const r = await api('POST', `/api/batches/${vdBatch.id}/videos`, {
      job_ids: chosenImages(vdBatch).map(j => j.id),
      prompt: vf.elements.prompt.value,
      model: vf.elements.model.value,
      size: vf.elements.size.value,
      duration: Number(vf.elements.duration.value) || 0,
      mode: vf.elements.mode.value,
      per_image: clampCount(Number(vf.elements.per_image.value)),
    });
    sel.clear();
    leave(vd);
    toast(`已创建 ${r.count} 个视频任务`);
    refreshBatches();
  } catch (err) {
    setMsg($('#vdMsg'), err.message, 'err');
  } finally {
    btn.disabled = false;
  }
};

/* ---------- 自动更新 ---------- */

let updateInfo = {};
let updateLater = ''; // 这次运行里点过「稍后」的版本
let updateTimer = 0;
const pollUpdate = () => { clearTimeout(updateTimer); updateTimer = setTimeout(refreshUpdate, 1000); };
const UPDATE_BUSY = new Set(['downloading', 'waiting', 'installing']);

function renderUpdate() {
  const u = updateInfo;
  if (u.current && u.current !== 'dev') $('#ssVer').textContent = '版本 ' + u.current;
  const busy = UPDATE_BUSY.has(u.state);
  const banner = $('#updateBanner');
  banner.hidden = !u.version || (!busy && u.state !== 'failed' && updateLater === u.version);
  if (banner.hidden) return;
  const pct = u.size ? Math.min(100, Math.round(u.got / u.size * 100)) : 0;
  $('#ubTitle').textContent = {
    downloading: `正在下载新版本 ${u.version}… ${pct}%`,
    waiting: `新版本 ${u.version} 已下载，任务全部完成后自动更新`,
    installing: `正在更新到 ${u.version}，程序马上重新打开…`,
    failed: `更新到 ${u.version} 失败`,
  }[u.state] || `发现新版本 ${u.version}`;
  $('#ubNotes').textContent = u.state === 'failed' ? u.error : busy ? '' : u.notes || '';
  $('#ubApply').hidden = busy;
  $('#ubApply').textContent = u.state === 'failed' ? '重试' : '立即更新';
  $('#ubLater').hidden = busy;
}

async function refreshUpdate() {
  try {
    updateInfo = (await api('GET', '/api/update')) || {};
    renderUpdate();
  } catch {
    // 程序重启更新时连不上是正常的
  }
  clearTimeout(updateTimer);
  updateTimer = setTimeout(refreshUpdate, UPDATE_BUSY.has(updateInfo.state) ? 1000 : 60000);
}

$('#ubLater').onclick = () => {
  updateLater = updateInfo.version;
  renderUpdate();
};
$('#ubApply').onclick = async () => {
  try {
    await api('POST', '/api/update/apply');
    updateInfo.state = 'downloading';
    updateInfo.got = 0;
    renderUpdate();

    pollUpdate();
  } catch (err) {
    alert(err.message);
  }
};
$('#checkUpdate').onclick = async () => {
  const btn = $('#checkUpdate');
  btn.disabled = true;
  btn.textContent = '检查中…';
  try {
    updateInfo = await api('POST', '/api/update/check');
    updateLater = '';
    renderUpdate();
    if (!updateInfo.version) toast(`已经是最新版本（${updateInfo.current}）`);
  } catch (err) {
    toast(err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = '检查更新';
  }
};

/* ---------- 结果预览 ---------- */

const lb = $('#lightbox');
function openLightbox({ src, kind, caption, id, path }) {
  $('.lb-stage', lb).innerHTML = kind === 'video'
    ? `<video src="${src}" controls autoplay playsinline></video>`
    : `<img src="${src}" alt="">`;
  $('.lb-caption', lb).textContent = caption || '';
  $('#lbPath').textContent = path || '';
  $('#lbReveal').dataset.id = id || '';
  lb.hidden = false;
  $('.lb-close', lb).focus();
}
$('#lbReveal').onclick = async () => {
  try {
    await api('POST', `/api/jobs/${$('#lbReveal').dataset.id}/reveal`);
  } catch (err) {
    alert(err.message);
  }
};

/* ---------- 保存位置 ---------- */

function joinPath(dir, name) {
  const sep = dir.includes('\\') ? '\\' : '/';
  return dir.replace(/[\\/]+$/, '') + sep + name;
}

async function copyText(text, btn) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement('textarea');
    ta.value = text;
    document.body.append(ta);
    ta.select();
    document.execCommand('copy');
    ta.remove();
  }
  if (btn) {
    const old = btn.innerHTML;
    btn.textContent = '已复制';
    btn.disabled = true;
    setTimeout(() => { btn.innerHTML = old; btn.disabled = false; }, 1400);
  }
}

$('#openOutRoot').onclick = async () => {
  try {
    await api('POST', '/api/open-output');
  } catch (err) {
    alert(err.message);
  }
};
document.addEventListener('click', e => {
  const b = e.target.closest('[data-copy-target]');
  if (b) copyText($('#' + b.dataset.copyTarget).textContent, b);
});
function closeLightbox() {
  leave(lb, () => { $('.lb-stage', lb).innerHTML = ''; });
}
lb.addEventListener('click', e => {
  if (e.target === lb || e.target.closest('.lb-close')) closeLightbox();
});
document.addEventListener('keydown', e => {
  if (e.key === 'Escape' && !lb.hidden) closeLightbox();
  if (e.key === 'Escape' && !vd.hidden && !e.target.closest('.dd')) leave(vd);
});

/* ---------- 启动 ---------- */

setupThumbs();

(async () => {
  try {
    presets = await api('GET', '/api/presets');
    $$('.spec-box').forEach(buildSpecBox);
    $$('select').forEach(enhanceSelect);
    $$('input[data-combo]').forEach(enhanceCombo);
    await loadConfig();
    loadModels();
  } catch (err) {
    if (!offline) alert('读取配置失败：' + err.message);
  }
  if (!configReady()) showTab('settings');
  refreshBatches();
  setInterval(refreshBatches, 2000);
  refreshUpdate();
})();

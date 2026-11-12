/* ==========================================================================
   pages/gateway.js —— 网关页：八个面覆盖内嵌网关的全部管理路由。

   数据只有一个来源：/admin/api/state。写操作走同一套 admin 路由，所以这里
   的每个开关都和 API 使用者看到的配置是同一份，没有任何一份"界面自己的"
   状态被当成事实。

   八个面（服务 / 模型 / 聚合 / 供应商 / 联网 / Agent / 日志 / 设置）是同一份数据
   的八个视角，各自是左侧菜单里的一个独立入口：改一个代理模式，模型面和联网面
   都要跟着动，因为它们读的是同一份 state。聚合面是唯一的例外——它读
   aggregate/state，因为那条表随临期变化，节奏和主状态不一样。
   ========================================================================== */

import {
  Card, Group, Toggle, Combo, NumberBox, InfoBar, Button, PageHead,
  Dialog, h, icon, clamp, debounce
} from '../components/index.js';
import { providerBrand, BrandMark, brandOf, BRANDS } from '../brands.js';
import createOpenCode from './opencode.js';
import checkinPanel from './checkin.js';

/* mark 只喂给页面自己的时间线：冷启动慢在哪一段，得由页面量出来。 */
function mark(name) {
  try { performance.mark(name); } catch (err) { /* 老内核忽略 */ }
}

const M = {
  gateway: 'Gateway',
  pickSave: 'PickSavePath',
  pickOpen: 'PickOpenPath',
  openPath: 'OpenPath',
  openUrl: 'OpenUrl'
};

/* 面的顺序就是"先看什么"的顺序：服务 → 能力 → 来源 → 出口 → 接入 → 日志 → 配置。
   每一面在左侧菜单里是一个独立入口（shell.js 的 PAGES），页内不再有分面标签。 */
export const FACES = [
  { key: 'service', label: '服务' },
  { key: 'models', label: '模型' },
  /* 聚合排在模型之后、供应商之前：它讲的是"同一个模型从哪一家的额度里发出去"，
     读的人刚看完模型清单，接着就想知道"这些模型在几处有、先烧谁"。 */
  { key: 'aggregate', label: '聚合' },
  /* 渠道排在聚合之后：刚看完「同一个模型有几条退路」，下一个问题就是
     「那些退路的钥匙配齐了吗、缺的是哪一家」。 */
  { key: 'channels', label: '渠道' },
  { key: 'opencode', label: 'OpenCode' },
  { key: 'providers', label: '供应商' },
  { key: 'network', label: '联网' },
  { key: 'agents', label: 'Agent' },
  { key: 'logs', label: '日志' },
  { key: 'settings', label: '设置' }
];

/* 车道状态：中文与药丸色。颜色只表达状态，不抢行标题。 */
const STATE_LABEL = {
  available: '可用',
  unknown: '未探测',
  throttled: '暂时限流',
  'region-blocked': '地区受限',
  unavailable: '不可用',
  custom: '自定义'
};

const STATE_TONE = {
  available: 'ok',
  throttled: 'warn',
  'region-blocked': 'warn',
  unavailable: 'bad',
  unknown: 'mute',
  custom: 'mute'
};

const TASK_LABEL = { running: '运行中', done: '已完成', failed: '失败' };

/* 公告的 level 是"这条有多要紧"，不是数据状态；三个词各对应一条色带。 */
const LEVEL_LABEL = { info: '说明', warn: '注意', critical: '重要' };

/* 七个日志类别，与 logx 的 Cat* 常量一一对应 */
/* 七类日志各自说一件不同的事（谁在访问、上游、正文、路由、管理、探测、
   生命周期），列表里给它们各自的图标，比七张一样的"文件"易扫。 */
const CAT_ICON = {
  access: 'globe',
  upstream: 'cloud',
  content: 'file',
  routing: 'route',
  admin: 'shield',
  probe: 'radar',
  lifecycle: 'play'
};

const LOG_CATS = [
  { id: 'access', label: '访问' },
  { id: 'upstream', label: '上游' },
  { id: 'content', label: '内容' },
  { id: 'routing', label: '路由' },
  { id: 'admin', label: '管理' },
  { id: 'probe', label: '探测' },
  { id: 'lifecycle', label: '生命周期' }
];

const PROXY_MODES = [
  { label: '跟随环境变量', value: 'env' },
  { label: '直连', value: 'direct' },
  { label: '系统代理', value: 'system' },
  { label: '自定义代理', value: 'custom' },
  { label: '订阅轮换', value: 'rotate' }
];

const EFFORT_OPTIONS = [
  { label: '轻度', value: 'light' },
  { label: '均衡', value: 'balanced' },
  { label: '深度', value: 'deep' }
];

const ROUTING_OPTIONS = [
  { label: '目录顺序', value: 'catalog' },
  { label: '首字延迟优先', value: 'latency' }
];

const LOG_LEVEL_OPTIONS = [
  { label: 'debug（全量）', value: 'debug' },
  { label: 'info', value: 'info' },
  { label: 'warn', value: 'warn' },
  { label: 'error', value: 'error' }
];

const PROTOCOL_OPTIONS = [
  { label: 'OpenAI 兼容', value: 'openai' },
  { label: 'Anthropic 兼容', value: 'anthropic' }
];

/* ---------------------------------------------------------------- 小工具 */

function pick(o, k, d) {
  if (!o || typeof o !== 'object') return d;
  const v = o[k];
  return v === undefined || v === null ? d : v;
}

function asNumber(v, d) {
  const n = Number(v);
  return Number.isFinite(n) ? n : d;
}

function hrow() {
  const kids = [];
  for (let i = 0; i < arguments.length; i += 1) if (arguments[i]) kids.push(arguments[i]);
  return h('div', { class: 'row-list', style: 'flex-direction:row;align-items:center;gap:8px;flex-wrap:wrap' }, kids);
}

function pct(n) {
  const v = asNumber(n, 0);
  if (v >= 1000000) return (v / 1000000).toFixed(1) + 'M';
  if (v >= 1000) return Math.round(v / 1000) + 'K';
  return String(v);
}

function fmtWindow(n) {
  const v = asNumber(n, 0);
  if (!v) return '—';
  const dec = v % 1000 === 0;
  const k = dec ? v / 1000 : v / 1024;
  if (k < 1000) return Math.round(k) + 'K';
  return Math.round((dec ? v / 1e6 : v / 1048576) * 100) / 100 + 'M';
}

/* clock 把网关日志里的 ISO 时间缩成"月-日 时:分:秒"——日志表只有一格宽度,
   完整时间戳会把消息挤成两行。 */
function clock(iso) {
  const s = String(iso == null ? '' : iso);
  if (!s) return '—';
  const m = s.match(/(-|^)(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})/);
  if (m) return m[3] + '-' + m[4] + ' ' + m[5] + ':' + m[6] + ':' + m[7];
  const d = new Date(s);
  if (!isFinite(d.getTime())) return s.slice(0, 19);
  const p = (x) => (x < 10 ? '0' + x : String(x));
  return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' +
    p(d.getMinutes()) + ':' + p(d.getSeconds());
}

function stamp(ms) {
  const v = asNumber(ms, 0);
  if (!v) return '';
  const d = new Date(v);
  const p = (x) => (x < 10 ? '0' + x : String(x));
  return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

function tail(s, n) {
  const v = String(s == null ? '' : s);
  return v.length > n ? v.slice(0, n) + '…' : v;
}

/* cleanDetail turns the gateway's raw refusal text into one readable line.
   上游把 {"type":"error","error":{"type":"RegionError","message":"…"}} 整段塞进
   detail，直接铺在卡片副标题上会把整行撑爆；这里取 type 与 message，
   其余原文仍留在展开区。 */
function cleanDetail(raw) {
  const s = String(raw == null ? '' : raw).trim();
  if (!s) return '';
  const brace = s.indexOf('{');
  if (brace < 0) return s;
  try {
    const obj = JSON.parse(s.slice(brace));
    const inner = (obj && obj.error) || obj;
    const kind = String((inner && inner.type) || '');
    const msg = String((inner && inner.message) || '');
    if (kind || msg) {
      return [kind, msg].filter(Boolean).join('：');
    }
  } catch (err) {
    /* 不是 JSON：当成一句话，截短即可。 */
  }
  return s;
}

function stateTag(state) {
  const key = String(state || 'unknown');
  return h('span', { class: 'pill tone-' + (STATE_TONE[key] || 'mute'),
    text: STATE_LABEL[key] || key });
}

function toneTag(text, tone) {
  return h('span', { class: 'pill ' + (tone || 'tone-mute'), text: String(text == null ? '' : text) });
}

/* fieldCard：一整行铺开的"标题 + 说明 + 输入框"。
   卡片网格默认是 [图标 | 标题 | 控件] 三格，控件占右边一列 —— 那条规则适合
   开关和选值，不适合"要读/要贴一条 URL"的字段：右边留给输入框，左边就只剩
   两百来像素，一串地址会按 anywhere 折成竖条。这里换成块布局，输入框独占一行。
   见 controls.css 的 .gw-field-card。 */
function fieldCard(opts) {
  const o = opts || {};
  const card = h('div', { class: 'card gw-field-card' });
  const head = h('div', { class: 'gw-field-head' });
  if (o.icon) head.append(h('span', { class: 'card-icon' }, icon(o.icon)));
  const main = h('div', { class: 'card-main has-desc' });
  main.append(h('div', { class: 'card-title', text: o.title }));
  if (o.desc) main.append(h('div', { class: 'card-desc', text: o.desc }));
  head.append(main);
  card.append(head);
  const line = h('div', { class: 'gw-field-line' });
  line.append(textField({
    value: o.value, placeholder: o.placeholder, mono: true, onChange: o.onChange
  }));
  card.append(line);
  return card;
}

/* 单行文本输入。只在失焦或回车时提交，避免每敲一个字就打一次网关。 */
function textField(opts) {
  const o = opts || {};
  const initial = o.value == null ? '' : String(o.value);
  const input = h('input', {
    class: 'text-box',
    type: o.password ? 'password' : 'text',
    placeholder: o.placeholder || '',
    value: initial
  });
  if (o.width) input.style.minWidth = o.width + 'px';
  if (o.mono) input.style.fontFamily = 'var(--font-mono, monospace)';
  let last = initial;
  const commit = () => {
    if (input.value === last) return;
    last = input.value;
    if (o.onChange) o.onChange(input.value);
  };
  input.addEventListener('blur', commit);
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { commit(); input.blur(); }
  });
  return input;
}

/* ==========================================================================
   页面
   ========================================================================== */

export default function createPage(ctx, options) {
  const face = (options && options.face) || 'service';
  if (face === 'opencode') return createOpenCode(ctx);
  const faceMeta = FACES.find((f) => f.key === face) || FACES[0];

  const root = h('div', { class: 'page' });
  const head = PageHead({ title: faceMeta.label, actions: [] });
  const body = h('div', { class: 'page-body' });
  root.append(head, body);

  let state = null;
  let downReason = '';
  let alertBar = null;
  let showAllModels = false;
  let modelFilter = '';
  /* 已取消勾选的那一段默认折起：它下面可能压着二十多张卡。 */
  let modelsFold = false;
  let autoRefresh = null;

  /* 各面的临时编辑态：不动 state，写失败后界面就不会比后端更乐观。 */
  const draft = {
    provider: null,
    catalog: null,
    proxyTest: null,
    qoder: null,
    wb: null,
    wbTimer: null,
    logs: null,
    logQuery: { cats: ['access', 'lifecycle'], level: '', q: '', day: '' },
    /* 聚合面自己的一份表：它随临期变化，和主状态不是同一个刷新节奏。 */
    aggregate: null,
    aggregateError: '',
    aggOpen: {},
    aggQuery: '',
    aggPage: 0,
    aggRefreshing: false,
    /* 渠道面同样有自己的一份表：它含登录态与逐家额度，节奏比主状态快。 */
    channels: null,
    channelsError: '',
    checkins: null,
    checkinError: ''
  };

  function hideBar() {
    if (alertBar && alertBar.parentNode) alertBar.parentNode.removeChild(alertBar);
    alertBar = null;
  }

  /* 一条报错必须**看得见**：它挂在 .page-body 的最前面，而 .page-body 在
     .content 里是可滚动的那一筒——用户滚到第 20 张渠道卡再点「登录」时，
     信息条被 prepend 到 3000px 之上的文档开头（实测 scrollTop 2316，
     可见区间 40..780），于是「点了没反应」：条在 DOM 里、在屏幕外。
     钉住（.is-pinned）而不是把页面滚回去：报错不该顺手把用户正在看的
     那一屏挪走，而他可能正要再点一次同一张卡。 */
  function showBar(severity, title, message, action) {
    hideBar();
    alertBar = InfoBar({
      severity, title, message: message || '',
      action: action || null,
      onClose: () => { alertBar = null; }
    });
    alertBar.classList.add('is-pinned');
    body.prepend(alertBar);
    return alertBar;
  }

  const ok = (title, message) => showBar('success', title, message);
  const bad = (title, message) => showBar('error', title, message);

  /* ------------------------------------------------------------ 与宿主通信 */

  async function host(method, payload) {
    try {
      const res = await ctx.call(method, payload);
      if (!res || typeof res !== 'object') {
        return { ok: false, thrown: true, error: '后台没有返回内容' };
      }
      return res;
    } catch (err) {
      return { ok: false, thrown: true, error: String((err && err.message) || err) };
    }
  }

  /* 网关的唯一门。path 是 admin 路由名；body 为 undefined 时是 GET。
     extra.saveTo / extra.loadFrom 让导出类路由直接落盘、导入从磁盘读回来。 */
  async function gw(path, payload, extra) {
    const args = { path, body: payload === undefined ? null : payload };
    if (extra) {
      if (extra.saveTo) args.saveTo = extra.saveTo;
      if (extra.loadFrom) args.loadFrom = extra.loadFrom;
    }
    const res = await host(M.gateway, args);
    if (window.__gwTrace) {
      window.__gwTrace.push({
        path, ok: res && res.ok, code: res && res.code, error: res && res.error,
        keys: res && res.data && typeof res.data === 'object' && !Array.isArray(res.data)
          ? Object.keys(res.data).length : null
      });
    }
    if (!res || res.ok !== true) {
      return { ok: false, error: (res && res.error) || '网关没有响应', code: res && res.code };
    }
    return { ok: true, data: res.data };
  }

  /* 一次写操作：成功后重载 state，失败就把后端那句话原样报出来。 */
  async function write(path, payload, title) {
    const res = await gw(path, payload === undefined ? {} : payload);
    if (!res.ok) {
      bad(title || '保存失败', res.error);
      return false;
    }
    await load();
    render();
    return true;
  }

  /* settings 的二十多个开关只有一个写入口：POST settings，只带改动的那一项。 */
  async function setSetting(key, value, title) {
    const patch = {};
    patch[key] = value;
    return write('settings', patch, title || '设置未保存');
  }

  async function load() {
    const res = await gw('state');
    if (!res.ok) {
      downReason = res.error;
      state = null;
      return false;
    }
    state = res.data || {};
    downReason = '';
    return true;
  }

  /* ------------------------------------------------------------ 复用小件 */

  function copyText(text, okTitle) {
    try {
      const ta = document.createElement('textarea');
      ta.value = String(text == null ? '' : text);
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.append(ta);
      ta.select();
      document.execCommand('copy');
      ta.remove();
      ok(okTitle, '');
    } catch (err) {
      bad('复制失败', String(err));
    }
  }

  async function openPathAt(path) {
    const p = String(path || '');
    if (!p) return;
    const res = await host(M.openPath, { path: p });
    if (!res.ok) bad('无法打开', res.error || '');
  }

  async function openUrlAt(url) {
    const s = String(url || '');
    if (!s) return;
    const res = await host(M.openUrl, { url: s });
    if (!res.ok) bad('无法打开链接', res.error || '');
  }

  async function exportRoute(route, suggested, title) {
    const picked = await host(M.pickSave, { prompt: title, name: suggested });
    if (!picked.ok) { bad('无法选择保存位置', picked.error || ''); return; }
    const d = picked.data || {};
    if (d.canceled) return;
    const res = await gw(route, undefined, { saveTo: String(d.path || '') });
    if (!res.ok) { bad(title + '失败', res.error); return; }
    const out = res.data || {};
    ok(title + '完成', tail(out.path, 80) + '  ·  ' + asNumber(out.bytes, 0) + ' 字节');
  }

  async function importRoute(title) {
    const picked = await host(M.pickOpen, { prompt: title });
    if (!picked.ok) { bad('无法选择文件', picked.error || ''); return; }
    const d = picked.data || {};
    if (d.canceled) return;
    const p = String(d.path || '');
    const name = p.split(/[\\/]/).pop();
    const yes = await confirmDialog(title, '将用「' + name + '」覆盖网关当前配置。本机的端口与主 Key 会保留。');
    if (!yes) return;
    const res = await gw('import', undefined, { loadFrom: p });
    if (!res.ok) { bad(title + '失败', res.error); return; }
    ok(title + '完成', name);
    await load();
    render();
  }

  function confirmDialog(title, message, primaryLabel = '继续') {
    return new Promise((resolve) => {
      let settled = false;
      const finish = (v) => { if (!settled) { settled = true; resolve(v); } };
      const dlg = Dialog({
        title,
        content: h('div', { class: 'gw-hint', text: message }),
        primaryLabel,
        onPrimary: () => { finish(true); },
        secondaryLabel: '取消',
        onSecondary: () => { finish(false); }
      });
      dlg.node.addEventListener('pointerdown', (e) => { if (e.target === dlg.node) finish(false); });
      dlg.open();
      if (primaryLabel !== '继续') dlg.card.querySelector('button')?.focus({ preventScroll: true });
    });
  }


  /* ------------------------------------------------------- 面：渠道账号 */

  /* 这一面回答的是「29 家里哪几家真的能发请求」，以及「其余几家缺什么」。
     它与聚合面分工明确：聚合面讲「同一个模型有几条退路」（读 aggregate/state），
     这一面讲「每条退路的钥匙在哪」（读 channel/state）。两个数据源分开取，
     因为它们的节奏不同：额度一分钟内可复用，登录态要实时。 */

  async function loadChannels() {
    await loadCheckins();
    const res = await gw('channel/state');
    if (!res.ok) { draft.channelsError = res.error; draft.channels = null; return false; }
    draft.channelsError = '';
    draft.channels = res.data || {};
    return true;
  }

  async function loadCheckins() {
    const res = await gw('checkin/state');
    draft.checkinError = res.ok ? '' : res.error;
    if (res.ok) draft.checkins = res.data;
  }

  async function saveCheckins(patch) {
    if (!draft.checkins) return;
    const res = await gw('checkin/settings', { ...draft.checkins.settings, ...patch });
    if (!res.ok) bad('签到设置未保存', res.error);
    await loadCheckins(); render();
  }

  async function runCheckins() {
    const res = await gw('checkin/run', {});
    if (!res.ok) bad('签到未启动', res.error);
    await loadCheckins(); render();
  }

  /* 登录形态的一句话说明。四种形态要用户做的事完全不同，
     写成同一句「请登录」会让「贴 Key」那 12 家的入口看起来像坏了。 */
  const LOGIN_KIND_LABEL = {
    paste: '贴 Key',
    'qoder-device': '浏览器授权',
    device: '设备码',
    pkce: '浏览器授权',
    poll: '浏览器授权',
    /* cliflow 是官方 CLI 的两段式授权：动作与浏览器授权完全相同（开一个页面、
       回来等这里变绿），差别只在「令牌由本机铸、授权页由上游给」。 */
    cliflow: '浏览器授权',
    /* ticket 与 claw 都是华为系的两段式：动作同样是「开一个页面、回来等」，
       差别在票据由浏览器回本机 / 状态由云端登记。 */
    ticket: '浏览器授权',
    claw: '浏览器授权',
    qr: '扫码'
  };

  const TIER_LABEL = {
    expiry: '按真实到期时刻排序',
    never: '按永久额度参与',
    none: '按永久额度参与（上游无额度端点）'
  };

  /* 授权口仍走不通的几家：端点都在，卡在**本项目拿不到的那把密钥**上。
     这份名单是**事实记录**，不是配置：每家后面都附着实测那句回话，等它不再成立时
     把它从这里删掉即可，不需要改别处。

     ⚠️ 这里曾经还列着 codearts / cline / raccoon 三家，那三条都已在实现侧修好
     （ticket 形态 / 正确的 WorkOS 客户端 / 本机铸码），故移出名单 —— 留着会让
     用户以为它们坏了，而实际上现在点「登录」就能进。 */
  const UPSTREAM_BLOCK_NOTE = {
    /* loomy 的扫码已经**整条走通**（出码 → 扫码 → 用微信 code 换讯飞会话，
       签名用的是随客户端分发的公开账号密钥）。唯一还剩一步的是「这个微信
       还没绑过讯飞手机号」——那时后端会把「差一次短信绑定」如实报出来，
       不会静默转圈。这里只留那一句出路。 */
    loomy: '微信扫码已能完整换回会话；只有「这个微信还没绑讯飞手机号」时'
      + '才需要先去 Loomy 客户端绑一次手机号（绑定后回来扫码即可）。',
  };
  /* zcode 的授权口现在**通了**（init 要的是本机铸的 64 位 pollToken + provider:zai，
     两段式合同见 chprov 的 cliflow 实现）。但它仍是「抄一份也行」的渠道：
     授权页被墙、账号是别人的、设备标识没法自证时，从 ZCode 客户端里抄一份 jwt
     与 device_mid 比重新登录快。这一格是备用入口，不是坏掉的记录。 */
  const PASTE_FALLBACK_NOTE = {
    zcode: '备用：从 ZCode 客户端（%USERPROFILE%\\.zcode\\v2\\credentials.json）里抄 zcodejwttoken，'
      + '补上本机 deviceMid（telemetry-state.json 里的 deviceMid）。额度端点缺 x-device-mid 得 400 code 3001。'
  };
  const hasUpstreamBlock = (id) => Object.prototype.hasOwnProperty.call(UPSTREAM_BLOCK_NOTE, id);
  const hasPasteFallback = (id) => Object.prototype.hasOwnProperty.call(PASTE_FALLBACK_NOTE, id);
  /* 这五家没有登记 PasteLabel（它们本来不该走贴 Key），这里给一句能抄的东西名。 */
  const channelTokenLabel = (id) => ({
    zcode: 'ZCode token', codearts: '华为云 AK/SK', cline: 'Cline token',
    loomy: 'Loomy session（也可从客户端抄一份）', raccoon: '小浣熊 token'
  }[id] || 'token');

  /* 附加字段的人话名字与填写指引。用原始的 access_key_id / device_mid 当标签，
     用户得先去猜「这个要去哪抄」；而这一列的每一格都是必须去某个控制台找的东西，
     猜错一次就是一次 401，而 401 在面板上长得像「没额度」。 */
  const FIELD_LABEL = {
	 refresh_token: 'Refresh Token（可选）',
	 sso_session_id: '登录会话 ssoSessionId',
	 token: '账号会话 Token（可选）',
	 organization_code: '组织码（国际站必填）',
    access_key_id: 'Access Key ID（AK）',
    secret_access_key: 'Secret Access Key（SK）',
    security_token: 'STS 临时令牌（可选）',
    cookie: 'Cookie',
    session: 'Session 值',
    access_key: 'Access Key',
    secret_key: 'Secret Key',
    device_mid: 'device_mid（设备标识）',
    device_id: 'device_id（设备标识）',
    zai_access_token: 'zai access token',
    project: '项目 / 工程 ID',
    org: '组织标识',
    uid: '用户 ID',
    machine_id: '机器 ID',
    account_id: '账号 ID',
    product: '产品线'
  };
  const FIELD_HINT = {
	 refresh_token: '填写后可在 Access Token 到期时自动续期；只保存在本机。',
	 sso_session_id: 'AStudio 的登录会话，和模型 Bearer Token 分别填写；也可直接从本机插件导入。',
	 token: 'AStudio 的账号会话 Token；未提供时使用 ssoSessionId。',
	 organization_code: '从当前网页登录的组织读取；国际站先选中组织后再导入。',
    access_key_id: '华为云「我的凭证 → 访问密钥」里那串 AK。',
    secret_access_key: '与 AK 成对的 SK，只在这里贴一次。',
    security_token: '只有临时凭证才要；长期 AK/SK 留空即可。',
    cookie: '浏览器里这一家的整条 Cookie（含登录态字段）。',
    session: '客户端请求头里的 session 值。',
    access_key: '签名用的 Access Key。',
    secret_key: '与 access_key 成对。',
    device_mid: '这家客户端首次登录时铸的标识；换账号必须换它。',
    device_id: '设备标识；同一账号保持同一个值。',
    project: '云项目 / 工程 ID，额度端点按它查。',
    org: '组织标识（企业 / 学校等）。'
  };
  const fieldLabel = (k) => FIELD_LABEL[k] || k;
  const fieldHint = (k) => FIELD_HINT[k] || '';

  /* 凭据来源的一句话：排查「这个账号为什么不会自动续期」第一眼看的就是它。 */
  const SOURCE_LABEL = {
    paste: '手工贴入',
    import: '本机导入',
    device: '设备码授权',
    pkce: '浏览器授权',
    poll: '浏览器授权',
    cliflow: '浏览器授权',
    ticket: '浏览器授权',
    claw: '浏览器授权',
    qr: '扫码授权',
    sms: '短信授权',
    lane: '网关托管'
  };

  /* 这一家的结论：它现在发不发得出请求，以及为什么。
     五种原因要用户做的事完全不同（接入 / 再探测一次 / 换一条 Key / 重新登录 /
     什么都别做），压成一句「未登录」会让人跑去查登录状态，而问题在额度。 */
  function isNativeChannel(id) {
    return ['astudio', 'autoclaw', 'autoclawintl', 'magicrew', 'magicrewintl'].includes(id);
  }

  function chanStatus(c) {
    const q = pick(c, 'quota', {}) || {};
    /* 账号数取两者之中大的那个：workbuddy 的登录态不在凭据表里（在 lane），
       只读 accountCount 会把「已托管登录」说成未接入。qoder 现在是凭据表里的
       真实账号，两个数本来就相等 —— 取大值对它没有副作用。 */
    const n = Math.max(asNumber(pick(c, 'accountCount', 0), 0), asNumber(pick(c, 'accounts', 0), 0));
    if (!n) return { text: '未接入', tone: 'tone-mute' };
    /* 有凭据 ≠ 发得出请求：27 家只登记了额度端点与登录形态，本项目没有它们的
       推理实现。先说这一档，否则下面那句「还剩 N 天」会让用户以为这一家已经在
       为他干活了。 */
    if (!pick(c, 'routable', true)) return { text: '凭据可用 · 无推理接线', tone: 'tone-warn' };
    if (pick(q, 'failed', false)) return { text: '这次读不到额度', tone: 'tone-warn' };
    if (pick(q, 'known', false)) {
      const ms = asNumber(pick(q, 'expiryMs', 0), 0);
      const left = ms - Date.now();
      if (left <= 0) return { text: '额度已到期', tone: 'tone-bad' };
      return { text: remainText(ms, false), tone: remainTone(ms, false) };
    }
    if (pick(q, 'never', false)) {
      return { text: remainText(0, true, pick(c, 'id', '')), tone: 'tone-mute' };
    }
    return { text: '上游说这份额度不可用', tone: 'tone-warn' };
  }

  /* 一条账号的额度胶囊：口径与上面完全一致，只是喂进去的是一个只有一条账号的
     对象。两处各写一套判据，面板会在「这一家还剩 3 天」与「这个号还剩 20 天」
     之间自相矛盾，而两者都没有说谎。 */
  function chanQuotaTag(q, id) {
    /* 空对象 = 这一条**没读**（后端只在读到读数时才挂 quota）。它不能落到
       「上游说不可用」那一档：那是「上游明确拒绝」，与「还没探测」是两件事。 */
    const o = q || {};
    if (!Object.keys(o).length) return toneTag('未读额度', 'tone-mute');
    const st = chanStatus({ id, accountCount: 1, quota: o });
    return toneTag(st.text, st.tone);
  }

  /* 一家渠道一卡：卡头是这一家的结论（名字 / 档位 / 状态 / 入口），卡体是它的
     **账号清单**。多账号是这一面的核心事实 —— 额度按账号算，一行只说「3 个账号」
     等于把「哪个号先烧完、哪个号该退」这两件事藏起来，而它们正是用户来这一页
     要做的事。 */
  function channelRow(c) {
    const id = String(pick(c, 'id', ''));
    if (id === 'opencode') return Card({ brand: 'opencode', title: 'OpenCode',
      desc: '账号、匿名通道、独立代理和模型限流在专用页面管理。', width: 'full',
      right: Button({ label: '管理 OpenCode', onClick: () => ctx.navigate('gw:opencode') }) });
    const spec = pick(c, 'spec', {}) || {};
    const login = pick(c, 'login', {}) || {};
    const quota = pick(c, 'quota', {}) || {};
    const hint = pick(c, 'tokenHint', {}) || {};
    const have = !!pick(hint, 'present', false);
    const tier = String(pick(c, 'tier', ''));
    const kind = String(pick(login, 'kind', ''));
    const unit = String(pick(c, 'creditUnit', ''));
    const single = !!pick(c, 'single', false);
    const accts = Array.isArray(pick(c, 'accountList', [])) ? pick(c, 'accountList', []) : [];

    /* 「缺什么」只报**用户能填的**那几个。描述表的 ExtraKeys 里还混着 product /
       account_id 这类内部字段（由后端派生，用户无从填），把它们列进「缺」会让
       每一行都挂着一条无法消除的警告 —— 那种警告会训练人忽略整栏。
       qoder 的四格同理：cosy_key / encrypt_user_info 是登录成功后由网关现场铸的，
       machine_id / uid 是读回来的这台设备与账号标识。它们一个都不该由用户填。 */
    const INTERNAL = ['product', 'account_id', 'cosy_key', 'encrypt_user_info', 'machine_id', 'uid']
      .filter((k) => !(id === 'astudio' && k === 'account_id'));
    const extra = (Array.isArray(pick(spec, 'extraKeys', [])) ? pick(spec, 'extraKeys', [])
      : []).map(String).filter((k) => INTERNAL.indexOf(k) < 0);
    const got = Array.isArray(pick(hint, 'extraKeys', [])) ? pick(hint, 'extraKeys', []).map(String) : [];
    const required = new Set(pick(spec, 'requiredExtraKeys', extra));
    const missing = extra.filter((k) => required.has(k) && !got.some((g) => g === k));

    /* 「登没登录」读**同一个来源**：分组与这里都用 loggedIn（含 lane 账号与
       sidecar）。若这里改用「有没有手填凭据」，sidecar 已登录、凭据没导入的那家
       会出现在「已登录」组里却写着「未登录」——同一屏两处互相打脸。 */
    const loggedIn = !!pick(c, 'loggedIn', false) || have;
    const st = chanStatus(c);

    /* 入口文案分三种：贴 Key 形态、只能一个账号的单份形态、可多账号的授权形态。
       多账号那一条写成「加一个账号」而不是「登录」：同一家第二个号也是走这里，
       写「登录」会让人以为点一下就把前一个号换掉了。 */
    /* 用 chan-actions 而不是 agg-turn-move：后者那套 CSS 把按钮压到 26px 宽，
       那是给上下箭头用的，写在带文字的按钮上会把「重新登录」挤成一团。 */
    const actions = h('div', { class: 'chan-actions' });
    if (kind === 'paste') {
      actions.append(Button({
        label: accts.length ? '加一个 Key' : '贴 Key', variant: 'subtle',
        onClick: () => promptToken(id, String(pick(login, 'pasteLabel', id)),
          String(pick(login, 'pasteHint', '')), extra, String(pick(c, 'name', id)), accts.length)
      }));
    } else if (single) {
      actions.append(Button({
        label: loggedIn ? '重新登录' : '登录', variant: 'subtle',
        onClick: () => startChannelLogin(id, String(pick(c, 'name', id)), true)
      }));
    } else {
      actions.append(Button({
        label: accts.length ? '加一个账号' : '登录', variant: 'subtle',
        onClick: () => startChannelLogin(id, String(pick(c, 'name', id)), false)
      }));
      /* 授权口被上游挡住的几家再挂一个「贴 Key」：它们的登录形态本身没有坏，
         坏的是上游那个端点（客户端 id 被撤销 / 路由没了 / 缺必填参数），而
         凭据本身仍然能从官方客户端里抄到。只留一个必然失败的入口，等于把
         这一格做成死路。 */
      if (hasUpstreamBlock(id) || hasPasteFallback(id)) {
        const note = 'NOTICE:' + (hasUpstreamBlock(id) ? UPSTREAM_BLOCK_NOTE[id] : PASTE_FALLBACK_NOTE[id]);
        actions.append(Button({
          label: '贴 Key', variant: 'subtle',
          onClick: () => promptToken(id, channelTokenLabel(id),
            note, extra, String(pick(c, 'name', id)), accts.length)
        }));
      }
    }

    /* 卡头副标题只放「排查要看的那几样」：厂商 / 形态 / 缺的字段 / 额度端点原文。
       「已登录」不再写进副标题 —— 它已经由右上角的状态胶囊说了，同一句话说两遍
       会让副标题那一行读起来像没信息。 */
    const remainRaw = String(pick(quota, 'remaining', ''));
    const subParts = [
      String(pick(c, 'vendor', '')),
      LOGIN_KIND_LABEL[kind] || kind,
      single ? '单账号' : '',
      remainRaw ? '剩 ' + remainRaw : '',
      missing.length ? '缺 ' + missing.map(fieldLabel).join(' / ') : '',
      typeof c.catalogCount === 'number' && loggedIn ? c.catalogCount + ' 个已获取模型' : '',
      c.catalogError ? '模型目录：' + c.catalogError : ''
    ].filter(Boolean);

    const endpoint = [String(pick(spec, 'method', '')), String(pick(spec, 'baseUrl', '')),
      String(pick(spec, 'path', ''))].filter(Boolean).join(' ');

    const card = h('div', { class: 'chan-card' + (loggedIn ? '' : ' is-off') });

    const head = h('div', { class: 'chan-head' },
      chanMark(id, String(pick(c, 'name', id))),
      h('div', { class: 'chan-head-main' },
        h('div', { class: 'agg-turn-name' },
          h('span', { text: String(pick(c, 'name', id)) }),
          toneTag(isNativeChannel(id) && tier === 'never' ? '按余额参与 · 未提供精确到期' : (TIER_LABEL[tier] || tier), tier === 'expiry' ? 'tone-ok' : 'tone-mute'),
          unit && unit !== '无' ? toneTag(unit, 'tone-mute') : null,
          accts.length ? toneTag(accts.length + ' 个账号', 'tone-mute') : null),
        h('div', { class: 'agg-turn-sub', text: subParts.join('  ·  ') }),
        endpoint ? h('div', { class: 'agg-turn-sub mono chan-endpoint', text: endpoint }) : null),
      h('div', { class: 'chan-head-right' },
        toneTag(st.text, st.tone),
        /* 整家退出只在真的有好几个号时出现：一个号的家，「退出这个号」与
           「退出整家」是同一件事，摆两个按钮只会让人多点一次确认。 */
        accts.length > 1 || (!accts.length && loggedIn)
          ? Button({
            label: accts.length > 1 ? '退出全部' : '退出', variant: 'subtle',
            onClick: () => logoutChannel(id, String(pick(c, 'name', id)))
          })
          : null,
        actions));

    card.append(head);

    /* 账号清单：一个号一行，四件事各占一格（谁 / 怎么来的 / 这一份的额度 / 退出）。
       workbuddy 的账号不在凭据表里（由 lane 托管），那时清单为空，必须写一句话顶上
       —— 空白清单会被读成「这一家没有账号」，而它其实有，只是不由这一面管。
       qoder 已经不在这一档：它在渠道面自己登录，凭据就落在这张表里，清单里是真实的行。 */
    const list = h('div', { class: 'chan-accs' });
    /* 「待接入」那几十家里，一个号的都没有 —— 给每一家挂一行「还没有账号」会把
       这一面撑成一长串同义句，而那件事分组标题已经说过了。空清单只留给**
       托管登录**的那一家（workbuddy）：它看起来「没有账号」而其实是有的。 */
    if (!accts.length && loggedIn) {
      list.append(h('div', { class: 'chan-acc is-empty' },
        /* 这一行现在只在 workbuddy 上出现。qoder 的凭据已经进了凭据表（内置
           登录），所以它的清单里是真实的一行，不再走这条「托管」文案 —— 那条
           文案读起来像「这个号不归这里管」，而它其实归这里管。 */
        h('span', { class: 'chan-acc-name', text: '已连接本机转发服务' }),
        h('span', { class: 'chan-acc-meta', text: loggedIn
          ? '登录状态由本机服务提供；退出后停止在网关中使用。'
          : '同一家可以接多个账号，额度按账号分别计算。' })));
    }
    for (const [index, a] of accts.entries()) {
      const key = String(pick(a, 'key', ''));
      const accountID = String(pick(a, 'id', '')).trim();
      const nickname = String(pick(a, 'nickname', '')).trim();
      const label = String(pick(a, 'label', '')).trim();
      const accountName = nickname || (label && label !== accountID && !key.endsWith(':' + label)
        ? label : '账号 ' + (index + 1));
      const identity = accountID || key.replace(/^[^:]+:/, '');
      const shortIdentity = identity.length > 24 ? identity.slice(0, 10) + '…' + identity.slice(-8) : identity;
      const aq = pick(a, 'quota', {}) || {};
      const extraKeys = Array.isArray(pick(a, 'extraKeys', [])) ? pick(a, 'extraKeys', []).map(String) : [];
      /* 附加字段在凭据里只报了「哪些在」，没报值 —— 面板上回显 cookie / AK
         等于一次截图带走全部凭据。这里只说缺了哪几个。 */
      const amiss = extra.filter((k) => required.has(k) && !extraKeys.some((g) => g === k));
      const created = asNumber(pick(a, 'createdAtMs', 0), 0);
      /* 「缺」只在**这一条和别人不一样**时写在这里：整家都缺同一个字段时，
         卡底那句警告已经说过一次，逐行重复会把清单读成一排同样的错。 */
      const onlyThis = accts.length > 1 && amiss.length
        && accts.some((b) => b !== a
          && extra.some((k) => (Array.isArray(pick(b, 'extraKeys', [])) ? pick(b, 'extraKeys', []) : [])
            .map(String).indexOf(k) >= 0 && amiss.indexOf(k) >= 0));
      const meta = [
        SOURCE_LABEL[String(pick(a, 'source', ''))] || String(pick(a, 'source', '')),
        created ? '接入于 ' + stamp(created) : '',
        onlyThis ? '缺 ' + amiss.map(fieldLabel).join(' / ') : ''
      ].filter(Boolean).join('  ·  ');

      /* 圆点跟着这一份的额度走，不是一个恒绿的点：一个号已到期、另一个号还能
         撑二十天，正是这一列要一眼分开的事；全涂成绿色就把它们又合回去了。 */
      const ast = chanStatus({ id, accountCount: 1, quota: aq });
      const dotTone = ast.tone === 'tone-ok' ? 'is-ok'
        : ast.tone === 'tone-bad' ? 'is-bad'
          : ast.tone === 'tone-warn' ? 'is-warn' : 'is-mute';
      /* 额度胶囊与「退出」包成一组：它们是这一行的右侧，
         窄窗里要一起折到名字下面，而不是各自落到不同的格子里。
         分开摆时，退出按钮会掉到第一列那 8px 的圆点格里。 */
      const logout = Button({
        label: '退出', variant: 'subtle',
        onClick: () => logoutChannel(id, String(pick(c, 'name', id)), key, accountName)
      });
      logout.classList.add('chan-logout');
      logout.setAttribute('aria-label', '退出账号：' + accountName);
      const row = h('div', { class: 'chan-acc' },
        h('span', { class: 'chan-acc-dot ' + dotTone, 'aria-hidden': 'true' }),
        h('div', { class: 'chan-acc-main' },
          h('div', { class: 'chan-acc-name', text: accountName }),
          identity ? h('div', { class: 'chan-acc-id', text: (accountID ? '账号 ID：' : '凭据标识：') + shortIdentity,
            title: identity }) : null,
          h('div', { class: 'chan-acc-meta', text: meta })),
        h('div', { class: 'chan-acc-side' },
          chanQuotaTag(aq, id),
          logout));
      list.append(row);
    }
    /* 清单挂上的条件与上面填内容的条件一致：有账号，或这家由网关托管登录。
       两者都不是时卡片只剩卡头 —— 那正是「待接入」该有的样子。 */
    if (accts.length || loggedIn) card.append(list);

    /* 附加字段缺失要写出来：zcode 缺 device_mid、codearts 缺 AK/SK 时，额度端点
       必然 401，而 401 在读数上表现为「不可用」—— 光看胶囊会以为是「没额度」，
       实际是「凭据不全」。这一句只在有账号却缺字段时出现，不挂在每一行。 */
    if (missing.length && accts.length) {
      card.append(h('div', { class: 'chan-warn', text:
        (kind === 'paste' ? '贴 Key 时还要带上 ' : '这一家的凭据还要带上 ')
        + missing.map(fieldLabel).join(' / ')
        + '，否则额度端点会 401（读数上看起来像「没额度」）。' }));
    }
    return card;
  }

  /* 贴 Key 的对话框：token 一个框，附加字段按描述表的 ExtraKeys 逐个给一格。
     附加字段不做「可以留空」的写法：codearts 缺 AK/SK 时请求必然 401，
     而那个 401 与「额度用光」在面板上长得一样。 */
  function promptToken(id, label, hint, extraKeys, chanName, existing) {
    const box = h('div', { class: 'chan-form' });
    /* 这一家已经接了账号时，先把那句「再加一个」写在最上面：它是这个框存在的
       理由（额度按账号算），而用户看到「换 Key」会以为要点掉旧的。
       同一家的第二个 Key = 第二个账号，两个号一起参与轮换。 */
    /* 那句话只在**已经有号**时才写：零账号的家说「不会覆盖已有账号」，
       等于告诉用户这里本来有一条他不认识的凭据。 */
    if (chanName && existing > 0) {
      box.append(h('div', { class: 'chan-form-lead', text:
        '「' + chanName + '」已经有 ' + existing + ' 个账号：这里贴进来的是新增一份，'
        + '旧账号会继续保留，额度按账号分别显示。' }));
    } else if (hint && hint.indexOf('NOTICE:') >= 0) {
      /* 这一家的授权口被上游挡着（客户端 id 被撤销 / 端点没了 / 缺必填参数），
         面板仍要给出**能走通的那条路**：从官方客户端里抄一份现成的凭据。
         只报「发起登录失败」会让这一格变成死路，而它本来是可用的。 */
      box.append(h('div', { class: 'chan-form-lead', text: hint.replace('NOTICE:', '').trim() }));
    } else {
      /* 家名已经在标题上，这里不再重复：同一句名字写三遍，对话框第一屏就只剩
         一个名字，读不出「这一步要做什么」。 */
      box.append(h('div', { class: 'chan-form-lead', text:
        '这一家还没有账号。贴进来的是第一个号；之后再贴一份就是第二个号，'
        + '账号保存后可分别查看额度与状态。' }));
    }
    const tokenInput = h('input', {
      class: 'text-box', type: 'password', autocomplete: 'off', spellcheck: 'false',
      placeholder: '粘贴 ' + (label || 'token')
    });
    box.append(h('div', { class: 'chan-field' },
      h('span', { class: 'chan-field-label', text: label || 'token' }), tokenInput));
    const extras = {};
    for (const k of extraKeys) {
      /* 占位文字不要与下面那条提示重复：fieldHint 已经作为 .chan-field-hint 写在
         输入框下面（它是必须读懂的指令），再原样放进 placeholder 会让同一句话
         在框内外各出现一次，看起来像两件事。占位只放字段名本身。 */
      const inp = h('input', {
        class: 'text-box', type: ['sso_session_id', 'token', 'refresh_token'].includes(k) ? 'password' : 'text', autocomplete: 'off', spellcheck: 'false',
        placeholder: fieldLabel(k) || k
      });
      extras[k] = inp;
      /* 每一格都写出「这是什么、去哪抄」：这一列的每一样都是要跑到某个控制台
         去翻出来的东西，标签只有 access_key_id 时用户得先猜它是什么。 */
      box.append(h('div', { class: 'chan-field' },
        h('span', { class: 'chan-field-label', text: fieldLabel(k) }),
        inp,
        fieldHint(k) ? h('span', { class: 'chan-field-hint', text: fieldHint(k) }) : null));
    }
    /* 昵称：同一家的两个号如果都叫「账号 1 / 账号 2」，两台机器上是同一串，
       用户分不出该退哪一个。留一格让他自己命名，比事后猜要省事。 */
    const nickInput = h('input', {
      class: 'text-box', type: 'text', autocomplete: 'off',
      placeholder: '给这个账号起个名字（可留空）'
    });
    box.append(h('div', { class: 'chan-field' },
      h('span', { class: 'chan-field-label', text: '账号名字' }), nickInput,
      h('span', { class: 'chan-field-hint', text: '用来在清单里区分同一家的几个号，只在本地显示。' })));
    /* 那句 NOTICE 已经在顶部当正文读过一遍了，这里不再重复第二遍：
       同一句话在对话框里出现两次，读起来像两件不同的事。 */
    if (hint && hint.indexOf('NOTICE:') < 0) box.append(h('div', { class: 'chan-form-hint', text: hint }));
    let alive = true;
    const dlg = Dialog({
      /* 标题写「哪一家」，字段标签写「抄什么」：两者写同一句时，那个框里会出现
         两行一样的「Qoder token」，用户会以为有两处要填。 */
      title: chanName ? ('贴 Key · ' + chanName) : (label || '粘贴凭据'),
      content: box,
      secondaryLabel: '取消',
      primaryLabel: '保存并读额度',
      onPrimary: async () => {
        /* 空值不许提交：贴 Key 这一形态里，token 空着却带着附加字段（例如只填了
           cookie）也会被后端收下，存进去的是一份永远 401 的凭据 —— 面板上它算
           「已登录」，而它一次都发不出去。 */
        const token = tokenInput.value.trim();
        const falt = extraKeys.filter((k) => !extras[k].value.trim());
        if (!token && falt.length === extraKeys.length) {
          bad('还没填任何凭据', '至少要填「' + (label || 'token') + '」。');
          return false;
        }
        const body = { id, token };
        const nick = nickInput.value.trim();
        if (nick) body.nickname = nick;
        const ex = {};
        for (const k of Object.keys(extras)) {
          const v = extras[k].value.trim();
          if (v) ex[k] = v;
        }
        if (Object.keys(ex).length) body.extra = ex;
        const res = await gw('channel/token', body);
        if (!res.ok) { bad('凭据未保存', res.error); return false; }
        await loadChannels();
        render();
        const q = pick(res.data || {}, 'quota', {}) || {};
        const added = !!pick(res.data || {}, 'added', false);
        const n = asNumber(pick(res.data || {}, 'accountCount', 0), 0);
        if (pick(q, 'failed', false)) {
          bad('凭据已保存，但这次读不到额度', cleanDetail(String(pick(q, 'detail', ''))));
        } else {
          /* 「新增」与「更新」要分开说：用户若以为点了「加一个 Key」而实际覆盖
             了原来那条，他会以为旧账号还在轮换。 */
          ok(added ? '已新增一个账号' : '已更新这个账号的凭据',
            (n ? '这一家现在有 ' + n + ' 个账号  ·  ' : '')
            + (pick(q, 'known', false)
              ? '这一份 ' + remainText(asNumber(pick(q, 'expiryMs', 0), 0), false)
              : (pick(q, 'never', false) ? (isNativeChannel(id) ? '余额可用，上游未提供精确到期时间。' : '这一份按永久额度参与轮换。') : '额度读数未提供。')));
        }
        alive = false;
        return true;
      }
    });
    dlg.open();
    tokenInput.focus();
  }

  /* 开一次授权，然后按会话给的节奏轮询。轮询上限就是会话的过期时刻，
     不是「试到成功」—— 挂着一个永远不结束的定时器会让面板一直转圈。 */
  async function startChannelLogin(id, chanName, single) {
    const res = await gw('channel/login', { id });
    /* 失败那一句必须说清「是谁失败了、上游说了什么」：这一面有 29 家，
       「无法开始登录」放在整页顶上时，用户看不出是哪一家，也看不出是
       「这一家还没接」还是「上游这次拒了」——两者要做的事完全不同。 */
    if (!res.ok) {
      bad('「' + (chanName || id) + '」没能开始登录',
        cleanDetail(res.error) || '上游没有回话。');
      return;
    }
    const sess = pick(res.data || {}, 'session', {}) || {};
    const kind = String(pick(res.data || {}, 'kind', ''));
    const openUrl = String(pick(sess, 'openUrl', ''));
    const userCode = String(pick(sess, 'userCode', ''));
    const qr = String(pick(sess, 'qrImage', ''));
    const detail = String(pick(sess, 'detail', ''));
    if (!openUrl && !qr && !userCode) {
      bad('上游没有给出授权入口', detail || '描述表里只有贴 Key 这一条路径');
      return;
    }
    const box = h('div', { class: 'chan-form' });
    /* 第一步永远是「这一家要你做什么」。四种形态各自的动作完全不同：
       设备码要抄一段码、浏览器授权要开一个页、扫码要拿手机、短信要收验证码。
       只写「请登录」会让其中三种看起来像卡住了。 */
    const KIND_STEP = {
      device: '1. 点「打开授权页面」；2. 把下面的码填进去；3. 回来等这里变绿。',
      pkce: '1. 点「打开授权页面」；2. 在浏览器里登录并同意；3. 回来等这里变绿。',
      poll: '1. 点「打开授权页面」；2. 在浏览器里登录并同意；3. 回来等这里变绿。',
      cliflow: '1. 点「打开授权页面」；2. 在浏览器里登录并同意；3. 回来等这里变绿。',
      ticket: '1. 点「打开授权页面」；2. 在浏览器里登录；3. 回来后这里自动换回凭据，等它变绿。',
      claw: '1. 点「打开授权页面」；2. 在浏览器里登录并同意；3. 回来等这里变绿。',
      /* Qoder 的设备授权：授权页是它自己的登录页，没有码要抄，也不用手机扫码。
         它与 poll 的区别只在等待那一档（上游回 404 = 还在等），但用户看到的动作
         完全相同，所以这一句照写「打开→登录→回来等」。 */
      'qoder-device': '1. 点「打开授权页面」；2. 在浏览器里登录 Qoder 并确认；3. 回来等这里变绿。',
      qr: '1. 用这一家的手机客户端扫下面的码；2. 手机上确认；3. 回来等这里变绿。'
    };
    box.append(h('div', { class: 'chan-form-lead', text:
      KIND_STEP[kind] || '按下面的提示在浏览器里完成授权；成功后这一家的账号会自动出现在清单里。' }));
    if (single) {
      /* 「只有一份」的理由要说准：qoder 那一份是**转发层**只认一份凭据，不是
         「由别的程序托管」。写错了会让用户以为凭据不在网关里，从而去别处找。 */
      box.append(h('div', { class: 'chan-form-hint', text:
        '这一家的请求只走本机那一条转发路径，因此只保留一份登录态：重新登录会替换现有那份。' }));
    }
    if (qr) box.append(h('img', { class: 'chan-qr', src: qr, alt: '扫码登录' }));
    if (userCode) {
      box.append(h('div', { class: 'chan-field-label', text: '把这段码填进授权页面' }));
      box.append(h('div', { class: 'chan-code mono', text: userCode }));
    }
    if (openUrl) {
      box.append(h('div', { class: 'chan-field-label', text: '授权页面' }));
      box.append(h('div', { class: 'chan-url mono', text: openUrl }));
    }
    if (detail) box.append(h('div', { class: 'chan-form-hint', text: detail }));
    const statusLine = h('div', { class: 'chan-status-line', text:
      '正在等待上游确认…这一行会在成功后自己变成「已登录」。' });
    box.append(statusLine);
    let polling = true;
    const dlg = Dialog({
      title: '登录 · ' + (chanName || id),
      content: box,
      secondaryLabel: '取消',
      primaryLabel: openUrl ? '打开授权页面' : '我已确认',
      onPrimary: () => {
        if (openUrl) {
          /* ⚠️ 这里过去调 window.__zenOpenUrl，而这个全局**从未被注入**过
             （实测 typeof 为 "undefined"），于是每次都落到 window.open；
             WebView2 没挂 NewWindowRequested 处理器时那个调用不弹窗也不报错，
             用户看到的就是「点了，什么都没发生」。
             宿主有 OpenUrl 这一个真实通道（ShellExecute 交给默认浏览器），
             走它才真正打得开；打不开也由它如实报一句原因。 */
          openUrlAt(openUrl);
          return false; // 不关窗：用户还要回来等轮询
        }
        return false;
      },
      onSecondary: () => { polling = false; }
    });
    dlg.open();
    const deadline = asNumber(pick(sess, 'expiresAtMs', 0), 0) || (Date.now() + 600000);
    const interval = Math.max(1000, asNumber(pick(sess, 'intervalMs', 2000), 2000));
    const tick = async () => {
      /* Esc / overlay close does not fire onSecondary. Without this guard the
         poll kept running to the 10-minute deadline and could render into an
         unmounted page. */
      if (statusLine && !statusLine.isConnected) { polling = false; return; }
      if (!polling) return;
      if (Date.now() > deadline) {
        polling = false;
        statusLine.textContent = '登录已超时，关掉窗口重新开始。';
        return;
      }
      const res2 = await gw('channel/poll?id=' + encodeURIComponent(id));
      if (!polling) return;
      if (!res2.ok) {
        polling = false;
        statusLine.textContent = '登录失败：' + res2.error;
        return;
      }
      const st = String(pick(res2.data || {}, 'state', ''));
      if (st === 'wait' || st === 'pending' || st === '') {
        setTimeout(tick, interval);
        return;
      }
      if (st !== 'ok') {
        polling = false;
        statusLine.textContent = '上游说这一步没法继续（' + st + '），关掉窗口重开一次。';
        return;
      }
      polling = false;
      dlg.close();
      await loadChannels();
      render();
      ok('登录成功', (chanName || id) + ' 的账号已保存，可在账号清单中查看状态。');
    };
    setTimeout(tick, interval);
  }

  /* 退一个账号，或退整家（key 为空）。
     不带 key 的退出会把这一家的全部账号一起清掉，而用户点的可能只是清单里
     某一行 —— 那两者的后果差好几个号，所以两个入口的文案必须不同。 */
  async function logoutChannel(id, name, key, accLabel) {
    const all = !key;
    const msg = all
      ? '将清除「' + name + '」的全部凭据，网关将停止使用这些账号。'
      : '将退出「' + name + '」里的账号「' + accLabel + '」，其余账号不受影响。';
    const yes = await confirmDialog(all ? '退出全部账号' : '退出这个账号', msg, all ? '退出全部' : '退出账号');
    if (!yes) return;
    const res = await gw('channel/logout', all ? { id } : { id, key });
    if (!res.ok) { bad('退出失败', res.error); return; }
    await loadChannels();
    render();
    const left = asNumber(pick(res.data || {}, 'remaining', 0), 0);
    ok(all ? '已退出整家' : '已退出这个账号',
      all ? name : (left ? '这一家还剩 ' + left + ' 个账号。' : '这一家已经没有账号了。'));
  }

  /* 渠道账号面：29 家的入口。
     它和聚合面是**两个数据源**：聚合面读 aggregate/state（讲「这个模型有几条退路」），
     这一面读 channel/state（讲「每条退路的钥匙在哪」）。分开取是因为节奏不同：
     额度一分钟内可复用，登录态必须实时。 */
  /* 从本机插件的凭据库导入。结果要逐栏念出来：只报「导入 N 家」会把一次
     部分成功说成成功，而用户会以为「其余那几家也已经好了」。 */
  async function importChannels() {
    const res = await gw('channel/import', {});
    if (!res.ok) { bad('导入失败', res.error); return; }
    const out = res.data || {};
    const rr = pick(out, 'result', {}) || {};
    const got = asNumber(pick(out, 'imported', 0), 0);
    const accounts = asNumber(pick(out, 'accounts', 0), 0);
    const read = asNumber(pick(rr, 'read', 0), 0);
    const skipped = pick(rr, 'skipped', {}) || {};
    const names = Object.keys(skipped).filter((k) => {
      /* 「同一渠道的其他账号」那一类不是失败：那一条本来就该被合并。
         把它们混进跳过列表会让一次完整的导入看起来像丢了东西。 */
      return String(skipped[k]).indexOf('合并') < 0 && String(skipped[k]).indexOf('覆盖') < 0;
    });
    await loadChannels();
    render();
    if (!got) {
      bad('没有可导入的凭据', names.length
        ? names.slice(0, 4).map((k) => k + '（' + String(skipped[k]) + '）').join('；')
        : (String(pick(rr, 'note', '')) || '本机没有参考插件的凭据库。'));
      return;
    }
    /* 家数与账号数分开念：同一家的第二个号也进来了，只说「导入 3 家」会让
       用户以为那 5 个号里只进来了 3 个。 */
    ok('导入 ' + got + ' 家 · ' + accounts + ' 个账号', names.length
      ? '没搬过来 ' + names.length + ' 项：' + names.slice(0, 4).map(
        (k) => k + '（' + String(skipped[k]) + '）').join('；')
        + (names.length > 4 ? '；其余 ' + (names.length - 4) + ' 项同理' : '')
      : (read ? '读到 ' + read + ' 条，' : '')
        + '同一家的多个账号都按账号分开导入。');
  }

  function buildChannelFace() {
    const data = draft.channels;
    if (!data) {
      return h('div', { class: 'page-body', style: 'gap:32px' },
        Group({
          title: '渠道账号',
          desc: '29 家渠道的登录入口与额度读数。同一家在这里登录之后，聚合面就会把它排进轮换。'
        }).node,
        Card({
          icon: 'key',
          width: 'full',
          title: draft.channelsError ? '读不到渠道状态' : '正在读取渠道状态',
          desc: draft.channelsError
            ? draft.channelsError + '  ·  这一面要等网关把 channel/state 路由挂上。'
            : '逐家读一次额度，读完就有了。',
          right: Button({ label: '重试', variant: 'standard', onClick: async () => { await loadChannels(); render(); } })
        }));
    }
    const list = (Array.isArray(pick(data, 'channels', [])) ? pick(data, 'channels', []) : [])
      .slice().sort((a, b) => {
        /* 已登录的排前面，组内按「谁更临期」排：这一面第一眼要回答的是
           「哪一个号马上要烧完」，而字母序把它埋在三十行之后。 */
        const av = pick(pick(a, 'quota', {}) || {}, 'expiryMs', 0);
        const bv = pick(pick(b, 'quota', {}) || {}, 'expiryMs', 0);
        const aon = pick(a, 'loggedIn', false) ? 0 : 1;
        const bon = pick(b, 'loggedIn', false) ? 0 : 1;
        if (aon !== bon) return aon - bon;
        if (av && bv && av !== bv) return av - bv;
        if (av && !bv) return -1;
        if (!av && bv) return 1;
        return String(pick(a, 'id', '')).localeCompare(String(pick(b, 'id', '')));
      });
    const totals = pick(data, 'totals', {}) || {};
    const pages = [];

    const top = Group({
      title: '渠道账号',
      desc: '凭据只从你自己的动作来：贴 Key 或走这一家的授权流程。本项目不读任何客户端缓存。'
    });
    const counts = [
      asNumber(pick(totals, 'channels', 0), 0) + ' 家渠道',
      /* 「凭据账号」而不是「账号」：workbuddy 的号由 lane 托管，不在凭据表里。
         写成「账号」时，页头会说「0 个账号」而同屏又写「1 家已登录」，
         读的人会先怀疑数据，而它只是两个不同的计数口径。 */
      asNumber(pick(totals, 'accounts', 0), 0) + ' 个已存凭据',
      asNumber(pick(totals, 'loggedIn', 0), 0) + ' 家已登录',
      asNumber(pick(totals, 'withQuota', 0), 0) + ' 家读到额度',
      asNumber(pick(totals, 'expiringSoon', 0), 0) + ' 家三天内作废'
    ];
    /* 供应商数与渠道数分开报：两个数是两件事，合成一句会让"我接了 4 家、
       这里只写 3"读起来像丢了一家。 */
    const provRows = Array.isArray(pick(data, 'providers', [])) ? pick(data, 'providers', []) : [];
    if (provRows.length) {
      counts.push(provRows.length + ' 家供应商（' + asNumber(pick(totals, 'shared', 0), 0) + ' 家有同名渠道）');
    }
    const sumRow = h('div', { class: 'agg-sum' });
    counts.forEach((t) => sumRow.append(h('span', { class: 'agg-sum-item', text: t })));
    top.body.append(sumRow);
    /* 本机已经装过参考插件、在那边登录过好几家时，逐个重扫一次码是白费：
       凭据就在盘上，而用户已经授权过了。这个按钮就是「把那些搬过来」。
       它只补齐空缺（已有凭据不覆盖），并如实报哪几家没搬、为什么。 */
    top.body.append(Card({
      icon: 'key',
      width: 'full',
      title: '从本机插件导入已登录的渠道',
      desc: '凭据只读一次、不回写那些文件；本机已有凭据的渠道保留现有的。',
      right: Button({ label: '导入', variant: 'subtle', onClick: importChannels })
    }));

    /* 供应商 vs 渠道：这一面是「谁有登录入口」，供应商面是「谁有自己的端点」。
       两套数据只在 id 同名时才重叠，所以「我接了 4 家供应商、这里只看到 3 家」
       是**正常的**，而它看起来像漏显示。不写这一格，用户只会去反复点刷新。 */
    if (provRows.length) {
      const solo = provRows.filter((x) => !pick(x, 'channel', false));
      /* 用这一面自己的 chan-card 结构，不用 Card()：Card 是四列 grid
         （图标 / 标题 / 内容 / 尾部），把清单直接 append 进去只会占掉第一列，
         整块被挤成一根竖条。chan-card 本来就是「卡头 + 卡体」两段纵向的。 */
      const card = h('div', { class: 'chan-card' });
      card.append(h('div', { class: 'chan-head' },
        h('span', { class: 'card-brand', 'aria-hidden': 'true' }, icon('profile')),
        h('div', { class: 'chan-head-main' },
          h('div', { class: 'agg-turn-name' },
            h('span', { text: '已接入的供应商与本面的关系' }),
            toneTag(provRows.length + ' 家供应商', 'tone-mute'),
            toneTag(solo.length ? solo.length + ' 家仅直连' : '全部同名', solo.length ? 'tone-warn' : 'tone-ok')),
          h('div', { class: 'agg-turn-sub', text
            : '供应商是你自己填的端点（直连、只要 Key），渠道是那张 30 家的清单（要登录、有额度、参与轮换）。'
              + '两者只在 id 同名时重叠 —— 所以「接了 4 家、这里只看到 3 家」不是漏显示。' })),
        h('div', { class: 'chan-head-right' },
          Button({
            label: '去供应商面', variant: 'subtle',
            onClick: () => { if (ctx.navigate) ctx.navigate('gw:providers'); }
          }))));
      const rows = h('div', { class: 'chan-accs' });
      for (const x of provRows) {
        const isCh = !!pick(x, 'channel', false);
        rows.append(h('div', { class: 'chan-acc' },
          h('span', { class: 'chan-acc-dot ' + (isCh ? 'is-ok' : 'is-mute'), 'aria-hidden': 'true' }),
          h('div', { class: 'chan-acc-main' },
            h('div', { class: 'chan-acc-name', text: String(pick(x, 'name', '')) || String(pick(x, 'id', '')) }),
            h('div', { class: 'chan-acc-meta', text: String(pick(x, 'id', ''))
              + '  ·  ' + asNumber(pick(x, 'models', 0), 0) + ' 个模型'
              + '  ·  ' + (pick(x, 'hasKey', false) ? '已填 Key' : '未填 Key') })),
          h('div', { class: 'chan-acc-side' },
            toneTag(isCh ? '同名渠道：' + String(pick(x, 'channelName', '')) : '仅直连，不在渠道表里',
              isCh ? 'tone-ok' : 'tone-mute'))));
      }
      card.append(rows);
      top.body.append(card);
    }
    pages.push(top.node);

    pages.push(checkinPanel(draft.checkins, draft.checkinError, saveCheckins, runCheckins,
      async () => { await loadCheckins(); render(); }));

    /* 只分两组：**按「这一家现在发不发得出请求」切**，不按档位再切一组。
       档位是排序解释（每行右侧的胶囊已经写了），照它切组会让同一家在两组里
       各出现一次 —— 用户数不出「到底有几家没配」。 */
    const groups = [
      {
        key: 'ready', title: '已接入',
        desc: '账号已保存，可在这里查看登录与额度状态。可用的模型与备用顺序请在「聚合」页查看。'
      },
      {
        key: 'todo', title: '待接入',
        desc: '还没有凭据。点卡片右上角的按钮，按这一家的形态接入（贴 Key / 设备码 / 浏览器授权 / 扫码）。'
      }
    ];
    const isReady = (c) => {
      const have = !!pick(pick(c, 'tokenHint', {}) || {}, 'present', false);
      return !!pick(c, 'loggedIn', false) || have;
    };
    for (const g of groups) {
      const rows = list.filter((c) => (g.key === 'ready' ? isReady(c) : !isReady(c)));
      if (!rows.length) continue;
      /* 账号数只数**凭据表里的条数**，与顶部汇总条用的是同一个数。workbuddy 的
         账号不在表里，若在这里补上一份，页头说 6 个、分组说 7 个，读的人会先
         怀疑整页的数据。它在卡片的清单里自己有一句「账号由 lane 托管」。 */
      const accounts = rows.reduce((n, c) => n + asNumber(pick(c, 'accountCount', 0), 0), 0);
      const box = Group({
        title: g.title + '（' + rows.length + ' 家'
          + (g.key === 'ready' && accounts ? ' · ' + accounts + ' 个账号' : '') + '）',
        desc: g.desc
      });
      for (const c of rows) box.body.append(channelRow(c));
      pages.push(box.node);
    }

    return h('div', { class: 'page-body', style: 'gap:32px' }, pages);
  }
  /* ------------------------------------------------------------ 面：服务 */

  function buildServiceFace() {
    const base = String(pick(state, 'baseURL', ''));
    const lan = String(pick(state, 'lanBaseURL', ''));
    const port = asNumber(pick(state, 'port', 0), 0);
    const key = String(pick(state, 'mainKey', ''));
    const settings = pick(state, 'settings', {}) || {};
    const ver = String(pick(state, 'version', ''));
    const uptime = String(pick(state, 'uptime', ''));
    const startedAt = String(pick(state, 'startedAt', ''));
    const dataDir = String(pick(state, 'dataDir', ''));
    const egress = pick(state, 'egress', {}) || {};

    const group = Group({ title: '运行配置', desc: '这一页改的任何一项都直接写进网关自己的 config.json。' });

    group.body.append(Card({
      icon: 'server',
      title: 'Base URL',
      desc: base || '网关没有启动',
      right: base ? Button({ label: '复制', variant: 'subtle', onClick: () => copyText(base, 'Base URL 已复制') }) : null
    }));

    if (lan) {
      group.body.append(Card({
        icon: 'globe',
        title: '局域网 Base URL',
        desc: lan + '  ·  任何设备带着 Key 都能访问',
        right: Button({ label: '复制', variant: 'subtle', onClick: () => copyText(lan, '局域网地址已复制') })
      }));
    }

    group.body.append(Card({
      icon: 'key',
      title: '主 Key',
      desc: key ? (key.length > 14 ? key.slice(0, 10) + '…' + key.slice(-4) : key) : '（尚未生成）',
      right: hrow(
        key ? Button({ label: '复制', variant: 'subtle', onClick: () => copyText(key, 'Key 已复制') }) : null,
        Button({ label: '轮换', variant: 'standard', onClick: rotateKey })
      )
    }));

    group.body.append(Card({
      icon: 'channel',
      title: '监听端口',
      desc: '改成新端口后网关立刻重建监听器，已连着的客户端需要重连。',
      right: NumberBox({
        value: port, min: 1, max: 65535, step: 1,
        onChange: (v) => setSetting('port', Math.round(v), '端口未保存')
      })
    }));

    group.body.append(Card({
      icon: 'cloud',
      title: '允许局域网访问',
      desc: pick(settings, 'allowLan', false)
        ? '当前监听所有网卡。API 仍要 Key，管理面仍然只对本机开放。'
        : '只监听 127.0.0.1。开启后同一局域网的其他设备可以直接用 API。',
      right: Toggle({
        checked: !!pick(settings, 'allowLan', false),
        onChange: (v) => setSetting('allowLan', v, '局域网监听未保存')
      })
    }));

    group.body.append(Card({
      icon: 'play',
      title: '运行状态',
      desc: '版本 ' + (ver || '—') + '  ·  已运行 ' + (uptime || '—')
        + (startedAt ? '  ·  启动于 ' + startedAt : '')
        + (egress.ip ? '  ·  出口 ' + egress.ip + (egress.country ? '（' + egress.country + '）' : '') : ''),
      right: dataDir ? Button({ label: '数据目录', variant: 'subtle', onClick: () => openPathAt(dataDir) }) : null
    }));

    group.body.append(Card({
      icon: 'restart',
      title: '开机自启',
      desc: '写注册表 Run 项，和网关自己的开关是同一处。',
      right: Toggle({
        checked: !!pick(settings, 'autostart', false),
        onChange: (v) => autostart(v)
      })
    }));

    const has = !!pick(settings, 'updateAvailable', false);
    const uver = String(pick(settings, 'updateVersion', ''));
    const selfUpdate = !!pick(settings, 'updateSelfUpdate', false);
    group.body.append(Card({
      icon: 'download',
      title: has ? '有新版本 ' + uver : '检查更新',
      desc: has
        ? (selfUpdate ? '可以直接下载替换并重启。' : '当前是捆绑版，只能到发布页手动下载。')
        : '当前版本 ' + (ver || '—') + '，点右侧手动检查一次。',
      right: hrow(
        Button({ label: '检查', variant: 'subtle', onClick: checkUpdate }),
        has && selfUpdate ? Button({ label: '一键更新', variant: 'accent', onClick: applyUpdate }) : null,
        has && !selfUpdate && pick(settings, 'updateURL', '')
          ? Button({ label: '发布页', variant: 'subtle', onClick: () => openUrlAt(pick(settings, 'updateURL', '')) })
          : null
      )
    }));

    return group.node;
  }

  function buildEndpointsGroup() {
    const base = String(pick(state, 'baseURL', '')) || 'http://127.0.0.1:端口/v1';
    const group = Group({ title: '兼容端点', desc: '任何支持 OpenAI / Anthropic 兼容方式的客户端都能接。' });
    const rows = [
      ['OpenAI Chat', base + '/chat/completions'],
      ['OpenAI Responses', base + '/responses'],
      ['Anthropic Messages', base + '/messages'],
      ['模型列表', base + '/models']
    ];
    /* 端点是一张"地址表"，不是四张卡片：地址必须一行读完（半宽卡片里
       /chat/completions 会在 complet|ions 之间断行，读的人得自己拼回去），
       而四个条目除了地址之外没有别的话要说，各占一张卡只留下大块空白。
       所以整组是一块面板，一行一条：名字 | 等宽地址 | 复制。 */
    const list = h('div', { class: 'ep-list' });
    for (const row of rows) {
      list.append(h('div', { class: 'ep-row' },
        h('span', { class: 'ep-name', text: row[0] }),
        h('code', { class: 'ep-url mono', text: row[1] }),
        Button({ label: '复制', variant: 'subtle', onClick: () => copyText(row[1], row[0] + ' 已复制') })
      ));
    }
    group.body.append(list);
    return group.node;
  }

  /* 一条公告。见 controls.css 的 .ann-item：色带 = 要紧程度，标题一行，
     正文一列（长文收三行，可展开），日期与动作落到最后一行。 */
  function announcementItem(a) {
    const id = String(pick(a, 'id', ''));
    const linked = String(pick(a, 'url', ''));
    const isUnread = !!pick(a, 'unread', false);
    const level = String(pick(a, 'level', 'info'));
    const bodyText = String(pick(a, 'body', ''));
    const start = String(pick(a, 'start', ''));
    const end = String(pick(a, 'end', ''));

    const titleRow = h('div', { class: 'ann-head' });
    if (isUnread) titleRow.append(h('span', { class: 'ann-dot', 'aria-hidden': 'true' }));
    titleRow.append(h('span', { class: 'ann-title', text: String(pick(a, 'title', id)) }), toneTag(
      LEVEL_LABEL[level] || LEVEL_LABEL.info,
      level === 'critical' ? 'tone-bad' : (level === 'warn' ? 'tone-warn' : 'tone-mute')
    ));

    const item = h('div', {
      class: 'ann-item lv-' + (LEVEL_LABEL[level] ? level : 'info')
        + (isUnread ? ' is-unread' : ' is-read')
    }, titleRow);

    let bodyEl = null;
    if (bodyText) {
      /* 长正文收三行再展开：列表里这一段的作用是「要不要点详情」，不是「现在读完」。
         三百字的版本说明全铺开会把后两条公告推到两屏之外。 */
      const long = bodyText.length > 150;
      bodyEl = h('div', { class: 'ann-body' + (long ? ' is-clamped' : ''), text: bodyText });
      item.append(bodyEl);
    }

    const foot = h('div', { class: 'ann-foot' });
    const span = start && end ? start + ' → ' + end : (start || end ? (start || '—') + ' → ' + (end || '长期') : '');
    if (span) foot.append(h('span', { class: 'ann-date', text: span }));
    foot.append(h('span', { class: 'ann-gap' }));

    let moreBtn = null;
    if (bodyEl && bodyEl.classList.contains('is-clamped')) {
      moreBtn = Button({
        label: '展开全文', variant: 'subtle',
        onClick: () => {
          const open = bodyEl.classList.toggle('is-clamped') === false;
          moreBtn.querySelector('span').textContent = open ? '收起' : '展开全文';
        }
      });
      foot.append(moreBtn);
    }
    if (linked) {
      foot.append(Button({ label: '详情', variant: 'subtle', onClick: () => openUrlAt(linked) }));
    }
    if (isUnread) {
      foot.append(Button({ label: '标记已读', variant: 'subtle', onClick: () => markRead(id) }));
    }
    item.append(foot);
    return item;
  }

  function buildAnnounceGroup() {
    const active = Array.isArray(pick(state, 'announcements', [])) ? pick(state, 'announcements', []) : [];
    const archive = Array.isArray(pick(state, 'announcementArchive', []))
      ? pick(state, 'announcementArchive', []) : [];
    const unread = active.filter((a) => pick(a, 'unread', false)).length;

    /* 这一组没有 Group 头：公告自己的那条横杆就是头。标题和两个数（未读 /
       有效期内）在左边，立刻重拉在右边，下面直接接清单 —— 原来是一行
       「公告」小标题 + 一行说明 + 一张「拉取公告」卡片，三层才走到第一条公告，
       而公告本身那点信息在一屏里被压在下面。 */
    const group = h('section', { class: 'group' });
    const bar = h('div', { class: 'ann-bar' },
      h('span', { class: 'ann-bar-title', text: '公告' }),
      active.length
        ? (unread ? toneTag(unread + ' 条未读', 'tone-warn') : toneTag('全部已读', 'tone-mute'))
        : toneTag('没有有效期内的公告', 'tone-mute'),
      active.length ? toneTag(active.length + ' 条在有效期内', 'tone-mute') : null,
      h('span', { class: 'ann-gap' }),
      Button({ label: '拉取公告', variant: 'subtle', onClick: refreshAnnouncements })
    );
    group.append(bar);
    const groupBody = h('div', { class: 'group-body' });
    group.append(groupBody);

    /* 一条公告是一段要读完的话，不是一行开关。它单独铺成一列（见 controls.css 的
       .ann-list / .ann-item）：色带说明要紧程度，标题一行，正文按字宽收成一列，
       日期和动作落到最下面一行。原来它是一张卡片、正文塞在 desc 里，半格里
       200px 宽的说明列会把一版更新日志折成二十行竖排字。 */
    const list = h('div', { class: 'ann-list' });
    for (const a of active) list.append(announcementItem(a));
    if (active.length) {
      groupBody.append(list);
    } else {
      groupBody.append(Card({
        icon: 'info',
        width: 'full',
        title: '现在没有要读的公告',
        desc: '维护通知与版本说明会在发布后出现在这里。'
      }));
    }

    if (archive.length) {
      groupBody.append(Card({
        icon: 'more',
        width: 'full',
        title: '已结束的公告 ' + archive.length + ' 条',
        expandable: true,
        content: (() => {
          const box = h('div', { class: 'row-list' });
          for (const a of archive) {
            box.append(h('div', { class: 'gw-form-row' },
              h('div', { class: 'gw-label', text: String(pick(a, 'title', '')) }),
              h('div', { class: 'gw-hint', text: String(pick(a, 'body', '')) + '  ·  ' + String(pick(a, 'end', '')) })
            ));
          }
          return box;
        })()
      }));
    }

    return group;
  }

  function buildMigrationGroup() {
    const group = Group({ title: '配置迁移', desc: '导出的就是网关自己的 config.json；导入会用文件内容整体替换。' });
    group.body.append(Card({
      icon: 'download',
      title: '导出配置（密钥打码）',
      desc: '贴问题报告用这一份：主 Key、Agent Key、供应商 Key 都换成占位符。',
      right: Button({ label: '导出', variant: 'standard', onClick: () => exportRoute('export', 'lamp-guide-config.json', '导出配置') })
    }));
    group.body.append(Card({
      icon: 'download',
      title: '导出配置（含真实密钥）',
      desc: '换机器迁移用这一份：文件里有可用的 Key。',
      right: Button({ label: '导出', variant: 'standard', onClick: () => exportRoute('export?secrets=1', 'lamp-guide-config-secrets.json', '导出完整配置') })
    }));
    group.body.append(Card({
      icon: 'folder',
      title: '导入配置',
      desc: '打码过的文件也能导入：本机的 Key 与端口会保留，占位符不会被当成凭据。',
      right: Button({ label: '选择文件', variant: 'standard', onClick: () => importRoute('导入配置') })
    }));
    group.body.append(Card({
      icon: 'scale',
      title: '导出用量 CSV',
      desc: '按日统计：requests / failed / input / output，外加每个模型一列。',
      right: Button({ label: '导出', variant: 'subtle', onClick: () => exportRoute('usage.csv', 'lamp-guide-usage.csv', '导出用量') })
    }));
    return group.node;
  }

  async function rotateKey() {
    const res = await gw('key/rotate', {});
    if (!res.ok) { bad('轮换失败', res.error); return; }
    await load();
    render();
    const key = String(pick(res.data || {}, 'key', ''));
    ok('已轮换 Key', '所有客户端都要换成新 Key' + (key ? '：' + tail(key, 14) : '') + '  ·  已开启的 Agent 配置会重新注入');
  }

  async function autostart(on) {
    const res = await gw('autostart', { enabled: !!on });
    if (!res.ok) { bad('开机自启未保存', res.error); await load(); render(); return; }
    await load();
    render();
    ok(on ? '已写入开机自启' : '已移除开机自启', '');
  }

  async function checkUpdate() {
    const res = await gw('update/check', {});
    if (!res.ok) { bad('检查更新失败', res.error); return; }
    const d = res.data || {};
    await load();
    render();
    if (pick(d, 'has', false)) ok('发现新版本 ' + String(pick(d, 'version', '')), '');
    else ok('已经是最新版本', '');
  }

  async function applyUpdate() {
    const yes = await confirmDialog('一键更新', '网关会下载新版本、替换自己的程序并重启。管理页会断开几秒。');
    if (!yes) return;
    const res = await gw('update/apply', {});
    if (!res.ok) { bad('更新失败', res.error); return; }
    ok('正在重启到新版本', '几秒后重新进入这一页即可看到新版本号。');
  }

  async function markRead(id) {
    const res = await gw('announcements/read', { id });
    if (!res.ok) { bad('标记已读失败', res.error); return; }
    await load();
    render();
  }

  async function refreshAnnouncements() {
    const res = await gw('announcements/refresh', {});
    if (!res.ok) { bad('拉取公告失败', res.error); return; }
    ok('已拉取公告', '一秒后自动刷新。');
    setTimeout(() => { load().then(render).catch((e) => console.error(e)); }, 1200);
  }

  /* ------------------------------------------------------------ 面：模型 */

  /* 模型清单分成两段：勾上的在上，取消勾选的在下。
     界线是"广不广播"：hidden 的那些仍接受显式点名调用，只是不写进
     /v1/models 的清单——它不是"停用"。所以两者不该混在一条流里：用户每找
     一个自己要的模型，都得往下跳过一堆自己关掉的。 */
  function modelHaystack(m) {
    return [
      String(pick(m, 'name', '')),
      String(pick(m, 'id', '')),
      String(pick(m, 'providerId', ''))
    ].join(' ').toLowerCase();
  }

  /* 一段模型区块的标题行。两段之间不是"间隔"，是"分界"：勾上的和取消勾选
     的是两种状态，靠一条分隔线加一句"这批会不会广播出去"的说明划开，比多留
     32px 空白更清楚。可折叠的那一段带一枚朝向箭头。 */
  function modelSection(title, count, note, opts) {
    const o = opts || {};
    const row = h('div', { class: 'model-section' + (o.fold ? ' is-foldable' : '') },
      o.fold ? h('span', { class: 'model-section-chev' }, icon(o.open ? 'chevronUp' : 'chevronDown')) : null,
      h('span', { class: 'model-section-title', text: title }),
      h('span', { class: 'pill tone-mute', text: String(count) }),
      h('span', { class: 'model-section-note', text: note })
    );
    if (o.fold && o.onToggle) {
      row.setAttribute('role', 'button');
      row.setAttribute('tabindex', '0');
      row.classList.toggle('is-open', !!o.open);
      row.addEventListener('click', o.onToggle);
      row.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); o.onToggle(); }
      });
    }
    return row;
  }

  function modelCard(m) {
    const id = String(pick(m, 'id', ''));
    const name = String(pick(m, 'name', id));
    const st = String(pick(m, 'state', 'unknown'));
    const ttft = asNumber(pick(m, 'ttftMs', 0), 0);
    const med = asNumber(pick(m, 'ttftMedMs', 0), 0);
    const cnt = asNumber(pick(m, 'ttftCount', 0), 0);
    const ctx = fmtWindow(pick(m, 'contextWindow', 0));
    const maxOut = fmtWindow(pick(m, 'maxOutput', 0));
    const hidden = !!pick(m, 'hidden', false);
    const custom = !!pick(m, 'custom', false);
    const quotaUsed = asNumber(pick(m, 'quotaUsed', 0), 0);
    const quotaEst = asNumber(pick(m, 'quotaEstimate', 0), 0);
    const throttledAt = asNumber(pick(m, 'throttledAt', 0), 0);
    const eta = asNumber(pick(m, 'recoverEta', 0), 0);
    const tagSource = String(pick(m, 'tagSource', ''));
    const detail = String(pick(m, 'detail', ''));
    const task = pick(m, 'task', null);
    const efforts = Array.isArray(pick(m, 'efforts', [])) ? pick(m, 'efforts', []) : [];
    const running = !!task && String(pick(task, 'status', '')) === 'running';

    const bits = [];
    if (ctx !== '—') bits.push(ctx + ' 上下文');
    if (maxOut !== '—') bits.push('最大输出 ' + maxOut);
    if (ttft) bits.push('首字 ' + ttft + 'ms');
    if (cnt && med) bits.push('中位 ' + med + 'ms（' + cnt + ' 次样本）');
    if (pick(m, 'vision', false)) bits.push('视觉');
    if (pick(m, 'audioInput', false)) bits.push('音频');
    if (pick(m, 'fileInput', false)) bits.push('文件');
    if (pick(m, 'reasoning', false)) bits.push('推理');
    if (custom) bits.push('供应商 ' + String(pick(m, 'providerId', '')));

    const quotaBits = [];
    if (quotaUsed) quotaBits.push('今日 ' + pct(quotaUsed) + ' tokens');
    if (quotaEst) quotaBits.push('额度估算 ' + pct(quotaEst));
    if (throttledAt) {
      quotaBits.push('限流于 ' + stamp(throttledAt) + (eta ? '，约 ' + stamp(eta) + ' 恢复' : ''));
    }

    /* 上游拒绝时常把整段 JSON 塞进 detail（RegionError / FreeUsageLimitError…）。
       那些字节属于"排查时要看"的东西，不是卡片副标题：这里只留一句人话，
       原文放进展开区，按等宽字体原样显示。 */
    const detailText = cleanDetail(detail);

    const desc = bits.concat(quotaBits).join('  ·  ')
      + (detailText ? '  ·  ' + tail(detailText, 96) : '');

    /* 尾部只留"这一条现在什么状态"：可用性 + 标签来源 + 正在测。
       「已隐藏」不再画成胶囊——它已经由它所在的段说了（下段整段的标题就是
       「已取消勾选」），同一句话说两遍会让每张卡右边多一格噪音。 */
    const tailChips = [stateTag(st)];
    if (tagSource) {
      const label = tagSource === 'probe' ? '实测' : tagSource === 'meta' ? '声明'
        : tagSource === 'ai' ? 'AI 标注' : tagSource === 'heuris' ? '推断' : tagSource;
      tailChips.push(toneTag(label, 'tone-mute'));
    }
    if (running) tailChips.push(toneTag('测试中', 'tone-warn'));

    /* 模型卡是一行长清单加三枚胶囊，挤进半格会把说明压成一条竖柱，
       所以它固定整行；这一页读的是"一条一条比较"，不是"扫一眼开关"。 */
    return Card({
      /* 车道卡左边那枚标说"这条模型从哪来"：免费车道是公共车道，自定义供应商
         是自家端点，所以两者用不同的记号，不是同一个通用图标。 */
      icon: custom ? 'server' : 'layers',
      title: name,
      desc,
      width: 'full',
      expandable: true,
      right: h('div', { class: 'row-list', style: 'flex-direction:row;align-items:center;gap:6px;flex-wrap:wrap' },
        tailChips),
      content: (() => {
        const box = h('div', { class: 'row-list', style: 'gap:10px' });

        if (efforts.length) {
          const row = h('div', { class: 'gw-form-row' },
            h('div', { class: 'gw-label', text: '三档思考预算' }));
          const chips = h('div', { class: 'gw-chips' });
          for (const e of efforts) {
            chips.append(h('span', {
              class: 'pill' + (pick(e, 'isDefault', false) ? ' tone-ok' : ' tone-mute'),
              text: String(pick(e, 'zh', pick(e, 'name', ''))) + ' ' + pct(pick(e, 'tokens', 0))
            }));
          }
          row.append(chips);
          box.append(row);
        }

        box.append(h('div', { class: 'gw-label', text: 'id：' + id }));
        if (detail) {
          box.append(h('div', { class: 'gw-form-row' },
            h('div', { class: 'gw-label', text: '上游原话' }),
            h('div', { class: 'gw-hint mono', style: 'white-space:pre-wrap;overflow-wrap:anywhere',
              text: detailText })
          ));
        }
        box.append(h('div', { class: 'gw-inline' },
          Button({
            label: running ? '测试中…' : '测试',
            variant: 'standard',
            icon: 'sync',
            onClick: () => probeOne(id, running)
          }),
          custom ? Button({ label: '能力实测', variant: 'standard', icon: 'eye', onClick: () => capsProbe(id) }) : null,
          Button({ label: '任务记录', variant: 'subtle', onClick: () => openTasks(id, name) }),
          /* 动作写清楚"点下去会发生什么"，而不是"这是哪个字段"：「可见性」
             读不出方向——它是已被勾上还是即将被取消？两种状态各给一个动词，
             并且不把它做成开关外观：这一行是清单，勾选是这一页的主结果。 */
          Button({
            label: hidden ? '重新勾选' : '取消勾选',
            variant: 'subtle',
            icon: hidden ? 'check' : 'close',
            onClick: () => setVisible(id, hidden)
          })
        ));

        if (task) {
          const steps = Array.isArray(pick(task, 'steps', [])) ? pick(task, 'steps', []) : [];
          const wrap = h('div', { class: 'gw-form-row' },
            h('div', { class: 'gw-label', text:
              '最近一次' + (String(pick(task, 'kind', '')) === 'caps' ? '能力实测' : '可用性测试')
              + '：' + (TASK_LABEL[String(pick(task, 'status', ''))] || '') }));
          for (const s of steps.slice(-4)) {
            wrap.append(h('div', { class: 'gw-step' },
              h('span', { class: 'step-name', text: String(pick(s, 'name', '')) }),
              h('span', { class: 'step-detail', text: String(pick(s, 'detail', '')) || String(pick(s, 'status', '')) }),
              h('span', { class: 'step-ms', text: asNumber(pick(s, 'ms', 0), 0) ? asNumber(pick(s, 'ms', 0), 0) + 'ms' : '' })
            ));
          }
          box.append(wrap);
        }

        return box;
      })()
    });
  }

  function buildModelsFace() {
    const models = Array.isArray(pick(state, 'models', [])) ? pick(state, 'models', []) : [];
    const available = models.filter((m) => String(pick(m, 'state', '')) === 'available').length;
    const probing = String(pick(state, 'probingModel', ''));
    const settings = pick(state, 'settings', {}) || {};

    const group = Group({
      title: '免费车道',
      desc: models.length
        ? available + ' / ' + models.length + ' 个可用  ·  可用性按本机网络出口实测'
          + (probing ? '  ·  正在探测 ' + probing : '')
        : '还没有探测结果'
    });

    /* 整行：这一面现在整页是"一条一条排下来"的清单，插一张半格卡会把
       唯一的左锚线从中间断开。 */
    group.body.append(Card({
      icon: 'sync',
      width: 'full',
      title: '整轮重新探测',
      desc: '按当前出口把免费车道全部重测一遍；每 '
        + asNumber(pick(settings, 'probeIntervalMinutes', 0), 0) + ' 分钟自动一轮。',
      right: hrow(
        Button({ label: '重新探测', variant: 'standard', onClick: reprobeAll }),
        Button({ label: 'AI 重新标注', variant: 'subtle', onClick: retag })
      )
    }));

    if (!models.length) {
      group.body.append(Card({ icon: 'info', width: 'full', title: '暂无模型', desc: '网关启动后会自动探测免费车道。' }));
      return group.node;
    }

    /* 搜索是"边打边筛"，不是"打完再筛"。它不重画整页，只重填下面那块结果区：
       render() 会 replaceChildren，把光标和还没提交的字符一起丢掉——原来那个
       筛选框之所以只能在失焦时提交，就是因为这个。这里输入框留在原地。 */
    const searchInput = h('input', {
      class: 'text-box model-search-input',
      type: 'text',
      placeholder: '搜索：名称、id 或供应商',
      value: modelFilter
    });
    const searchCount = h('span', { class: 'model-search-count' });
    const clearBtn = Button({
      label: '清除', variant: 'subtle',
      onClick: () => { modelFilter = ''; searchInput.value = ''; searchInput.focus(); paint(); }
    });
    const results = h('div', { class: 'model-results' });

    const onType = debounce(() => { modelFilter = searchInput.value; paint(); }, 90);
    searchInput.addEventListener('input', onType);
    searchInput.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') { e.preventDefault(); modelFilter = ''; searchInput.value = ''; paint(); }
    });

    group.body.append(h('div', { class: 'model-search' },
      h('span', { class: 'model-search-field' },
        h('span', { class: 'model-search-icon' }, icon('search')),
        searchInput),
      searchCount,
      clearBtn
    ), results);

    function paint() {
      const q = modelFilter.trim().toLowerCase();
      const hit = q ? models.filter((m) => modelHaystack(m).indexOf(q) >= 0) : models.slice();

      searchCount.textContent = q
        ? hit.length + ' / ' + models.length + ' 个匹配'
        : '共 ' + models.length + ' 个';
      searchCount.classList.toggle('is-zero', q !== '' && hit.length === 0);
      clearBtn.style.display = q ? '' : 'none';

      results.replaceChildren();

      if (q && !hit.length) {
        results.append(Card({
          icon: 'search', width: 'full', title: '没有匹配的模型',
          desc: '换个关键词试试：可以搜 id、显示名，或供应商（如 qoder、intern、nvidia-nim）。'
        }));
        return;
      }

      const open = hit.filter((m) => !pick(m, 'hidden', false));
      const shut = hit.filter((m) => pick(m, 'hidden', false));

      /* 上半段：勾上的。这是 /v1/models 真正广播出去的那一批，也是这一页的
         主体，所以它永远展开、永远排在前面。 */
      if (open.length) {
        results.append(modelSection('已勾选', open.length, '写进 /v1/models，对外可见'));
        const shown = showAllModels || q ? open : open.slice(0, 12);
        for (const m of shown) results.append(modelCard(m));
        if (open.length > shown.length) {
          results.append(Card({
            icon: 'more', width: 'full',
            title: '还有 ' + (open.length - shown.length) + ' 个已勾选的模型',
            desc: '点开查看全部',
            onClick: () => { showAllModels = true; paint(); }
          }));
        }
      }

      /* 下半段：取消勾选的。它们仍在目录里、仍能按 id 点名调用，只是不写进广播
         清单——所以是"降下去"，不是"删掉"。默认折起来：26 张卡铺在页尾，会把
         上面真正在用的那几张挤到要滚动才看得见。搜索时自动展开，用户搜的词才
         不会落在折起来的那一半里。 */
      if (shut.length) {
        results.append(modelSection('已取消勾选', shut.length,
          '不写进 /v1/models，仍可按 id 点名调用', {
            fold: true,
            open: modelsFold || q !== '',
            onToggle: () => { modelsFold = !modelsFold; paint(); }
          }));
        if (modelsFold || q) {
          for (const m of shut) results.append(modelCard(m));
        }
      }
    }

    paint();
    return group.node;
  }

  async function reprobeAll() {
    const res = await gw('reprobe', {});
    if (!res.ok) { bad('重新探测失败', res.error); return; }
    ok('已开始整轮探测', '结果会在探测进行时陆续出现，30 秒后会刷新一次。');
    setTimeout(() => { load().then(render).catch((e) => console.error(e)); }, 2000);
  }

  async function retag() {
    const res = await gw('tags/retag', {});
    if (!res.ok) { bad('重新标注失败', res.error); return; }
    const queued = asNumber(pick(res.data || {}, 'queued', 0), 0);
    ok('已排队 ' + queued + ' 个模型', 'AI 标注在后台跑，结果会写回模型标签。');
  }

  async function setVisible(id, visible) {
    const res = await gw('models/visibility', { id, hidden: !visible });
    if (!res.ok) { bad('可见性未保存', res.error); return; }
    await load();
    render();
  }

  async function probeOne(id, running) {
    if (running) { ok('这个模型正在测试', '等它这一次跑完，不要重复花额度。'); return; }
    const res = await gw('probe/' + encodeURIComponent(id), {});
    if (!res.ok) { bad('测试失败', res.error); await load(); render(); return; }
    const r = pick(res.data || {}, 'result', null);
    if (r) {
      const st = String(pick(r, 'state', ''));
      const ms = asNumber(pick(r, 'ttftMs', 0), 0);
      ok('测试完成：' + (STATE_LABEL[st] || st),
        (ms ? '首字 ' + ms + 'ms  ·  ' : '') + String(pick(r, 'detail', '')));
    } else if (pick(res.data || {}, 'running', false)) {
      ok('这个模型正在测试', '结果会写回卡片。');
    }
    await load();
    render();
  }

  async function capsProbe(id) {
    const res = await gw('tags/probe', { id });
    if (!res.ok) { bad('能力实测失败', res.error); return; }
    ok('能力实测已开始', '视觉 / 音频 / 文件三项会分阶段回报，点「任务记录」看进度。');
    setTimeout(() => { load().then(render).catch((e) => console.error(e)); }, 2000);
  }

  async function openTasks(model, name) {
    const res = await gw('tasks?model=' + encodeURIComponent(model), undefined);
    if (!res.ok) { bad('读取任务记录失败', res.error); return; }
    const runs = Array.isArray(pick(res.data || {}, 'runs', [])) ? pick(res.data || {}, 'runs', []) : [];
    const content = [];
    if (!runs.length) {
      content.push(h('div', { class: 'gw-hint', text: '这个模型还没有跑过测试。' }));
    }
    for (const t of runs) {
      const steps = Array.isArray(pick(t, 'steps', [])) ? pick(t, 'steps', []) : [];
      const block = h('div', { class: 'gw-form-row' },
        h('div', { class: 'gw-label', text:
          (String(pick(t, 'kind', '')) === 'caps' ? '能力实测' : '可用性测试')
          + '  ·  ' + (TASK_LABEL[String(pick(t, 'status', ''))] || String(pick(t, 'status', '')))
          + '  ·  ' + stamp(pick(t, 'started', 0)) }),
        h('div', { class: 'gw-hint', text: '编号 ' + String(pick(t, 'id', '')) })
      );
      for (const s of steps) {
        block.append(h('div', { class: 'gw-step' },
          h('span', { class: 'step-name', text: String(pick(s, 'name', '')) }),
          h('span', { class: 'step-detail', text: String(pick(s, 'detail', '')) || '—' }),
          h('span', { class: 'step-ms', text: asNumber(pick(s, 'ms', 0), 0) ? asNumber(pick(s, 'ms', 0), 0) + 'ms' : '' })
        ));
      }
      const verdicts = pick(t, 'verdicts', null);
      if (verdicts && typeof verdicts === 'object') {
        const parts = [];
        for (const k of Object.keys(verdicts)) parts.push(k + '=' + String(verdicts[k]));
        block.append(h('div', { class: 'gw-hint', text: '结论：' + parts.join('  ·  ') }));
      }
      content.push(block);
    }
    const dlg = Dialog({
      title: '任务记录 · ' + name,
      content: h('div', { class: 'gw-form',
        style: 'max-height:420px;overflow-y:auto;min-width:min(520px, 80vw)' }, content),
      primaryLabel: '关闭',
      onPrimary: null
    });
    dlg.open();
  }

  /* ------------------------------------------------------------ 面：供应商 */

  function emptyProvider() {
    return {
      id: '', name: '', baseUrl: '', apiKey: '', protocol: 'openai',
      enabled: true, models: [], catalog: [], note: ''
    };
  }

  function applyPreset(p) {
    draft.provider = {
      id: '', name: String(pick(p, 'name', '')),
      baseUrl: String(pick(p, 'baseUrl', '')),
      apiKey: '', protocol: String(pick(p, 'protocol', 'openai')),
      enabled: true, models: [], catalog: [], note: String(pick(p, 'note', ''))
    };
    draft.catalog = null;
    render();
  }

  function editProvider(pv) {
    const models = Array.isArray(pick(pv, 'models', [])) ? pick(pv, 'models', []) : [];
    draft.provider = {
      id: String(pick(pv, 'id', '')),
      name: String(pick(pv, 'name', '')),
      baseUrl: String(pick(pv, 'baseUrl', '')),
      apiKey: '',
      protocol: String(pick(pv, 'protocol', 'openai')),
      enabled: !!pick(pv, 'enabled', true),
      models: models.slice(),
      catalog: [],
      note: String(pick(pv, 'note', ''))
    };
    draft.catalog = null;
    render();
  }

  function providerForm() {
    const p = draft.provider;
    if (!p) return null;

    const wrap = h('div', { class: 'gw-form' });

    wrap.append(h('div', { class: 'gw-label', text: p.id ? '编辑供应商 · ' + p.id : '新增供应商' }));

    wrap.append(h('div', { class: 'gw-form-row' }, h('div', { class: 'gw-label', text: '名称' }),
      textField({ value: p.name, placeholder: '例如 NVIDIA NIM', width: 240,
        onChange: (v) => { p.name = v; } })));

    wrap.append(h('div', { class: 'gw-form-row' }, h('div', { class: 'gw-label', text: 'Base URL' }),
      textField({ value: p.baseUrl, placeholder: 'https://…/v1', width: 320, mono: true,
        onChange: (v) => { p.baseUrl = v; } })));

    wrap.append(h('div', { class: 'gw-form-row' }, h('div', { class: 'gw-label', text: '协议' }),
      Combo({ options: PROTOCOL_OPTIONS, value: p.protocol,
        onChange: (v) => { p.protocol = v; } })));

    wrap.append(h('div', { class: 'gw-form-row' },
      h('div', { class: 'gw-label', text: 'API Key' + (p.id ? '（留空表示不改）' : '') }),
      textField({ value: p.apiKey, password: true, placeholder: '粘贴上游 Key', width: 320,
        onChange: (v) => { p.apiKey = v; } })));

    if (p.note) wrap.append(h('div', { class: 'gw-hint', text: p.note }));

    wrap.append(h('div', { class: 'gw-inline' },
      Button({ label: '拉取模型列表', variant: 'standard', icon: 'sync', onClick: pullProviderModels }),
      Button({ label: '保存', variant: 'accent', onClick: saveProvider }),
      Button({ label: '取消', variant: 'subtle', onClick: () => { draft.provider = null; draft.catalog = null; render(); } })
    ));

    if (draft.catalog) {
      const rows = Array.isArray(draft.catalog) ? draft.catalog : [];
      wrap.append(h('div', { class: 'gw-label', text: '上游返回 ' + rows.length + ' 个模型，勾选的才会进网关目录' }));
      const list = h('div', { class: 'row-list', style: 'max-height:280px;overflow-y:auto' });
      for (const row of rows) {
        const id = String(pick(row, 'id', ''));
        const checked = p.models.indexOf(id) >= 0;
        list.append(h('div', { class: 'gw-inline', style: 'justify-content:space-between' },
          h('span', { class: 'gw-hint', style: 'flex:1;color:var(--text-primary)', text: id }),
          Toggle({
            checked,
            onChange: (v) => {
              if (v) { if (p.models.indexOf(id) < 0) p.models.push(id); }
              else { p.models = p.models.filter((x) => x !== id); }
            }
          })
        ));
      }
      wrap.append(list);
    } else if (p.models.length) {
      wrap.append(h('div', { class: 'gw-hint', text: '当前已选 ' + p.models.length + ' 个模型：' + p.models.join('、') }));
    }

    return wrap;
  }

  /* 一家供应商 = 一张卡，卡里的字段排成同一组"标签 / 值"格：端点、协议、
     Key、模型各占一格，标题下面第一行就能横向比价。字段写成独立的行而不是
    一句用 · 连起来的长副标题——一串点号分不出哪一段是 Key、哪一段是模型数，
     13 家摆在一起时每家的行还都不一样长。 */
  /* 卡里的每个事实都占一格"标签 / 值"，标签在上用 12px 三级色，值在下用
     13px 主色。写成格子而不是一句用 · 连起来的长副标题，是因为一串点号分不出
     哪一段是 Key、哪一段是模型数，而且十几家摆在一起时每家的行长短不一，
     横向没法比。 */
  /* wide: true 占两格，'all' 占整行。端点这类等宽串给整行——它一断行就分成
     两截，还容易正好卡在容器边缘上，读起来像被裁掉了。 */
  function provField(label, value, opts) {
    const o = opts || {};
    const cls = 'prov-field' + (o.wide === 'all' ? ' is-span' : o.wide ? ' is-wide' : '');
    const v = h('span', {
      class: 'prov-v' + (o.mono ? ' is-mono' : '') + (o.mute ? ' is-mute' : ''),
      text: value == null || value === '' ? '—' : String(value)
    });
    return h('div', { class: cls }, h('span', { class: 'prov-k', text: label }), v);
  }

  /* 排名只在"预设"里出现：它是网关建议的接入顺序；已接入的供应商按用户自己
     接的顺序排，不该被一个名次重新编号。两位数补成 01 这种，竖着对齐好看。 */
  function provRank(rank) {
    const n = asNumber(rank, 0);
    if (!n) return null;
    return h('span', { class: 'prov-rank', 'aria-label': '推荐顺序 ' + n,
      text: n < 10 ? '0' + n : String(n) });
  }

  /* 模型清单只铺前三个：一家长清单（ModelScope 七个）会把卡片撑成一大坨，
     而这里要回答的是"这家接进来能跑什么"，完整清单在编辑表单里。 */
  function modelList(models) {
    const head = models.slice(0, 3).join('、');
    return models.length > 3 ? head + ' 等 ' + models.length + ' 个' : head;
  }

  function provHead(rankNode, brandKey, name, badge, right) {
    return h('div', { class: 'prov-head' },
      rankNode,
      BrandMark({ id: brandKey || '' }),
      h('div', { class: 'prov-ident' },
        h('span', { class: 'prov-name', text: name }),
        badge || null
      ),
      h('div', { class: 'card-right is-control' }, right || null)
    );
  }

  function presetCard(p) {
    const url = String(pick(p, 'keyUrl', ''));
    const id = String(pick(p, 'id', ''));
    const baseUrl = String(pick(p, 'baseUrl', ''));
    const protocol = String(pick(p, 'protocol', '')) === 'anthropic' ? 'Anthropic' : 'OpenAI';
    const note = String(pick(p, 'note', ''));
    /* 预设是一张"排名清单"，所以每张占整行：01 在 02 上面，扫描顺序就是推荐
       顺序。占半格的话第二名列会插进序列里，名次读起来是锯齿。 */
    /* 预设卡占整行：它是一张"按名次读"的清单，半格里六个字段只能竖着叠，
       一页要滚五轮；整行时它们排成两排格，四家并排也能横向比同一格。 */
    return h('article', { class: 'card prov-card prov-preset has-rank is-full' },
      provHead(
        provRank(pick(p, 'rank', 0)),
        providerBrand(id, baseUrl),
        String(pick(p, 'name', '')),
        null,
        [
          /* 标准形态而不是强调色：十四张卡上各一枚蓝色按钮会把页面压满；
             但也不能退成纯文字——纯文字的"填入表单"看起来像一句标签，点不动。 */
          Button({ label: '填入表单', variant: 'standard', onClick: () => applyPreset(p) }),
          url ? Button({ icon: 'link', variant: 'subtle', label: '', onClick: () => openUrlAt(url) }) : null
        ]
      ),
      h('div', { class: 'prov-rows' },
        provField('免费档', pick(p, 'free', '')),
        provField('额度', pick(p, 'quota', '')),
        provField('网络', pick(p, 'region', '')),
        provField('协议', protocol),
        provField('端点', baseUrl.replace(/^https?:\/\//, ''), { wide: 'all', mono: true }),
        provField('免费模型', pick(p, 'models', ''), { wide: 'all' })
      ),
      note ? h('div', { class: 'prov-note', text: note }) : null
    );
  }

  function connectedCard(pv) {
    const id = String(pick(pv, 'id', ''));
    const models = Array.isArray(pick(pv, 'models', [])) ? pick(pv, 'models', []) : [];
    const enabled = !!pick(pv, 'enabled', false);
    const hasKey = !!pick(pv, 'hasKey', false);
    const baseUrl = String(pick(pv, 'baseUrl', ''));
    const protocol = String(pick(pv, 'protocol', '')) === 'anthropic' ? 'Anthropic' : 'OpenAI';
    return h('article', { class: 'card prov-card' + (enabled ? '' : ' is-off') },
      provHead(
        null,
        providerBrand(id, baseUrl),
        String(pick(pv, 'name', id)),
        toneTag(enabled ? '已启用' : '已停用', enabled ? 'tone-ok' : 'tone-mute'),
        Toggle({ checked: enabled, onChange: (v) => toggleProvider(id, pv, v) })
      ),
      h('div', { class: 'prov-rows' },
        provField('端点', baseUrl.replace(/^https?:\/\//, ''), { wide: true, mono: true }),
        provField('协议', protocol),
        provField('Key', hasKey ? String(pick(pv, 'apiKeyMasked', '')) : '未填', { mono: true, mute: !hasKey }),
        provField('模型', models.length ? modelList(models) : '还没有勾选模型', { wide: true, mute: !models.length })
      ),
      h('div', { class: 'gw-inline' },
        Button({ label: '编辑', variant: 'standard', onClick: () => editProvider(pv) }),
        Button({ label: '刷新模型', variant: 'subtle', onClick: () => refreshProviderModels(id) }),
        Button({ label: '删除', variant: 'critical', onClick: () => deleteProvider(id, String(pick(pv, 'name', id))) })
      )
    );
  }

  function tierLabel(tier) {
    if (tier <= 1) return { title: '首选', desc: '额度写明白、注册就能用，先接这几家。' };
    if (tier === 2) return { title: '可用', desc: '有免费档，但要看网络、协议或只免费一部分模型。' };
    return { title: '备选', desc: '限速很紧或要付费，留着对照和应急。' };
  }

  function buildProvidersFace() {
    const providers = Array.isArray(pick(state, 'providers', [])) ? pick(state, 'providers', []) : [];
    const presets = Array.isArray(pick(state, 'providerPresets', [])) ? pick(state, 'providerPresets', []) : [];

    const group = Group({
      title: '已接入供应商',
      desc: '加上自己的 Key 之后，免费车道耗尽的请求会回落到这些上游（需在「设置」打开回落开关）。'
    });

    /* 新增这一格和一行供应商卡同高同宽：它不是"另一类东西"，是同一排里的
       一个位置。左边那枚加号也是 32px 方块位，排在品牌标该在的地方。 */
    /* 新增这一格占整行，但内部几何和下面每一张供应商卡一样：加号坐在品牌方块的
       位置（32px），名字从同一个 X 起。这样"新增"看起来是这支列表的第一行，
       而不是另一种卡。 */
    const addCard = h('article', { class: 'card prov-card prov-add is-full' },
      h('div', { class: 'prov-head' },
        h('span', { class: 'card-brand prov-addbox', 'aria-hidden': 'true' }, icon('add')),
        h('div', { class: 'prov-ident' },
          h('span', { class: 'prov-name', text: '新增供应商' }),
          h('span', { class: 'prov-sub', text: presets.length ? '从下面的预设一键填入端点，或手动填一个兼容端点' : '手动填一个兼容端点' })
        ),
        h('div', { class: 'card-right is-control' },
          Button({ label: '新建', variant: 'accent', onClick: () => { draft.provider = emptyProvider(); draft.catalog = null; render(); } })
        )
      )
    );
    group.body.append(addCard);

    if (draft.provider) {
      group.body.append(h('div', { class: 'card gw-block' }, providerForm()));
    }

    if (!providers.length && !draft.provider) {
      group.body.append(h('div', { class: 'card gw-block' },
        h('div', { class: 'gw-hint', text: '还没有接入任何供应商。从下面「预设」里选一家填入表单即可。' })));
    }

    for (const pv of providers) group.body.append(connectedCard(pv));

    if (!presets.length) return group.node;

    /* 预设按 tier 分组，组内按 rank 排：首选在最上面，"额度写明白"的那几家
       先被看到；备选沉底，但仍然在页面上，不是藏起来。 */
    const tiers = [1, 2, 3];
    const pages = [group.node];
    for (const t of tiers) {
      const rows = presets.filter((p) => asNumber(pick(p, 'tier', 1), 1) === t);
      if (!rows.length) continue;
      const meta = tierLabel(t);
      const g = Group({ title: meta.title + '（' + rows.length + ' 家）', desc: meta.desc });
      for (const p of rows) g.body.append(presetCard(p));
      pages.push(g.node);
    }
    return h('div', { class: 'page-body', style: 'gap:32px' }, pages);
  }

  /* ------------------------------------------------------------ 面：聚合 */

  /* 聚合面读的是 GET aggregate/state（不是 /admin/api/state）：这份表随临期
     变化，重算时机和主状态不一致，混进主状态会让 30 秒轮询替它做决定。 */

  /* 「还有多久作废」是这一页的全部价值，所以它写成人话而不是百分比：
     剩余比例要除以一个"总额度寿命"，那个分母上游不给，编一个就是在画假数据。 */
  function remainText(ms, never, channel) {
    if (never) return isNativeChannel(channel) ? '余额可用 · 未提供精确到期' : '无到期（永久额度）';
    const left = asNumber(ms, 0) - Date.now();
    if (!ms) return '到期时刻未知';
    if (left <= 0) return '已到期';
    const days = Math.floor(left / 86400000);
    const hours = Math.floor((left % 86400000) / 3600000);
    const mins = Math.floor((left % 3600000) / 60000);
    if (days >= 1) return days + ' 天' + (hours ? ' ' + hours + ' 小时' : '') + '后作废';
    if (hours >= 1) return hours + ' 小时' + (mins ? ' ' + mins + ' 分' : '') + '后作废';
    return Math.max(1, mins) + ' 分钟后作废';
  }

  /* 紧迫度只给药丸，不给进度条：条的填充比例必须有真实分母，而"额度总寿命"
     上游没有这个数。三档写死的是判定阈值，读的人能自己核对。 */
  function remainTone(ms, never) {
    if (never || !ms) return 'tone-mute';
    const left = asNumber(ms, 0) - Date.now();
    if (left <= 0) return 'tone-bad';
    /* 两档而不是一档：半天内作废和三天内作废不是同一件事，全涂成一种黄，
       读的人就得逐个去念那几个字，药丸也就白放了。 */
    if (left < 12 * 3600000) return 'tone-bad';
    if (left < 3 * 86400000) return 'tone-warn';
    return 'tone-mute';
  }

  /* 没有登记品牌的渠道不留虚线空框：那一竖行 7 个空框看起来像"这一面没做完"。
     改用它自己的名字首字（中文取首字、英文取首字母），是"这家是谁"的缩写，
     而不是一个编造的品牌标。 */
  function chanMark(id, name) {
    const key = chanBrand(id);
    const known = !!(BRANDS[key] || brandOf(key));
    if (known) return BrandMark({ id: key });
    const ch = String(name || id || '?').trim().slice(0, 1).toUpperCase();
    return h('span', { class: 'card-brand is-mono', 'aria-hidden': 'true' },
      h('span', { class: 'brand-mono', text: ch }));
  }

  /* 渠道 id → 品牌标。渠道表里的 id 是账号渠道自己的叫法（buddy / zcode），
     品牌登记表按产品名登记（codebuddy），两套命名必须显式对上，否则十几行
     全是虚线空框——那会让人以为"这家没有标"，而不是"标还没配上"。 */
  const CHANNEL_BRAND = {
    buddy: 'codebuddy',
    workbuddy: 'workbuddy',
    zcode: 'zcode',
    qoder: 'qoder',
    qodercn: 'qoder',
    gemini: 'googlegemini',
    modelscope: 'modelscope',
    zhipu: 'zhipu',
    nvidia: 'nvidia',
    opencode: 'opencode',
    openrouter: 'openrouter',
    huggingface: 'huggingface',
    siliconflow: 'siliconflow',
    anthropic: 'anthropic',
    minimax: 'anthropic',
    minimaxintl: 'anthropic'
  };

  const chanBrand = (id) => CHANNEL_BRAND[String(id)] || String(id);

  /* 一个渠道的"临期"是同它所有在轮换里的候选里最早的那个时刻。
     不算它自己的某一份额度——渠道级没有单一的到期时刻，把某个模型的到期
     当成"这家的到期"会让整行在一个具体模型前说谎。 */
  function chanExpiry(agg, id) {
    const rows = [];
    const list = Array.isArray(pick(agg, 'virtual', [])) ? pick(agg, 'virtual', []) : [];
    for (const vm of list) {
      for (const c of (Array.isArray(pick(vm, 'candidates', [])) ? pick(vm, 'candidates', []) : [])) {
        if (String(pick(c, 'channel', '')) !== String(id)) continue;
        if (!pick(c, 'eligible', false)) continue;
        rows.push(c);
      }
    }
    if (!rows.length) return { ms: 0, never: false, any: false };
    let best = 0;
    let never = false;
    for (const c of rows) {
      if (pick(c, 'never', false)) { never = true; continue; }
      const ms = asNumber(pick(c, 'expiryMs', 0), 0);
      if (ms && (!best || ms < best)) best = ms;
    }
    return { ms: best, never: never && !best, any: true };
  }

  function chanOf(agg, id) {
    const list = Array.isArray(pick(agg, 'channels', [])) ? pick(agg, 'channels', []) : [];
    return list.find((c) => String(pick(c, 'id', '')) === String(id)) || {};
  }

  function chanTitle(agg, id, fallback) {
    const c = chanOf(agg, id);
    return String(pick(c, 'name', fallback || id));
  }

  function aggregateMembershipButton(c) {
    const cid = String(pick(c, 'id', ''));
    const on = !!pick(c, 'aggregateEnabled', true);
    const button = Button({
      label: on ? '参与轮换' : '不参与轮换',
      variant: on ? 'standard' : 'subtle',
      onClick: async () => {
        if (button.disabled) return;
        button.disabled = true;
        button.setAttribute('aria-busy', 'true');
        try {
          await saveAggSettings({ channelEnabled: { [cid]: !on } }, '渠道轮换设置未保存');
        } finally {
          button.disabled = false;
          button.removeAttribute('aria-busy');
        }
      }
    });
    button.classList.add('agg-membership-button');
    button.classList.toggle('is-on', on);
    button.setAttribute('aria-pressed', String(on));
    button.setAttribute('aria-label', String(pick(c, 'name', cid)) + '：' + (on ? '参与轮换，点击退出' : '不参与轮换，点击接入'));
    return button;
  }

  /* 一行渠道：名次是等宽数字，竖着扫是一条对得齐的轴（和供应商面同一套几何）。
     右侧那对箭头是真的会改轮换顺序：手工位次严格优先于临期，所以点一次就把
     这一家固定到前面/后面，而不是"建议"。 */
  function aggTurnRow(agg, ch, idx, total, order) {
    const cid = String(pick(ch, 'id', ''));
    const c = chanOf(agg, cid);
    const accounts = asNumber(pick(c, 'accounts', 0), 0);
    const unit = String(pick(c, 'creditUnit', ''));
    const note = String(pick(c, 'note', ''));
    /* 到期优先读**渠道级**那三个字段（后端直接给出的这次读结果），因为它们能区分
       「读失败」与「读到没有额度」；推不出来的老数据才回落到「从候选里取最早那条」。
       两种来源混用时以渠道级为准：候选的到期是同一份额度的另一种说法，二者若不一致，
       渠道级那句更接近本次探测的结论。 */
    const readFailed = !!pick(c, 'readFailed', false);
    const knownMs = asNumber(pick(c, 'expiryMs', 0), 0);
    const knownNever = !!pick(c, 'never', false);
    const exp = (knownMs || knownNever || readFailed)
      ? { any: knownMs > 0 || knownNever, ms: knownMs, never: knownNever, failed: readFailed }
      : chanExpiry(agg, cid);
    const participates = !!pick(c, 'aggregateEnabled', true);
    const usable = participates && accounts > 0 && exp.any;

    const move = (delta) => {
      const next = order.slice();
      const at = next.indexOf(String(pick(ch, 'id', '')));
      const to = clamp(at + delta, 0, next.length - 1);
      if (at < 0 || to === at || to < 0) return;
      next.splice(at, 1);
      next.splice(to, 0, String(pick(ch, 'id', '')));
      saveAggOrder(next);
    };

    return h('div', { class: 'agg-turn' + (usable ? '' : ' is-off') },
      h('span', { class: 'prov-rank', text: String(idx + 1).padStart(2, '0') }),
      chanMark(cid, chanTitle(agg, cid)),
      h('div', { class: 'agg-turn-main' },
        h('div', { class: 'agg-turn-name' },
          h('span', { text: chanTitle(agg, cid, cid) }),
          unit && unit !== '无' ? toneTag(unit, 'tone-mute') : null,
          !pick(c, 'wired', true) ? toneTag('未接入轮换', 'tone-mute') : null),
        h('div', { class: 'agg-turn-sub', text:
          (accounts ? accounts + ' 个账号' : '没有启用的账号')
          + (note ? '  ·  ' + note : '') })),
      h('div', { class: 'agg-turn-right' },
        /* 三种状态各说一句，而不是合成一句「无在轮换的额度」：读失败是这次探测的问题
           （可能一点「重算临期」就好了），没额度是这一家当前真的没东西可烧。 */
        !participates ? toneTag('未接入聚合', 'tone-mute') : exp.failed
          ? toneTag('这次读不到额度', 'tone-warn')
          : exp.any
            ? toneTag(remainText(exp.ms, exp.never, cid), remainTone(exp.ms, exp.never))
            : toneTag('无在轮换的额度', 'tone-mute'),
        h('div', { class: 'agg-turn-move' },
          Button({ icon: 'up', variant: 'subtle', square: true, disabled: idx === 0, onClick: () => move(-1) }),
          Button({ icon: 'down', variant: 'subtle', square: true, disabled: idx === total - 1, onClick: () => move(1) }))));
  }

  /* 一条候选 = 备用链上的一站。这一面要说清的不是"有几家"，而是**第几站**：
     请求先发给第 1 站，它不行（限流 / 额度空 / 连不上）才发给第 2 站。
     所以每一行带位次和一根连线，位次断了就说明这条链在这里结束。 */
  function aggCandidateRow(agg, vm, cand, index, total) {
    const chID = String(pick(cand, 'channel', ''));
    const realID = String(pick(cand, 'realId', ''));
    const realName = String(pick(cand, 'realName', realID));
    const via = { id: '按模型 ID 匹配', name: '按展示名匹配', patch: '按已确认别名匹配' }[pick(cand, 'via', 'id')] || '按模型 ID 匹配';
    const price = pick(cand, 'price', null);
    const baseRate = pick(cand, 'creditsRate', null);
    const knownPrice = price !== null && Number.isFinite(Number(price));
    const priceText = !knownPrice ? '倍率未提供'
      : (baseRate !== null && Number.isFinite(Number(baseRate)) && Number(baseRate) !== Number(price)
        ? '积分倍率 ×' + baseRate + ' → ×' + price + (Number(price) === 0 ? '（免费）' : '')
        : (Number(price) === 0 ? '免费 · ×0' : '积分倍率 ×' + price));
    const never = !!pick(cand, 'never', false);
    const expiryMs = asNumber(pick(cand, 'expiryMs', 0), 0);
    const eligible = !!pick(cand, 'eligible', false);
    const wired = !!pick(cand, 'wired', true);
    const excluded = !!pick(cand, 'excluded', false);
    const channelEnabled = !!pick(cand, 'channelEnabled', true);
    const requiresKey = !!pick(cand, 'requiresKey', false);
    /* 读失败与「没有额度」是两件事：前者点一次「重算临期」可能就好了，后者不会。
       合成一句话会让用户对一次网络抖动做出「这家不能用了」的判断。 */
    const readFailed = !!pick(cand, 'readFailed', false);
    const last = index === total - 1;

    /* 关掉一条候选只改这一条：写回去的是整张排除名单（去掉当前这行），
       而不是"这次改了什么"——后端只认全量，避免两处各持半张表。 */
    const setOn = (on) => {
      saveAggSettings((current) => {
        const off = aggregateExcluded(current).filter((k) => k !== (chID + '|' + realID));
        if (!on) off.push(chID + '|' + realID);
        return { excluded: off };
      }, on ? '已放回备用链' : '已移出备用链');
    };

    /* 停用的那一站仍然占着位次（而不是被删掉）：链断在哪要看得出来，
       删一行之后"这家本来能接"就无从判断了。 */
    const row = h('div', { class: 'agg-step-line' },
      h('span', { class: 'agg-step-no', text: String(index + 1).padStart(2, '0') }),
      h('span', { class: 'agg-step-rail', 'aria-hidden': 'true' }),
      h('div', { class: 'agg-step-body' },
        h('div', { class: 'agg-step-main' },
          chanMark(chID, chanTitle(agg, chID, chID)),
          h('div', { class: 'agg-step-id' },
            h('div', { class: 'agg-step-name' },
              h('span', { text: chanTitle(agg, chID, chID) }),
              /* 这一行的胶囊要说清**为什么**这一站现在不能用：三种原因要用户做的事
                 完全不同（打开开关 / 等额度探测 / 登录）。一律写"不在链上"，读的人
                 会去翻开关，而问题在账号。 */
              excluded ? toneTag('已停用', 'tone-mute')
                : !channelEnabled ? toneTag('渠道未接入聚合', 'tone-mute')
                  : !wired ? toneTag('未接入轮换', 'tone-mute')
                  : requiresKey ? toneTag('需要渠道 API Key', 'tone-warn')
                  : readFailed ? toneTag('读不到额度', 'tone-warn')
                    : !eligible ? toneTag('没有可用账号', 'tone-warn')
                      : null),
            h('div', { class: 'agg-step-real mono', text: realID, title: realName + ' · ' + via }),
            h('div', { class: 'agg-step-real', text: via + (realName !== realID ? ' · ' + realName : '') }))),
        h('div', { class: 'agg-step-side' },
          h('span', { class: 'agg-step-price mono', text: priceText, title: knownPrice ? '渠道公布的当前倍率；促销按生效时段计算。不同渠道的积分单位由各渠道定义。' : '渠道未公布积分倍率；按 token 计费的价格不会换算为积分倍率。' }),
          readFailed
            ? toneTag('这次读不到额度', 'tone-warn')
            : toneTag(remainText(expiryMs, never, chID), remainTone(expiryMs, never)),
          Toggle({ checked: !excluded, onChange: (v) => setOn(v), labelOn: '用', labelOff: '停', showLabel: false }))));

    const box = h('div', { class: 'agg-step' + (excluded || !eligible ? ' is-off' : '') + (last ? ' is-last' : '') }, row);
    /* 站与站之间那根连线只在中间出现一次，并写明它代表什么：这条链的全部
       含义就是"上一站不行才走下一站"，不写出来读的人只会当成装饰。 */
    /* 这句话只在第一处断点写一次：每一段都重复一遍，读起来像同样的脚注被
       贴了三张，反而看不清"哪一段是链"。后面几段只留连线，规则已经说过了。 */
    if (!last) {
      box.append(h('div', { class: 'agg-step-elbow' },
        h('span', { class: 'agg-step-elbow-line', 'aria-hidden': 'true' }),
        index === 0 ? h('span', { class: 'agg-step-elbow-text', text: '不行就换下一站，直到有一站接住' }) : null));
    }
    return box;
  }

  /* 一个规范名一张卡：卡头是"这个名字在几处有、先烧谁"，展开才看逐条候选。
     默认收起是有意为之——十几个虚拟模型全展开会把这一面撑成几十屏，而它平时
     只需要回答"这批模型够不够冗余"。 */
  function aggregateModelCard(agg, vm, order) {
    const key = String(pick(vm, 'key', ''));
    const name = String(pick(vm, 'name', key));
    const cands = Array.isArray(pick(vm, 'candidates', [])) ? pick(vm, 'candidates', []) : [];
    const live = cands.filter((c) => !!pick(c, 'eligible', false));
    const first = live.length ? String(pick(live[0], 'channel', '')) : '';
    const opened = !!draft.aggOpen[key];
    const allOff = cands.length > 0 && cands.every((c) => !!pick(c, 'excluded', false));
    const bulk = Button({
      label: allOff ? '全部启用' : '全部停用',
      variant: 'subtle',
      disabled: !cands.length,
      onClick: async () => {
        if (bulk.disabled) return;
        bulk.disabled = true;
        bulk.setAttribute('aria-busy', 'true');
        try {
          await saveAggSettings((current) => {
            const latest = pick(current, 'virtual', []).find((m) => String(pick(m, 'key', '')) === key);
            const candidates = latest ? pick(latest, 'candidates', []) : cands;
            const keys = new Set(candidates.map((c) => String(pick(c, 'channel', '')) + '|' + String(pick(c, 'realId', ''))));
            const excluded = new Set(aggregateExcluded(current));
            keys.forEach((candidate) => { if (allOff) excluded.delete(candidate); else excluded.add(candidate); });
            return { excluded: Array.from(excluded) };
          }, allOff ? '模型全部渠道未能启用' : '模型全部渠道未能停用');
        } finally {
          bulk.disabled = !cands.length;
          bulk.removeAttribute('aria-busy');
        }
      }
    });
    bulk.classList.add('agg-model-bulk');
    bulk.title = (allOff ? '启用' : '停用') + '这个模型的全部渠道候选，不改变渠道总开关';
    bulk.setAttribute('aria-label', name + '：' + (allOff ? '全部启用' : '全部停用'));

    /* 卡头直接把"这一个模型能走哪几家、按什么顺序"画成一条小链：
       名字 → 第 1 站 → → 第 2 站 …。收起状态也能读到"它有几条退路"，
       这正是聚合要做的事；展开只是看每一站的真实 id 和额度。 */
    const hops = h('div', { class: 'agg-hops' });
    live.forEach((c, i) => {
      if (i) hops.append(h('span', { class: 'agg-hop-arrow', 'aria-hidden': 'true' }, icon('chevronRight')));
      const cid = String(pick(c, 'channel', ''));
      hops.append(h('span', { class: 'agg-hop' + (i === 0 ? ' is-first' : '') },
        h('span', { class: 'agg-hop-dot', 'aria-hidden': 'true' }),
        h('span', { class: 'agg-hop-text', text: chanTitle(agg, cid, cid) })));
    });
    if (!live.length) {
      /* 链路画不出来时要说清**卡在哪一步**，而不是笼统一句"没有一站能接"：
         没账号、这次读不到额度、被用户停用，是三种要用户做不同事情的处境。 */
      const why = !cands.length ? '这家还没有上报模型'
        : cands.some((c) => !!pick(c, 'readFailed', false)) ? '这次读不到额度'
          : cands.every((c) => !!pick(c, 'excluded', false)) ? '候选已被停用'
            : cands.every((c) => !pick(c, 'channelEnabled', true)) ? '渠道未接入聚合'
              : cands.some((c) => !!pick(c, 'requiresKey', false)) ? '需要配置渠道 API Key'
            : '账号没登录或没有额度';
      hops.append(h('span', { class: 'agg-hop is-none', text: why }));
    }

    /* 卡头是这一张卡的唯一入口，所以它必须是个真的按钮：role / tabindex /
       回车与空格 / aria-expanded 一样都不能少。只挂 click 的 div 在键盘上
       按不动——「能点的东西要让人看得出能点」，也包括用键盘的人。 */
    const head = h('div', {
      class: 'agg-vm-head',
      role: 'button',
      tabindex: '0',
      'aria-expanded': opened ? 'true' : 'false'
    },
      h('div', { class: 'agg-vm-id' },
        h('span', { class: 'agg-vm-name', text: name }),
        h('span', { class: 'agg-vm-key mono', text: key })),
      hops,
      h('div', { class: 'agg-vm-meta' },
        /* 两枚胶囊各说一件事：左边"能接几站"（冗余度，聚合的全部价值），
           右边只在有候选被撤下时才出现——否则两个数字几乎总是同一个，
           并列摆着反而让人以为哪里对不上。 */
        /* 0 站时左侧那枚胶囊不写原因：原因已经在卡头的小链位置上写了一次，
           这里再写一句只是同一句话排两遍，而两处颜色不同还会让人以为各指一件事。 */
        live.length > 1 ? toneTag(live.length + ' 站可接', 'tone-ok')
          : live.length === 1 ? toneTag('单点 · 挂了就没了', 'tone-warn')
            : toneTag(cands.length ? '0 站可接' : '没有候选',
              cands.length ? 'tone-mute' : 'tone-mute'),
        cands.length > live.length
          ? toneTag((cands.length - live.length) + ' 条暂不可用', 'tone-mute') : null),
      h('span', { class: 'agg-caret' + (opened ? ' is-open' : '') }, icon('chevronDown')));

    const flip = () => {
      draft.aggOpen[key] = !opened;
      render();
    };
    head.addEventListener('click', flip);
    head.addEventListener('keydown', (ev) => {
      if (ev.key !== 'Enter' && ev.key !== ' ') return;
      ev.preventDefault();
      flip();
    });

    const card = h('article', { class: 'card agg-vm' + (opened ? ' is-open' : '') },
      h('div', { class: 'agg-vm-header' }, head, bulk));
    if (!opened) return card;

    /* 顺序就是这张卡的答案：候选表已经按轮换顺序排过（临期升序 / 手工位次），
       所以第一行就是"现在会发往哪一家"。表头换成一句人话，因为它讲的不是
       "有几列"，而是"这一串是什么"。 */
    const chain = h('div', { class: 'agg-chain' },
      h('div', { class: 'agg-chain-head' },
        h('span', { class: 'agg-chain-title', text: '备用链' }),
        h('span', { class: 'agg-chain-sub', text: live.length
          ? '从上往下依次尝试 · 前 ' + live.length + ' 站能接'
          : cands.some((c) => !!pick(c, 'readFailed', false))
            ? '点「重算临期」重读一次；也可以先用这几家直连的模型名'
            : '先在「供应商」面接入账号，或到「Agent」面登录渠道账号' })));
    cands.forEach((c, i) => chain.append(aggCandidateRow(agg, vm, c, i, cands.length)));
    card.append(chain);
    return card;
  }

  async function loadAggregate() {
    const res = await gw('aggregate/state');
    if (!res.ok) { draft.aggregateError = res.error; draft.aggregate = null; return false; }
    draft.aggregateError = '';
    draft.aggregate = res.data || {};
    return true;
  }

  function aggregateExcluded(agg) {
    const saved = pick(agg, 'excluded', null);
    if (Array.isArray(saved)) return saved.slice();
    const out = [];
    const list = Array.isArray(pick(agg, 'virtual', [])) ? pick(agg, 'virtual', []) : [];
    for (const vm of list) {
      for (const c of (Array.isArray(pick(vm, 'candidates', [])) ? pick(vm, 'candidates', []) : [])) {
        if (pick(c, 'excluded', false)) out.push(String(pick(c, 'channel', '')) + '|' + String(pick(c, 'realId', '')));
      }
    }
    return out;
  }

  /* 渠道级顺序单独占 AggregateManualRank 的一格（后端 ChannelOrderKey）。
     原来这版是从各虚拟模型的候选表反推的：30 家渠道里只有 2 家有候选，于是
     拖动第 3 家往后的箭头时算出的名单只有那 2 家——位次等于没写，重新读回来
     又被临期排序拉回原位，看到的就是"点了没反应"。 */
  const CHANNEL_ORDER_KEY = '[channels]';

  async function saveAggOrder(ids) {
    const okd = await saveAggSettings({ manualRank: aggregateManualRank(ids) }, '顺序未保存');
    if (okd) ok('轮换顺序已保存', '手工排过的渠道一律排在没排过的之前。');
  }

  /* 读改写：只替换渠道级那一格，各虚拟模型自己的位次原样带回去。
     manualRank 是全量覆盖（后端如此，这样「关掉一条、另一条自己回来」不会
     发生），所以前端必须交出整张表，而不是只交自己动过的那一格。 */
  function aggregateManualRank(ids) {
    const agg = draft.aggregate || {};
    const cur = pick(agg, 'manualRank', {}) || {};
    const rank = {};
    for (const k of Object.keys(cur)) {
      if (k === CHANNEL_ORDER_KEY) continue;
      const v = cur[k];
      if (Array.isArray(v) && v.length) rank[k] = v.slice();
    }
    rank[CHANNEL_ORDER_KEY] = ids.slice();
    return rank;
  }

  let aggregateSaveQueue = Promise.resolve();
  function saveAggSettings(patch, title) {
    const save = async () => {
      // 逐次基于最新状态修改，连续停用多行时不互相覆盖。
      const payload = typeof patch === 'function' ? patch(draft.aggregate || {}) : patch;
      const res = await gw('aggregate/settings', payload);
      if (!res.ok) { bad(title || '聚合设置未保存', res.error); return false; }
      draft.aggregate = pick(res.data || {}, 'state', draft.aggregate);
      render();
      return true;
    };
    const pending = aggregateSaveQueue.then(save, save);
    aggregateSaveQueue = pending.catch(() => {});
    return pending;
  }

  async function recheckAggregate() {
    const res = await gw('aggregate/recheck', {});
    if (!res.ok) { bad('重算临期失败', res.error); return; }
    ok('临期已重算', '重算了 ' + asNumber(pick(res.data || {}, 'recomputed', 0), 0) + ' 条候选。');
    await loadAggregate();
    render();
  }

  async function refreshAggregateModels() {
    if (draft.aggRefreshing) return;
    draft.aggRefreshing = true;
    render();
    const res = await gw('aggregate/models/refresh', {});
    draft.aggRefreshing = false;
    if (!res.ok) { bad('刷新模型失败', res.error); render(); return; }
    draft.aggregate = pick(res.data || {}, 'state', draft.aggregate);
    const failed = pick(pick(res.data || {}, 'report', {}), 'failed', []);
    ok('模型目录刷新完成', failed.length
      ? failed.join('、') + ' 暂时读取失败，已保留原目录。'
      : '包含需要积分的模型；上游暂不可达时保留已有目录。');
    render();
  }

  function buildAggregateFace() {
    const agg = draft.aggregate;
    if (!agg) {
      return h('div', { class: 'page-body', style: 'gap:32px' },
        Group({
          title: '聚合路由',
          desc: '把同一个真实模型在各渠道的条目归到一个规范名下，按「最快作废的额度先用」自动选渠道。'
        }).node,
        Card({
          icon: 'route',
          width: 'full',
          title: draft.aggregateError ? '读不到聚合状态' : '正在读取聚合状态',
          desc: draft.aggregateError
            ? draft.aggregateError + '  ·  这一面要等网关把 aggregate/state 路由挂上。'
            : '这一份表随临期变化，和主状态分开取。',
          right: Button({ label: '重试', variant: 'standard', onClick: async () => { await loadAggregate(); render(); } })
        }));
    }

    const enabled = !!pick(agg, 'enabled', false);
    const totals = pick(agg, 'totals', {}) || {};
    const channels = Array.isArray(pick(agg, 'channels', [])) ? pick(agg, 'channels', []) : [];
    const modelStatusRank = (vm) => {
      const cands = pick(vm, 'candidates', []);
      if (cands.some((c) => !!pick(c, 'eligible', false))) return 0;
      if (cands.some((c) => !pick(c, 'excluded', false) && pick(c, 'channelEnabled', true))) return 1;
      return 2;
    };
    const virtuals = (Array.isArray(pick(agg, 'virtual', [])) ? pick(agg, 'virtual', []) : []).slice()
      .sort((a, b) => modelStatusRank(a) - modelStatusRank(b));
    const order = (Array.isArray(pick(agg, 'order', [])) ? pick(agg, 'order', []) : []).filter(Boolean);

    const pages = [];

    /* 开关单独一组：它决定的是"要不要这一层"，不是这一层的某条参数。 */
    const top = Group({
      title: '聚合路由',
      desc: enabled
        ? '已开启：客户端统一显示聚合模型名；停用的候选不再参与，全部停用的模型会从列表移除。'
        : '未开启：网关只按各渠道自己的模型名提供服务，不做跨渠道选型。'
    });
    top.body.append(Card({
      icon: 'route',
      width: 'full',
      title: '按临期顺序自动挑渠道',
      desc: '多家都有额度时，先烧最快作废的那份；没吐出内容才换下一家。',
      right: hrow(
        Toggle({ checked: enabled, onChange: (v) => saveAggSettings({ enabled: v }, '聚合开关未保存') }),
        Button({ label: draft.aggRefreshing ? '正在刷新…' : '刷新模型', disabled: draft.aggRefreshing, variant: 'subtle', onClick: refreshAggregateModels }),
        Button({ label: '重算临期', variant: 'subtle', onClick: recheckAggregate })
      )
    }));

    /* 关着的时候这几个数字是"打开之后会得到什么"，不是"现在有什 么"。
       不写这一句前缀，页面上会同时出现「未开启」和「5 家参与轮换」，
       读的人只当这两句在打架。 */
    const lead = enabled ? '' : '打开后：';
    const counts = [
      lead + asNumber(pick(totals, 'participatingChannels', pick(totals, 'wiredChannels', 0)), 0) + ' 家接入聚合',
      asNumber(pick(totals, 'virtualModels', 0), 0) + ' 个规范名',
      asNumber(pick(totals, 'candidates', 0), 0) + ' 条候选',
      asNumber(pick(totals, 'expiringSoon', 0), 0) + ' 条三天内作废'
    ];
    const sumRow = h('div', { class: 'agg-sum' });
    counts.forEach((t, i) => {
      const urgent = i === counts.length - 1 && asNumber(pick(totals, 'expiringSoon', 0), 0) > 0;
      sumRow.append(h('span', { class: 'agg-sum-item' + (urgent ? ' is-warn' : ''), text: t }));
    });
    top.body.append(sumRow);
    pages.push(top.node);

    /* 规范名（备用链）排在烧掉顺序之前：这一面的第一件事是"一个模型有哪几条退路"，
       渠道级的先后是支撑它的第二层信息。卡头那条小链就是为了让人不展开也能读到。 */
    if (enabled && virtuals.length) {
      const vmGroup = Group({
        title: '每个模型的备用链（' + virtuals.length + ' 个）',
        desc: '可用模型优先，已启用但暂不可用的随后，全部停用的排最后。点开查看各渠道和积分倍率；停用后会同步到已接入客户端。'
      });
      const search = h('input', { class: 'text-box model-search-input', type: 'search',
        placeholder: '搜索模型名称、ID 或渠道，例如 WorkBuddy', value: draft.aggQuery,
        'aria-label': '搜索聚合模型' });
      const count = h('span', { class: 'model-search-count' });
      const pager = h('div', { class: 'agg-catalog-pager' });
      const results = h('div', { class: 'agg-catalog-results' });
      const paint = () => {
        const q = draft.aggQuery.trim().toLowerCase();
        const filtered = q ? virtuals.filter((vm) => {
          const cands = pick(vm, 'candidates', []);
          const haystack = [pick(vm, 'name', ''), pick(vm, 'key', '')];
          cands.forEach((c) => haystack.push(pick(c, 'realId', ''), pick(c, 'realName', ''),
            chanTitle(agg, pick(c, 'channel', ''), ''), pick(c, 'channel', '')));
          return haystack.join(' ').toLowerCase().includes(q);
        }) : virtuals;
        const size = 30;
        const pages = Math.max(1, Math.ceil(filtered.length / size));
        draft.aggPage = Math.max(0, Math.min(draft.aggPage, pages - 1));
        count.textContent = filtered.length + ' / ' + virtuals.length + ' 个模型';
        pager.replaceChildren(
          Button({ label: '上一页', variant: 'subtle', disabled: draft.aggPage === 0,
            onClick: () => { draft.aggPage--; paint(); search.scrollIntoView({ block: 'center' }); } }),
          h('span', { text: '第 ' + (draft.aggPage + 1) + ' / ' + pages + ' 页' }),
          Button({ label: '下一页', variant: 'subtle', disabled: draft.aggPage + 1 >= pages,
            onClick: () => { draft.aggPage++; paint(); search.scrollIntoView({ block: 'center' }); } }));
        results.replaceChildren();
        for (const vm of filtered.slice(draft.aggPage * size, (draft.aggPage + 1) * size)) {
          results.append(aggregateModelCard(agg, vm, order));
        }
        if (!filtered.length) results.append(h('p', { class: 'muted', text: '没有匹配的模型。' }));
      };
      search.addEventListener('input', debounce(() => { draft.aggQuery = search.value; draft.aggPage = 0; paint(); }, 90));
      vmGroup.body.append(h('div', { class: 'model-search' },
        h('span', { class: 'model-search-field' },
          h('span', { class: 'model-search-icon' }, icon('search')), search), count), results, pager);
      paint();
      pages.push(vmGroup.node);
    } else if (enabled) {
      pages.push(Group({
        title: '每个模型的备用链',
        desc: '还没有任何渠道上报模型清单：先在「供应商」面接入，或等账号池拉完目录。'
      }).node);
    }

    /* 烧掉顺序：渠道级的先后。它解释"上面那些链为什么是这个顺序"。 */
    if (order.length) {
      const turn = Group({
        title: '渠道的烧掉顺序（' + order.length + ' 家）',
        desc: '临期升序：越早作废的排越前。手工排过的渠道一律压在没排过的之前——箭头点一次就生效，不会被临期值悄悄覆盖。'
      });
      order.forEach((id, i) => turn.body.append(aggTurnRow(agg, { id }, i, order.length, order)));
      pages.push(turn.node);
    }

    /* 渠道与额度：这一组回答"哪些家根本没资格进轮换"，以及为什么。
       它排在最后——读的人是先想知道"这个模型有几条退路"，再想知道"这家里谁能排上"。 */
    if (channels.length) {
      const idle = channels.filter((c) => !pick(c, 'wired', false));
      const list = Group({
        title: '渠道与额度（' + channels.length + ' 家）',
        desc: idle.length
          ? '点击右侧「参与轮换 / 不参与轮换」切换是否接入聚合，账号和直连不受影响。' + idle.length + ' 家尚未接通轮换，状态仍作说明。'
          : '点击右侧「参与轮换 / 不参与轮换」切换是否接入聚合，账号、直连和逐模型停用记录都会保留。'
      });
      for (const c of channels) {
        const wired = !!pick(c, 'wired', false);
        /* 渠道行和烧掉顺序行共用同一套几何（标 / 名 / 说明 / 右侧状态），
           只是没有名次和箭头：画成两种形状，读的人得学两遍才能横向比。 */
        list.body.append(h('div', { class: 'agg-turn is-chan' + (wired ? '' : ' is-off') },
          chanMark(pick(c, 'id', ''), String(pick(c, 'name', ''))),
          h('div', { class: 'agg-turn-main' },
            h('div', { class: 'agg-turn-name' },
              h('span', { text: String(pick(c, 'name', pick(c, 'id', ''))) }),
              String(pick(c, 'creditUnit', '')) && String(pick(c, 'creditUnit', '')) !== '无'
                ? toneTag(String(pick(c, 'creditUnit', '')), 'tone-mute') : null),
            h('div', { class: 'agg-turn-sub', text: [String(pick(c, 'vendor', '')),
              asNumber(pick(c, 'accounts', 0), 0) + ' 个账号',
              String(pick(c, 'note', ''))].filter(Boolean).join('  ·  ') })),
          h('div', { class: 'agg-turn-right' },
            wired && pick(c, 'routable', true)
              ? aggregateMembershipButton(c)
              : toneTag(wired ? '只能读额度' : '仅直连', 'tone-mute'))));
      }
      pages.push(list.node);
    }

    return h('div', { class: 'page-body', style: 'gap:32px' }, pages);
  }

  async function pullProviderModels() {
    const p = draft.provider;
    if (!p) return;
    if (!p.baseUrl) { bad('先填 Base URL', '拉取模型列表需要端点地址。'); return; }
    const res = await gw('providers/models', {
      id: p.id, baseUrl: p.baseUrl,
      apiKey: p.apiKey || undefined,
      protocol: p.protocol, save: false
    });
    if (!res.ok) { bad('拉取模型失败', res.error); return; }
    const d = res.data || {};
    draft.catalog = Array.isArray(pick(d, 'catalog', [])) ? pick(d, 'catalog', []) : [];
    const recommended = Array.isArray(pick(d, 'recommended', [])) ? pick(d, 'recommended', []) : [];
    if (!p.models.length || p.id === '') p.models = recommended.slice();
    ok('拉到 ' + draft.catalog.length + ' 个模型', recommended.length
      ? '已按推荐勾选 ' + recommended.length + ' 个，可以再手动调整。' : '请勾选要暴露的模型。');
    render();
  }

  async function saveProvider() {
    const p = draft.provider;
    if (!p) return;
    if (!p.name || !p.baseUrl) { bad('名称和 Base URL 都要填', ''); return; }
    const body = {
      id: p.id, name: p.name, baseUrl: p.baseUrl, protocol: p.protocol,
      enabled: p.enabled, models: p.models
    };
    if (p.apiKey) body.apiKey = p.apiKey;
    if (draft.catalog) body.catalog = draft.catalog;
    const res = await gw('providers', body);
    if (!res.ok) { bad('供应商未保存', res.error); return; }
    draft.provider = null;
    draft.catalog = null;
    await load();
    render();
    ok('供应商已保存', String(pick(res.data || {}, 'id', '')) || '已写入网关');
  }

  async function toggleProvider(id, pv, enabled) {
    const res = await gw('providers', {
      id, name: String(pick(pv, 'name', '')), baseUrl: String(pick(pv, 'baseUrl', '')),
      protocol: String(pick(pv, 'protocol', 'openai')), enabled: !!enabled
    });
    if (!res.ok) { bad('状态未保存', res.error); await load(); render(); return; }
    await load();
    render();
  }

  async function refreshProviderModels(id) {
    const res = await gw('providers/models', { id });
    if (!res.ok) { bad('刷新模型失败', res.error); return; }
    const d = res.data || {};
    ok('模型已刷新', '保留 ' + asNumber(pick(d, 'models', []).length, 0) + ' 个仍在线的勾选');
    await load();
    render();
  }

  async function deleteProvider(id, name) {
    const yes = await confirmDialog('删除供应商', '将删除「' + name + '」及其模型勾选，网关会立刻停止为它提供服务。');
    if (!yes) return;
    const res = await gw('providers/delete', { id });
    if (!res.ok) { bad('删除失败', res.error); return; }
    await load();
    render();
    ok('已删除', name);
  }

  /* ------------------------------------------------------------ 面：联网 */

  function subscriptionDraft() {
    if (!draft.subs) {
      const subs = pick(state, 'subscriptions', {}) || {};
      const items = Array.isArray(pick(subs, 'items', [])) ? pick(subs, 'items', []) : [];
      draft.subs = {
        enabled: !!pick(subs, 'enabled', false),
        items: items.map((it) => ({
          id: String(pick(it, 'id', '')),
          name: String(pick(it, 'name', '')),
          url: String(pick(it, 'url', '')),
          enabled: !!pick(it, 'enabled', true)
        }))
      };
      if (!draft.subs.items.length) draft.subs.items.push({ id: '', name: '', url: '', enabled: true });
    }
    return draft.subs;
  }

  function buildNetworkFace() {
    const settings = pick(state, 'settings', {}) || {};
    const egress = pick(state, 'egress', {}) || {};
    /* 出口探测是异步的：首帧渲染时 state.egress 往往还是空的，卡片就会写死成
       "还没探测到出口 IP"。这里让本轮刚测到的结果顶上来，界面不再比后端更旧。 */
    const probedIp = (draft.proxyTest && draft.proxyTest.ok && draft.proxyTest.egressIP) || '';
    const subs = pick(state, 'subscriptions', {}) || {};
    const singbox = pick(subs, 'singbox', {}) || {};
    const nodes = Array.isArray(pick(singbox, 'nodes', [])) ? pick(singbox, 'nodes', []) : [];

    const proxy = Group({ title: '出口与代理', desc: '这里决定网关的请求从哪个 IP 出去，也就决定了免费额度的归属。' });

    proxy.body.append(Card({
      icon: 'radar',
      title: '当前出口',
      desc: (egress.ip
        ? egress.ip + (egress.country ? '（' + egress.country + '）' : '') + '  ·  模式 ' + String(pick(settings, 'proxyMode', 'env'))
        : (probedIp ? '本轮测到 ' + probedIp + '  ·  尚未写入网关状态' : '还没探测到出口 IP')),
      right: Button({ label: '重新探测', variant: 'subtle', onClick: () => testProxy() })
    }));

    proxy.body.append(Card({
      icon: 'route',
      title: '代理模式',
      desc: 'custom 要填地址；rotate 由订阅轮换决定出口，需要先配好订阅。',
      right: Combo({
        options: PROXY_MODES,
        value: String(pick(settings, 'proxyMode', 'env')),
        onChange: (v) => setSetting('proxyMode', v, '代理模式未保存')
      })
    }));

    proxy.body.append(Card({
      icon: 'link',
      title: '自定义代理地址',
      desc: '仅在模式为「自定义代理」时生效，形如 http://127.0.0.1:7890 或 socks5://…',
      width: 'full',
      right: hrow(
        textField({
          value: String(pick(settings, 'proxyUrl', '')),
          placeholder: 'http://127.0.0.1:7890',
          width: 260,
          mono: true,
          onChange: (v) => setSetting('proxyUrl', v, '代理地址未保存')
        }),
        Button({ label: '测试', variant: 'standard', onClick: () => testProxy() })
      )
    }));

    if (draft.proxyTest) {
      const t = draft.proxyTest;
      proxy.body.append(Card({
        icon: t.ok ? 'check' : 'warning',
        title: t.ok ? '代理可用' : '代理不通',
        desc: (t.latencyMs != null ? '延迟 ' + t.latencyMs + 'ms  ·  ' : '')
          + (t.status ? 'HTTP ' + t.status + '  ·  ' : '')
          + (t.egressIP ? '出口 ' + t.egressIP + (t.egressCountry ? '（' + t.egressCountry + '）' : '') : '')
          + (t.error ? t.error : '')
      }));
    }

    proxy.body.append(Card({
      icon: 'globe',
      title: '向上游暴露地区',
      desc: '把本机地区带给上游，部分模型按地区开放。',
      right: Toggle({
        checked: !!pick(settings, 'exposeRegion', false),
        onChange: (v) => setSetting('exposeRegion', v, '地区暴露未保存')
      })
    }));

    const subGroup = Group({ title: '订阅轮换', desc: 'sing-box 侧车把订阅里的节点变成可轮换的本地出口，一次请求换一个 IP。' });

    subGroup.body.append(h('div', { class: 'card gw-block' },
      (() => {
        const s = subscriptionDraft();
        const wrap = h('div', { class: 'gw-form' });
        wrap.append(h('div', { class: 'gw-inline', style: 'justify-content:space-between' },
          h('div', { class: 'gw-form-row' },
            h('div', { class: 'gw-label', text: '启用订阅轮换' }),
            h('div', { class: 'gw-hint', text: '开启后会立刻抓取一次并启动侧车。' })),
          Toggle({
            checked: s.enabled,
            onChange: (v) => { s.enabled = v; saveSubs(); }
          })
        ));

        s.items.forEach((it, i) => {
          wrap.append(h('div', { class: 'gw-inline' },
            textField({ value: it.name, placeholder: '名称（可留空）', width: 120,
              onChange: (v) => { it.name = v; } }),
            textField({ value: it.url, placeholder: '订阅地址（http / https / 直接节点）', width: 320, mono: true,
              onChange: (v) => { it.url = v; } }),
            Toggle({ checked: it.enabled, onChange: (v) => { it.enabled = v; } }),
            Button({ label: '移除', variant: 'subtle', onClick: () => {
              s.items.splice(i, 1);
              if (!s.items.length) s.items.push({ id: '', name: '', url: '', enabled: true });
              render();
            } })
          ));
        });

        wrap.append(h('div', { class: 'gw-inline' },
          Button({ label: '再加一条', variant: 'subtle', icon: 'add', onClick: () => {
            s.items.push({ id: '', name: '', url: '', enabled: true });
            render();
          } }),
          Button({ label: '保存订阅', variant: 'accent', onClick: saveSubs }),
          Button({ label: '重新抓取', variant: 'standard', icon: 'sync', onClick: refreshSubs })
        ));
        return wrap;
      })()
    ));

    subGroup.body.append(Card({
      icon: 'shuffle',
      title: 'sing-box 侧车',
      desc: (pick(singbox, 'running', false) ? '运行中' : '未运行')
        + '  ·  ' + nodes.length + ' 个节点，' + asNumber(pick(singbox, 'alive', 0), 0) + ' 个存活'
        + (pick(singbox, 'version', '') ? '  ·  版本 ' + String(pick(singbox, 'version', '')) : '')
        + (pick(singbox, 'lastErr', '') ? '  ·  上次错误：' + String(pick(singbox, 'lastErr', '')) : ''),
      expandable: nodes.length > 0,
      right: hrow(
        Button({ label: '指定路径', variant: 'subtle', onClick: pickSingBoxPath }),
        Button({ label: '自动下载', variant: 'subtle', onClick: downloadSingBox })
      ),
      content: (() => {
        const box = h('div', { class: 'row-list' });
        for (const n of nodes) {
          const alive = !!pick(n, 'alive', false);
          box.append(h('div', { class: 'gw-inline', style: 'justify-content:space-between' },
            h('div', { class: 'gw-form-row' },
              h('div', { class: 'gw-label', text: String(pick(n, 'name', pick(n, 'id', '')) ) }),
              h('div', { class: 'gw-hint', text: String(pick(n, 'proto', ''))
                + (pick(n, 'country', '') ? '  ·  ' + String(pick(n, 'country', '')) : '')
                + (pick(n, 'ip', '') ? '  ·  ' + String(pick(n, 'ip', '')) : '')
                + (asNumber(pick(n, 'latencyMs', 0), 0) ? '  ·  ' + asNumber(pick(n, 'latencyMs', 0), 0) + 'ms' : '')
                + (pick(n, 'port', 0) ? '  ·  本地端口 ' + asNumber(pick(n, 'port', 0), 0) : '') })
            ),
            toneTag(alive ? '存活' : '不可用', alive ? 'tone-ok' : 'tone-bad')
          ));
        }
        return box;
      })()
    }));

    subGroup.body.append(Card({
      icon: 'terminal',
      title: 'sing-box 路径',
      desc: String(pick(singbox, 'path', '')) || '（没有指定，自动下载会放到网关数据目录）'
    }));

    return h('div', { class: 'page-body', style: 'gap:32px' }, proxy.node, subGroup.node);
  }

  async function testProxy() {
    const settings = pick(state, 'settings', {}) || {};
    const res = await gw('proxy-test', {
      mode: String(pick(settings, 'proxyMode', '')),
      url: String(pick(settings, 'proxyUrl', ''))
    });
    const d = res.data || {};
    /* 代理测试用 200 + ok:false 表达"这条路走不通"，所以这里要按 data.ok 判断，
       不能只看信封。 */
    draft.proxyTest = {
      ok: !!pick(d, 'ok', res.ok),
      latencyMs: asNumber(pick(d, 'latencyMs', 0), 0),
      status: asNumber(pick(d, 'status', 0), 0),
      egressIP: String(pick(d, 'egressIP', '')),
      egressCountry: String(pick(d, 'egressCountry', '')),
      error: String(pick(d, 'error', res.ok ? '' : (res.error || '')))
    };
    if (!res.ok && !d.error) draft.proxyTest.error = res.error || '';
    render();
  }

  async function saveSubs() {
    const s = subscriptionDraft();
    const items = s.items
      .filter((it) => String(it.url || '').trim() !== '')
      .map((it) => ({ id: it.id, name: it.name, url: it.url.trim(), enabled: !!it.enabled }));
    const res = await gw('subs/save', { enabled: !!s.enabled, items });
    if (!res.ok) { bad('订阅未保存', res.error); return; }
    draft.subs = null;
    await load();
    render();
    ok('订阅已保存', s.enabled ? '正在抓取并启动侧车，稍后刷新看节点。' : '轮换已关闭。');
    if (s.enabled) setTimeout(() => { load().then(render).catch((e) => console.error(e)); }, 3000);
  }

  async function refreshSubs() {
    const res = await gw('subs/refresh', {});
    if (!res.ok) { bad('重新抓取失败', res.error); return; }
    ok('已开始重新抓取', '抓取 → 解析 → 重启侧车 → 探测，大约十几秒。');
    setTimeout(() => { load().then(render).catch((e) => console.error(e)); }, 4000);
  }

  async function pickSingBoxPath() {
    const picked = await host(M.pickOpen, { prompt: '选择 sing-box.exe' });
    if (!picked.ok) { bad('无法选择文件', picked.error || ''); return; }
    const d = picked.data || {};
    if (d.canceled) return;
    const res = await gw('subs/singbox', { path: String(d.path || '') });
    if (!res.ok) { bad('路径未保存', res.error); return; }
    await load();
    render();
    ok('已指定 sing-box', tail(d.path, 60));
  }

  async function downloadSingBox() {
    const yes = await confirmDialog('下载 sing-box', '会从官方发布页下载当前平台版本到网关数据目录。');
    if (!yes) return;
    const res = await gw('subs/singbox', { download: true });
    if (!res.ok) { bad('下载失败', res.error); return; }
    ok('已开始下载', '完成后重新抓取订阅即可启动侧车。');
  }

  /* ------------------------------------------------------------ 面：Agent */

  async function loadQoder() {
    const res = await gw('qoder/status');
    if (!res.ok) { draft.qoder = { error: res.error }; return; }
    draft.qoder = res.data || {};
  }

  async function loadWorkBuddy() {
    const res = await gw('workbuddy/status');
    if (!res.ok) { draft.wb = { error: res.error }; return; }
    draft.wb = res.data || {};
  }

  function buildAgentsFace() {
    const agents = Array.isArray(pick(state, 'agents', [])) ? pick(state, 'agents', []) : [];
    const agentKeys = pick(state, 'agentKeys', {}) || {};
    const settings = pick(state, 'settings', {}) || {};

    const aggregateOn = !!pick(settings, 'aggregateEnabled', false);
    const group = Group({
      title: 'Agent 接入',
      desc: '打开后对应 Agent 的配置文件里出现网关的供应商；改动前会先备份，关掉就还原。'
    });

    /* 注入进 Agent 的那张清单随聚合开关变：开着时只剩聚合规范名，关掉才逐家
       列出。这是用户此刻最需要知道的一句——否则他打开 Agent 的选择器，看到的
       与自己刚在「模型」面勾选的东西不是同一批，而页面上没有任何一处解释。 */
    group.body.append(Card({
      icon: 'route',
      width: 'full',
      title: aggregateOn ? '注入给 Agent 的是聚合规范名' : '注入给 Agent 的是逐家模型',
      desc: aggregateOn
        ? '聚合开着：Agent 的选择器里只有 aggregate/auto 与各聚合规范名，'
          + '供应商的真实 id 不再单独列出。关掉聚合后恢复逐家列出。'
        : '聚合关掉：每个供应商的每个模型各自一条，Agent 的选择器按家逐一列出。'
          + '打开聚合后只会留下聚合规范名。',
      right: hrow(Button({
        label: '去聚合面',
        variant: 'subtle',
        icon: 'route',
        onClick: () => { if (ctx.navigate) ctx.navigate('gw:aggregate'); }
      }))
    }));

    if (!agents.length) {
      group.body.append(Card({ icon: 'info', title: '没有检测到可接入的 Agent', desc: '' }));
    }

    for (const a of agents) {
      const id = String(pick(a, 'id', ''));
      const name = String(pick(a, 'name', id));
      const installed = !!pick(a, 'installed', false);
      const enabled = !!pick(a, 'enabled', false);
      const detail = String(pick(a, 'detail', ''));
      const version = String(pick(a, 'version', ''));
      const warn = String(pick(a, 'warn', ''));
      const key = String(pick(agentKeys, id, ''));

      group.body.append(Card({
        /* Agent 卡片左边是它自己的标：ZCode 的方块、Claude 的星芒、DSH 的
           波浪……认不出来才退回通用小人。 */
        brand: id,
        icon: 'profile',
        title: name + (version ? '  ' + version : ''),
        desc: (detail || (installed ? '已检测到配置文件' : '未安装'))
          + (warn ? '  ·  ' + warn : ''),
        right: Toggle({
          checked: enabled,
          disabled: !installed,
          onChange: (v) => setAgent(id, v)
        }),
        expandable: installed,
        content: key ? (() => {
          const box = h('div', { class: 'row-list', style: 'gap:8px' });
          box.append(h('div', { class: 'gw-label', text: '这个 Agent 独立使用的 Key' }));
          box.append(h('div', { class: 'gw-inline' },
            h('span', { class: 'gw-hint mono', style: 'flex:1', text: tail(key, 24) }),
            Button({ label: '复制', variant: 'subtle', onClick: () => copyText(key, name + ' 的 Key 已复制') })
          ));
          box.append(h('div', { class: 'gw-hint',
            text: 'Agent 用这条 Key 访问网关时，用量会单独记在它名下，方便分辨是谁在花额度。' }));
          return box;
        })() : null
      }));
    }

    /* WorkBuddy：账号渠道 —— 登录态、额度、到期时间都在网关里，不在这里估算 */
    const wb = draft.wb;
    const wbOn = !!pick(settings, 'workBuddyEnabled', false);
    const wbGroup = Group({
      title: 'WorkBuddy 账号渠道',
      desc: '账号登录与模型接入分别控制：登录后仍需开启下方开关，模型才会进入聚合和客户端。'
    });

    wbGroup.body.append(Card({
      brand: 'workbuddy',
      icon: 'profile',
      title: '启用 WorkBuddy 渠道',
      desc: '关闭后这批模型从所有选择器里消失，账号本身不会被清掉。',
      right: Toggle({
        checked: wbOn,
        onChange: (v) => setSetting('workBuddyEnabled', v, 'WorkBuddy 开关未保存')
      })
    }));

    if (!wb) {
      wbGroup.body.append(Card({
        icon: 'sync',
        title: '读取账号状态',
        desc: '登录态不在 state 里，点一次去网关拿。',
        right: Button({ label: '读取', variant: 'subtle', onClick: () => { loadWorkBuddy().then(render).catch((e) => console.error(e)); } })
      }));
    } else if (wb.error) {
      wbGroup.body.append(Card({ icon: 'warning', title: '读取失败', desc: String(wb.error) }));
    } else {
      const logged = !!pick(wb, 'loggedIn', false);
      wbGroup.body.append(Card({
        brand: 'workbuddy',
        icon: logged ? 'check' : 'profile',
        title: logged ? '已登录 · ' + (wbOn ? '模型接入已开启' : '模型接入已关闭') + (pick(wb, 'nickname', '') ? '：' + String(pick(wb, 'nickname', '')) : '') : '未登录',
        desc: logged
          ? '到期 ' + stamp(pick(wb, 'expiresAtMs', 0))
            + (pick(wb, 'lastActivityDate', '') ? '  ·  最近活跃 ' + String(pick(wb, 'lastActivityDate', '')) : '')
            + (pick(wb, 'credits', '') ? '  ·  额度 ' + String(pick(wb, 'credits', '')) : '')
            + (pick(wb, 'detail', '') ? '  ·  ' + String(pick(wb, 'detail', '')) : '')
          : '还没有登录。点右侧在默认浏览器里打开授权页。',
        right: hrow(
          logged
            ? Button({ label: '退出', variant: 'critical', onClick: wbLogout })
            : Button({ label: '登录', variant: 'accent', onClick: wbLogin }),
          logged ? null : Button({ label: '刷新状态', variant: 'subtle', onClick: () => { loadWorkBuddy().then(render).catch((e) => console.error(e)); } })
        )
      }));

      if (logged) {
        wbGroup.body.append(Card({
          icon: 'sync',
          title: '推进登录 / 刷新',
          desc: '登录流程每点一次推进一步；已登录时用来刷新额度读数。',
          right: Button({ label: '推进一步', variant: 'subtle', onClick: wbPoll })
        }));
      }
    }

    /* Qoder：内置转发层。登录在「渠道」面完成，凭据进凭据表；这一面只报它接没接上。
       ⚠️ 这里曾经写着「登录状态跟随本机的 Qoder IDE」—— 那是内置登录之前的事实，
         留着会让用户以为必须装 IDE 才能用，而他现在可以在渠道面自己登录。 */
    const q = draft.qoder;
    const qGroup = Group({
      title: 'Qoder 转发层',
      desc: '渠道面登录后由这里把模型接进目录；IDE 里那份缓存只是没登录过时的兜底。'
    });
    if (!q) {
      qGroup.body.append(Card({
        icon: 'sync',
        title: '读取 Qoder 状态',
        desc: '转发层是否在跑、有没有可用凭据、接了几个模型，都要问网关。',
        right: Button({ label: '读取', variant: 'subtle', onClick: () => { loadQoder().then(render).catch((e) => console.error(e)); } })
      }));
    } else if (q.error) {
      qGroup.body.append(Card({ icon: 'warning', title: '读取失败', desc: String(q.error) }));
    } else {
      const models = Array.isArray(pick(q, 'models', [])) ? pick(q, 'models', []) : [];
      qGroup.body.append(Card({
        brand: 'qoder',
        icon: 'profile',
        title: (pick(q, 'running', false) ? '转发层运行中' : '转发层未运行')
          + (pick(q, 'loggedIn', false) ? '，凭据可用' : '，没有可用凭据'),
        desc: String(pick(q, 'detail', ''))
          + (pick(q, 'providerWired', false) ? '  ·  已接入网关目录' : '  ·  未接入')
          + (models.length ? '  ·  ' + models.length + ' 个模型' : ''),
        right: hrow(
          Button({ label: '重新探测', variant: 'subtle', onClick: () => { loadQoder().then(render).catch((e) => console.error(e)); } }),
          /* 入口直接指向渠道面那一行的「登录」：这一面不重复实现登录，
             只负责把人送到真正能动手的那一处。 */
          Button({ label: '去登录', variant: 'subtle', onClick: () => {
            if (ctx.navigate) ctx.navigate('gw:channels');
          } })
        )
      }));
    }

    return h('div', { class: 'page-body', style: 'gap:32px' }, group.node, wbGroup.node, qGroup.node);
  }

  async function setAgent(id, enabled) {
    const res = await gw('agent/' + encodeURIComponent(id) + '/' + (enabled ? 'enable' : 'disable'), {});
    if (!res.ok) {
      bad((enabled ? '开启' : '关闭') + '失败', res.error);
      await load();
      render();
      return;
    }
    await load();
    render();
  }

  async function wbLogin() {
    const res = await gw('workbuddy/login/start', {});
    if (!res.ok) { bad('登录未开始', res.error); return; }
    ok('授权页已在默认浏览器打开', '授权完成后点「推进一步」把登录态写回网关。');
    await loadWorkBuddy();
    render();
  }

  async function wbPoll() {
    const res = await gw('workbuddy/login/poll');
    if (!res.ok) {
      bad('推进登录失败', res.error);
      await loadWorkBuddy();
      render();
      return;
    }
    const st = String(pick(res.data || {}, 'state', ''));
    await loadWorkBuddy();
    render();
    if (st === 'ok') ok('账号已登录', '这一批模型已经进入目录，已开启的 Agent 会自动重新注入。');
    else ok('等待授权', '当前状态：' + (st || 'pending') + '。授权完成后要退出浏览器再回来点一次。');
  }

  async function wbLogout() {
    const yes = await confirmDialog('退出 WorkBuddy', '账号会从网关里清掉，这批模型立刻退出目录。');
    if (!yes) return;
    const res = await gw('workbuddy/logout', {});
    if (!res.ok) { bad('退出失败', res.error); return; }
    draft.wb = null;
    await load();
    await loadWorkBuddy();
    render();
    ok('已退出账号', '');
  }

  /* ------------------------------------------------------------ 面：日志 / 设置 */

  function buildLogFace() {
    return h('div', { class: 'page-body', style: 'gap:32px' }, buildLogViewer());
  }

  function buildSettingsFace() {
    const settings = pick(state, 'settings', {}) || {};
    const cats = pick(settings, 'logCategories', {}) || {};

    const settingsGroup = Group({ title: '模型与请求', desc: '这些是网关每一轮请求的默认值。' });
    settingsGroup.body.append(Card({
      icon: 'tokens',
      title: '默认 max_tokens',
      desc: '调用方没有显式带 max_tokens 时用这个数。',
      right: NumberBox({
        value: asNumber(pick(settings, 'defaultMaxTokens', 32768), 32768),
        min: 256, max: 200000, step: 256,
        onChange: (v) => setSetting('defaultMaxTokens', Math.round(v), '默认 max_tokens 未保存')
      })
    }));
    settingsGroup.body.append(Card({
      icon: 'levels',
      title: '默认思考档',
      desc: 'light / balanced / deep 对应三档 token 预算。',
      right: Combo({
        options: EFFORT_OPTIONS,
        value: String(pick(settings, 'defaultEffort', 'balanced')),
        onChange: (v) => setSetting('defaultEffort', v, '默认思考档未保存')
      })
    }));
    settingsGroup.body.append(Card({
      icon: 'radar',
      title: '自动探测间隔',
      desc: '每隔这么多分钟把免费车道重测一轮；太小会一直打上游。',
      right: NumberBox({
        value: asNumber(pick(settings, 'probeIntervalMinutes', 15), 15),
        min: 1, max: 1440, step: 1, unit: '分钟',
        onChange: (v) => setSetting('probeIntervalMinutes', Math.round(v), '探测间隔未保存')
      })
    }));

    const routeGroup = Group({ title: '失败转移与路由', desc: '一个模型拒绝之后，网关按这里的规则挑下一个。' });
    routeGroup.body.append(Card({
      icon: 'shuffle',
      title: '失败转移',
      desc: '请求被上游拒绝时自动换下一个模型重试，客户端只会看到一次成功。',
      right: Toggle({
        checked: !!pick(settings, 'failoverEnabled', false),
        onChange: (v) => setSetting('failoverEnabled', v, '失败转移未保存')
      })
    }));
    routeGroup.body.append(Card({
      icon: 'repeat',
      title: '最多转移次数',
      desc: '一轮请求里最多换几个模型，1 到 5。',
      right: NumberBox({
        value: asNumber(pick(settings, 'failoverMax', 1), 1),
        min: 1, max: 5, step: 1,
        onChange: (v) => setSetting('failoverMax', Math.round(v), '转移次数未保存')
      })
    }));
    routeGroup.body.append(Card({
      icon: 'route',
      title: '智能路由',
      desc: '按能力与延迟过滤候选；关掉就回到最原始的目录顺序。',
      right: Toggle({
        checked: !!pick(settings, 'smartRouting', false),
        onChange: (v) => setSetting('smartRouting', v, '智能路由未保存')
      })
    }));
    routeGroup.body.append(Card({
      icon: 'list',
      title: '路由策略',
      desc: '目录顺序保底稳定；首字延迟优先在可用候选里挑最快的一个。',
      right: Combo({
        options: ROUTING_OPTIONS,
        value: String(pick(settings, 'routingStrategy', 'catalog')),
        onChange: (v) => setSetting('routingStrategy', v, '路由策略未保存')
      })
    }));
    routeGroup.body.append(Card({
      icon: 'key',
      title: '免费车道耗尽后回落自有 Key',
      desc: '关闭时免费车道没额度就直接报错，不会花自己的 Key。',
      right: Toggle({
        checked: !!pick(settings, 'laneFallbackToProviders', false),
        onChange: (v) => setSetting('laneFallbackToProviders', v, '回落开关未保存')
      })
    }));

    const sourceGroup = Group({ title: '目录与来源', desc: '模型能力标签和容量数据的补充来源。' });
    sourceGroup.body.append(Card({
      icon: 'sparkle',
      title: 'AI 自动标注',
      desc: '用模型给新出现的模型打能力标签，省得手动勾。',
      right: Toggle({
        checked: !!pick(settings, 'autoTagEnabled', false),
        onChange: (v) => setSetting('autoTagEnabled', v, 'AI 标注开关未保存')
      })
    }));
    sourceGroup.body.append(Card({
      icon: 'database',
      title: 'OpenRouter 补容量',
      desc: '从公开目录补齐上下文窗口等数字；打开会立刻拉一次。',
      right: Toggle({
        checked: !!pick(settings, 'openRefEnabled', false),
        onChange: (v) => setSetting('openRefEnabled', v, 'OpenRef 开关未保存')
      })
    }));
    /* 这三条的值是 URL，不是开关。放进卡片的 right 槽（260px）时，左边说明列
       被挤到 200px，一条 300 字符的地址就折成每行十几个字母的竖条 —— 截图里
       那三坨竖排字就是它。所以它们走 .gw-field-card：标题和说明占一整行，
       输入框另起一行铺满整格，三条并排时左边缘对齐。 */
    sourceGroup.body.append(fieldCard({
      icon: 'download',
      title: '更新源',
      desc: '版本清单的地址。留空就用内置的 GitHub 发布页。',
      value: String(pick(settings, 'updateFeed', '')),
      placeholder: '留空用内置地址',
      onChange: (v) => setSetting('updateFeed', v, '更新源未保存')
    }));
    sourceGroup.body.append(fieldCard({
      icon: 'megaphone',
      title: '公告源',
      desc: '公告 JSON 的地址，支持 GitHub Contents API 那种 base64 信封。',
      value: String(pick(settings, 'announcementFeed', '')),
      placeholder: '留空用内置地址',
      onChange: (v) => setSetting('announcementFeed', v, '公告源未保存')
    }));
    sourceGroup.body.append(fieldCard({
      icon: 'chart',
      title: '统计上报地址',
      desc: '用量发往的地址。留空就只在本地记账，不发出去。',
      value: String(pick(settings, 'statsServerUrl', '')),
      placeholder: '留空表示不上报',
      onChange: (v) => setSetting('statsServerUrl', v, '统计上报地址未保存')
    }));

    /* 网关原版是托盘程序：关掉管理页它还在后台提供服务。这个窗口不是——它自己
       就是网关进程，关窗就是退出，所以「关闭到托盘」在这里是空开关，不给它位置。 */
    const shellGroup = Group({ title: '网关行为', desc: '这些是网关程序自己的习惯，和模型无关。' });
    shellGroup.body.append(Card({
      icon: 'bell',
      title: '桌面通知',
      desc: '模型被限流、额度到顶这类事件弹一条系统通知。',
      right: Toggle({
        checked: !!pick(settings, 'notifications', false),
        onChange: (v) => setSetting('notifications', v, '通知开关未保存')
      })
    }));

    const catGroup = Group({
      title: '日志类别',
      desc: '关掉的类别完全不写文件；upstream 与 content 默认关闭，因为量大且可能带请求正文。'
    });
    for (const c of LOG_CATS) {
      catGroup.body.append(Card({
        icon: CAT_ICON[c.id] || 'file',
        title: c.label,
        desc: '类别 ' + c.id,
        right: Toggle({
          checked: !!pick(cats, c.id, false),
          onChange: (v) => {
            const next = {};
            for (const k of Object.keys(cats)) next[k] = !!cats[k];
            next[c.id] = v;
            setSetting('logCategories', next, '日志类别未保存');
          }
        })
      }));
    }
    catGroup.body.append(Card({
      icon: 'levels',
      title: '文件日志级别',
      desc: '低于这个级别的行只留在内存环形缓冲里。',
      right: Combo({
        options: LOG_LEVEL_OPTIONS,
        value: String(pick(settings, 'logLevel', 'debug')),
        onChange: (v) => setSetting('logLevel', v, '日志级别未保存')
      })
    }));
    catGroup.body.append(Card({
      icon: 'calendar',
      title: '日志保留天数',
      desc: '超出的每日文件会在启动时清掉，1 到 365。',
      right: NumberBox({
        value: asNumber(pick(settings, 'logKeepDays', 7), 7),
        min: 1, max: 365, step: 1, unit: '天',
        onChange: (v) => setSetting('logKeepDays', Math.round(v), '保留天数未保存')
      })
    }));

    return h('div', { class: 'page-body', style: 'gap:32px' },
      settingsGroup.node,
      routeGroup.node,
      sourceGroup.node,
      shellGroup.node,
      catGroup.node);
  }

  function buildLogViewer() {
    const q = draft.logQuery;
    const group = Group({
      title: '日志查看器',
      desc: '读的是网关自己的日志：不传 day 就取内存环形缓冲，传了就取那一天的每日文件。'
    });

    const catRow = h('div', { class: 'gw-inline' });
    for (const c of LOG_CATS) {
      const on = q.cats.indexOf(c.id) >= 0;
      /* 标签上带一个类别图标：七类都是两个汉字的词，光看字形要一行行读；
         图标先给出"这是谁在说话"，再读词。 */
      const btn = h('button', {
        class: 'gw-tab' + (on ? ' is-selected' : ''), type: 'button'
      }, h('span', { class: 'gw-tab-icon' }, icon(CAT_ICON[c.id] || 'file')), c.label);
      btn.addEventListener('click', () => {
        if (q.cats.indexOf(c.id) >= 0) q.cats = q.cats.filter((x) => x !== c.id);
        else q.cats.push(c.id);
        render();
        loadLogs();
      });
      catRow.append(btn);
    }

    const filterRow = h('div', { class: 'gw-inline', style: 'padding:8px 0' },
      Combo({
        options: [{ label: '全部级别', value: '' }].concat(LOG_LEVEL_OPTIONS),
        value: q.level,
        onChange: (v) => { q.level = v; render(); loadLogs(); }
      }),
      textField({
        value: q.q, placeholder: '关键词（包含匹配）', width: 200,
        onChange: (v) => { q.q = v; loadLogs(); }
      }),
      Button({ label: '刷新', variant: 'standard', icon: 'sync', onClick: loadLogs }),
      Button({ label: '导出文本', variant: 'subtle', onClick: exportLogs })
    );

    const dayRow = h('div', { class: 'gw-inline' });
    const days = Array.isArray(draft.logDays) ? draft.logDays : [];
    if (days.length) {
      dayRow.append(Combo({
        options: [{ label: '内存缓冲（最近）', value: '' }].concat(days.map((d) => ({ label: d, value: d }))),
        value: q.day,
        onChange: (v) => { q.day = v; render(); loadLogs(); }
      }));
    }

    group.body.append(h('div', { class: 'card gw-block' },
      h('div', { class: 'gw-form' },
        h('div', { class: 'gw-label', text: '按类别筛选（可多选）' }),
        catRow, filterRow, dayRow)));

    /* 日志正文 */
    const entries = draft.logs;
    if (!entries) {
      group.body.append(h('div', { class: 'gw-log' },
        h('div', { class: 'gw-log-empty', text: '还没有读取。点「刷新」或改一个筛选条件就会拉取。' })));
    } else {
      const list = h('div', { class: 'gw-log' });
      if (!entries.length) {
        list.append(h('div', { class: 'gw-log-empty', text: '没有匹配的行。' }));
      }
      for (const e of entries) {
        const lv = String(pick(e, 'level', 'info')).toLowerCase();
        const cat = String(pick(e, 'cat', ''));
        list.append(h('div', { class: 'gw-log-row lv-' + lv },
          h('span', { class: 'log-at', text: clock(pick(e, 'at', '')) }),
          h('span', { class: 'log-cat', text: cat || '—' }),
          h('span', { class: 'log-msg', text: String(pick(e, 'msg', '')) })
        ));
      }
      group.body.append(list);
    }
    return group.node;
  }

  function logQueryString(extra) {
    const q = draft.logQuery;
    const parts = [];
    if (q.cats.length) parts.push('cat=' + encodeURIComponent(q.cats.join(',')));
    if (q.level) parts.push('level=' + encodeURIComponent(q.level));
    if (q.q) parts.push('q=' + encodeURIComponent(q.q));
    if (q.day) parts.push('day=' + encodeURIComponent(q.day));
    if (extra) parts.push(extra);
    return parts.length ? 'logs?' + parts.join('&') : 'logs';
  }

  /* 验收钩子:让外部脚本读到这一页的内部状态,不必靠像素猜。只在 ZEN_UI_DIAG
     开着的构建里装,正常运行时它就是一次函数定义,没有任何开销。 */
  window.__gwDebug = () => ({
    face,
    rows: document.querySelectorAll('.gw-log-row').length,
    toggles: document.querySelectorAll('.toggle').length,
    /* 聚合面：这三条数就是"有没有真的画出来"的判据。光看像素分不清
       "卡片少了一张"和"候选链画断了"，而这些能。 */
    aggSteps: document.querySelectorAll('.agg-step').length,
    aggHops: document.querySelectorAll('.agg-hop').length,
    aggExpanded: document.querySelectorAll('.card.agg-vm.is-open').length,
    aggError: draft.aggregateError || '',
    trace: (window.__gwTrace || []).slice(-4)
  });

  async function loadLogs() {
    const my = ++logSeq;
    const res = await gw(logQueryString());
    if (my !== logSeq) return;
    if (!res.ok) { bad('读取日志失败', res.error); return; }
    const d = res.data || {};
    draft.logs = Array.isArray(pick(d, 'entries', [])) ? pick(d, 'entries', []) : [];
    draft.logDays = Array.isArray(pick(d, 'days', [])) ? pick(d, 'days', []) : [];
    render();
  }

  async function exportLogs() {
    const picked = await host(M.pickSave, { prompt: '导出日志', name: 'lamp-guide-log.txt' });
    if (!picked.ok) { bad('无法选择保存位置', picked.error || ''); return; }
    const d = picked.data || {};
    if (d.canceled) return;
    const route = 'logs/download' + (logQueryString().indexOf('?') >= 0
      ? '?' + logQueryString().split('?')[1] : '');
    const res = await gw(route, undefined, { saveTo: String(d.path || '') });
    if (!res.ok) { bad('导出日志失败', res.error); return; }
    const out = res.data || {};
    ok('日志已导出', tail(out.path, 80) + '  ·  ' + asNumber(out.bytes, 0) + ' 字节');
  }

  /* ------------------------------------------------------------ 渲染 */

  function faceNode() {
    switch (face) {
      case 'models': return buildModelsFace();
      case 'aggregate': return buildAggregateFace();
      case 'channels': return buildChannelFace();
      case 'providers': return buildProvidersFace();
      case 'network': return buildNetworkFace();
      case 'agents': return buildAgentsFace();
      case 'logs': return buildLogFace();
      case 'settings': return buildSettingsFace();
      default:
        return h('div', { class: 'page-body', style: 'gap:32px' },
          buildServiceFace(),
          buildEndpointsGroup(),
          buildAnnounceGroup(),
          buildMigrationGroup());
    }
  }

  function render() {
    if (!state) {
      body.replaceChildren(InfoBar({
        severity: 'error',
        title: '网关没有启动',
        message: downReason || '未能与网关通信。'
      }));
      return;
    }
    try {
      body.replaceChildren(faceNode());
    } catch (err) {
      /* 一面在构建时抛错,不能把整个页面留在上一帧——那会让"界面没变"看起来
         像"数据没来"。这里把异常摊开,而不是让页面静止。 */
      body.replaceChildren(InfoBar({
        severity: 'error',
        title: '这一面没能画出来',
        message: String((err && err.message) || err)
      }));
      throw err;
    }
    if (alertBar) body.prepend(alertBar);
    mark('first-card');
  }

  /* typing 判断焦点是否落在可输入的控件里。自动刷新只在"没人正在输入"
     的帧上发生。 */
  function typing() {
    const el = document.activeElement;
    if (!el) return false;
    const tag = String(el.tagName || '').toUpperCase();
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
    return !!(el.isContentEditable);
  }

  /* 首帧常常比网关的状态早一拍：出口探测要一次网络往返，目录与额度也是后台
     落库的。这里在头几秒补几拍，让"还没探测到出口 IP"这种过渡态自己消失，
     而不是等到 30 秒的轮询或者用户手点刷新。 */
  let settleTimers = [];
  /* A slower, earlier log query must never overwrite the newest filter's
     result. Every load bumps the sequence; stale responses are dropped. */
  let logSeq = 0;
  function settle() {
    for (const t of settleTimers) clearTimeout(t);
    const before = {
      ip: String(pick(pick(state, 'egress', {}) || {}, 'ip', '')),
      probing: String(pick(state, 'probingModel', ''))
    };
    settleTimers = [1200, 3500, 8000].map((ms) => setTimeout(() => {
      if (typing()) return;
      load().then((got) => {
        if (!got) return;
        /* 只在"上一帧确实是过渡态"时补画：数据没变就不动，省得把展开器、
           滚动位置这些用户手动造成的状态擦掉。 */
        const ip = String(pick(pick(state, 'egress', {}) || {}, 'ip', ''));
        const probing = String(pick(state, 'probingModel', ''));
        const moved = ip !== before.ip || probing !== before.probing;
        if (moved && face !== 'logs') render();
      }).catch((e) => console.error(e));
    }, ms));
  }

  async function reload() {
    window.__gwTrace = window.__gwTrace || [];
    await load();
    render();
    settle();
    document.dispatchEvent(new CustomEvent('gw-traced'));
  }

  return {
    node: root,
    title: faceMeta.label,
    onMount() {
      hideBar();
      /* 日志面一进来就拉一次：这一面只有日志查看器，空着等于什么都没做。 */
      if (face === 'logs' && !draft.logs) loadLogs();
      /* 聚合面不等 30 秒轮询：临期是"马上要作废"的数字，晚半分钟就换了一种说法。 */
      if (face === 'aggregate') { loadAggregate().then(render).catch((e) => console.error(e)); }
      /* 渠道面一进来就读：登录态是活的，缓存一分钟就会说错「这家还没登录」。 */
      if (face === 'channels') { loadChannels().then(render).catch((e) => console.error(e)); }
      if (face === 'agents') { loadQoder(); loadWorkBuddy(); }
      reload();
      /* 探测与出口检测都是后台跑完才落库的：首帧往往拿到的是空值，页面不刷新
         就永远停在"还没探测到出口 IP"。服务面还挂着运行时长与侧车状态，同样
         是活的数字，所以这几面一起按 30 秒一轮跟状态走。 */
      if (autoRefresh) clearInterval(autoRefresh);
      autoRefresh = setInterval(() => {
        if (face === 'channels' && !typing()) { loadCheckins().then(render).catch((e) => console.error(e)); return; }
        const busy = String(pick(state, 'probingModel', '')) !== '';
        const live = face === 'models' || face === 'logs'
          || face === 'network' || face === 'service';
        /* 有输入框正被操作时不重画：replaceChildren 会把焦点和还没提交的
           字符一起丢掉，用户会觉得自己打的字被吞了。 */
        if ((busy || live) && !typing()) load().then(render).catch((e) => console.error(e));
      }, face === 'channels' ? 5000 : 30000);
    },
    onUnmount() {
      hideBar();
      if (autoRefresh) { clearInterval(autoRefresh); autoRefresh = null; }
      for (const t of settleTimers) clearTimeout(t);
      settleTimers = [];
    }
  };
}



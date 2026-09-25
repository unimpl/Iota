const shell = document.getElementById('shell');
const sessionNav = document.getElementById('sessions');
const sessionCount = document.getElementById('session-count');
const header = document.getElementById('session-header');
const timeline = document.getElementById('timeline');
const connection = document.getElementById('connection');
const toggle = document.getElementById('toggle-sidebar');

let sessions = [];
let selected = null;
let source = null;
let records = [];
let renderPending = false;
const expanded = new Set();

toggle.addEventListener('click', () => {
  const hidden = shell.classList.toggle('sidebar-hidden');
  toggle.setAttribute('aria-expanded', String(!hidden));
  toggle.setAttribute('aria-label', hidden ? '显示 session 列表' : '隐藏 session 列表');
});

function node(tag, className, value) {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (value != null) element.textContent = String(value);
  return element;
}

function formatTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value || '' : date.toLocaleString('zh-CN', { hour12: false });
}

function clockTime(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value || '';
  return date.toLocaleTimeString('zh-CN', { hour12: false }) + '.' + String(date.getMilliseconds()).padStart(3, '0');
}

function setConnection(label, state = '') {
  connection.textContent = label;
  connection.className = 'connection ' + state;
}

function disclosure(label, key, build) {
  const wrap = node('details', 'disclosure');
  wrap.open = expanded.has(key);
  let loaded = false;
  const load = () => {
    if (loaded) return;
    wrap.append(build());
    loaded = true;
  };
  wrap.addEventListener('toggle', () => {
    if (wrap.open) { expanded.add(key); load(); }
    else expanded.delete(key);
  });
  wrap.append(node('summary', '', label));
  if (wrap.open) load();
  return wrap;
}

function textDisclosure(label, value, key) {
  return disclosure(label, key, () => node('pre', '', typeof value === 'string' ? value || '（空）' : JSON.stringify(value, null, 2)));
}

function highlightedJSON(value) {
  const code = node('code', 'json-code');
  const text = JSON.stringify(value, null, 2);
  const tokens = /("(?:\\.|[^"\\])*"(?=\s*:))|("(?:\\.|[^"\\])*")|(-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)|\b(true|false|null)\b|([{}\[\]:,])/g;
  let position = 0;
  for (const match of text.matchAll(tokens)) {
    if (match.index > position) code.append(document.createTextNode(text.slice(position, match.index)));
    const kind = match[1] ? 'key' : match[2] ? 'string' : match[3] ? 'number' : match[4] ? 'literal' : 'punctuation';
    code.append(node('span', 'json-' + kind, match[0]));
    position = match.index + match[0].length;
  }
  if (position < text.length) code.append(document.createTextNode(text.slice(position)));
  return { code, text };
}

function jsonDisclosure(record) {
  return disclosure('查看 JSON · #' + record.seq, 'json-' + record.seq, () => {
    const view = node('div', 'json-view');
    const toolbar = node('div', 'json-toolbar');
    toolbar.append(node('span', 'json-toolbar-label', '事件 #' + record.seq));
    const copy = node('button', 'json-copy');
    copy.type = 'button';
    copy.setAttribute('aria-label', '复制事件 #' + record.seq + ' 的 JSON');
    copy.title = '复制 JSON';
    const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    icon.setAttribute('viewBox', '0 0 24 24');
    icon.setAttribute('fill', 'none');
    icon.setAttribute('stroke', 'currentColor');
    icon.setAttribute('stroke-width', '1.8');
    icon.setAttribute('stroke-linecap', 'round');
    icon.setAttribute('stroke-linejoin', 'round');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'M8 4h10a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2Zm-4 2H3a1 1 0 0 0-1 1v13a2 2 0 0 0 2 2h11');
    icon.append(path);
    const copyLabel = node('span', '', '复制');
    copyLabel.setAttribute('aria-live', 'polite');
    copy.append(icon, copyLabel);
    toolbar.append(copy);
    const { code, text } = highlightedJSON(record);
    copy.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(text);
        copyLabel.textContent = '已复制';
      } catch {
        copyLabel.textContent = '复制失败';
      }
    });
    const pre = node('pre', 'json-pre');
    pre.append(code);
    view.append(toolbar, pre);
    return view;
  });
}

async function refreshSessions() {
  try {
    const response = await fetch('/api/sessions', { cache: 'no-store' });
    if (!response.ok) throw new Error('HTTP ' + response.status);
    sessions = await response.json();
    sessionCount.textContent = String(sessions.length);
    renderSessionList();
    if (!selected && sessions.length) selectSession(sessions[0].name);
    if (selected && !sessions.some(item => item.name === selected)) {
      source?.close();
      source = null;
      selected = null;
      records = [];
      if (sessions.length) selectSession(sessions[0].name);
      else scheduleRender();
    }
  } catch (error) {
    setConnection('无法读取目录：' + error.message, 'error');
  }
}

function renderSessionList() {
  sessionNav.replaceChildren();
  if (!sessions.length) {
    sessionNav.append(node('p', 'side-empty', '暂无 session。运行 iota 后，记录会出现在这里。'));
    return;
  }
  for (const session of sessions) {
    const button = node('button', 'session-item' + (session.name === selected ? ' active' : ''));
    button.type = 'button';
    button.setAttribute('aria-current', session.name === selected ? 'page' : 'false');
    button.append(node('span', 'session-title', session.title || '尚无提问'));
    button.append(node('span', 'session-date', formatTime(session.created_at)));
    button.append(node('span', 'session-meta', (session.model || 'model') + ' · ' + session.name.slice(11, 19)));
    button.addEventListener('click', () => selectSession(session.name));
    sessionNav.append(button);
  }
}

function selectSession(name) {
  if (selected === name) return;
  source?.close();
  selected = name;
  records = [];
  expanded.clear();
  renderSessionList();
  scheduleRender();
  setConnection('正在连接');
  source = new EventSource('/api/events?name=' + encodeURIComponent(name));
  source.onopen = () => setConnection('实时跟随', 'live');
  source.onerror = () => setConnection('连接中断，正在重试', 'error');
  source.onmessage = event => {
    try { records.push(JSON.parse(event.data)); scheduleRender(); }
    catch { setConnection('记录格式有误', 'error'); }
  };
  source.addEventListener('reset', () => { records = []; scheduleRender(); });
  if (window.innerWidth <= 700) {
    shell.classList.add('sidebar-hidden');
    toggle.setAttribute('aria-expanded', 'false');
    toggle.setAttribute('aria-label', '显示 session 列表');
  }
}

function scheduleRender() {
  if (renderPending) return;
  renderPending = true;
  setTimeout(() => { renderPending = false; render(); }, 100);
}

function groupedEvents() {
  const result = [];
  for (const record of records) {
    const previous = result.at(-1);
    if (record.type === 'text_delta' && previous?.kind === 'stream' &&
        previous.runID === record.run_id && previous.turn === record.payload?.turn) {
      previous.records.push(record);
    } else if (record.type === 'text_delta') {
      result.push({ kind: 'stream', runID: record.run_id, turn: record.payload?.turn, records: [record] });
    } else {
      result.push({ kind: 'event', record });
    }
  }
  return result;
}

function render() {
  header.replaceChildren();
  timeline.replaceChildren();
  if (!selected) {
    header.append(node('h1', '', '尚无 session'));
    header.append(node('p', '', '运行 iota 后，这里会显示最近创建的 session。'));
    return;
  }
  header.append(node('h1', '', 'Session 时间线'));
  header.append(node('p', 'filename', selected));
  const meta = records.find(record => record.type === 'session_start');
  if (meta) {
    const facts = node('div', 'header-meta');
    facts.append(node('span', '', '模型 ' + (meta.payload?.model || '未知')));
    facts.append(node('span', '', '创建于 ' + formatTime(meta.timestamp)));
    facts.append(node('span', '', '目录 ' + (meta.payload?.cwd || '未知')));
    header.append(facts);
  }
  if (!records.length) {
    timeline.append(node('p', 'empty', '正在等待执行记录……'));
    return;
  }
  const counts = node('div', 'timeline-counts');
  counts.append(node('strong', '', records.length + ' 条事件'));
  counts.append(node('span', '', records.filter(item => item.type === 'run_start').length + ' 次任务 · ' +
    records.filter(item => item.type === 'turn_start').length + ' 轮模型 · ' +
    records.filter(item => item.type === 'tool_start').length + ' 次工具调用'));
  timeline.append(counts);
  const list = node('ol', 'event-list');
  for (const item of groupedEvents()) list.append(renderEvent(item));
  timeline.append(list);
}

function renderEvent(item) {
  const record = item.record || item.records[0];
  const last = item.records?.at(-1) || record;
  const payload = record.payload || {};
  const entry = node('li', 'event-entry event-' + (item.kind === 'stream' ? 'stream' : record.type));
  const rail = node('div', 'event-rail');
  rail.append(node('span', 'event-seq', record.seq === last.seq ? '#' + record.seq : '#' + record.seq + '–' + last.seq));
  const time = node('time', 'event-time', clockTime(record.timestamp));
  time.dateTime = record.timestamp || '';
  rail.append(time);
  entry.append(rail, node('span', 'event-dot'));
  const content = node('div', 'event-content');
  const head = node('div', 'event-head');
  const badge = node('span', 'event-badge');
  const title = node('strong', 'event-title');
  head.append(badge, title);
  content.append(head);
  entry.append(content);
  const summary = (value, className = 'event-summary') => {
    if (value) content.append(node('p', className, value));
  };

  if (item.kind === 'stream') {
    badge.textContent = 'STREAM';
    title.textContent = '模型文本逐段到达';
    summary(item.records.length + ' 个片段 · 第 ' + item.turn + ' 轮。完整回复会在稍后写入对话。');
    content.append(textDisclosure('查看生成中的文字', item.records.map(part => part.payload?.text || '').join(''), 'stream-' + record.seq));
    content.append(disclosure('查看各片段的序号、时间和 JSON', 'chunks-' + record.seq, () => {
      const rows = node('div', 'chunk-list');
      for (const part of item.records) {
        const row = node('div', 'chunk-row');
        row.append(node('span', 'mono', '#' + part.seq + ' · ' + clockTime(part.timestamp)));
        row.append(node('span', '', JSON.stringify(part.payload?.text || '')));
        row.append(jsonDisclosure(part));
        rows.append(row);
      }
      return rows;
    }));
    return entry;
  }

  switch (record.type) {
    case 'session_start':
      badge.textContent = 'SESSION'; title.textContent = '会话创建';
      summary('模型 ' + (payload.model || '未知') + ' · 工作目录 ' + (payload.cwd || '未知'));
      break;
    case 'session_end':
      badge.textContent = 'SESSION'; title.textContent = '会话关闭';
      summary('CLI 进程结束，记录文件已关闭。');
      break;
    case 'session_reset':
      badge.textContent = 'RESET'; title.textContent = '对话上下文重置';
      summary('后续模型请求从新的消息历史开始。');
      break;
    case 'run_start':
      badge.textContent = 'RUN'; title.textContent = '任务开始';
      summary('任务 ID ' + (record.run_id || '未知'));
      break;
    case 'run_end': {
      badge.textContent = 'RUN';
      const state = payload.reason === 'aborted' ? '已取消' : payload.is_error ? '失败' : payload.reason === 'max_turns' ? '达到轮数上限' : '正常结束';
      title.textContent = '任务' + state;
      summary('结束原因 ' + (payload.reason || '未知') + (payload.error ? ' · ' + payload.error : ''), payload.is_error ? 'event-summary error-text' : 'event-summary');
      break;
    }
    case 'turn_start':
      badge.textContent = 'TURN'; title.textContent = '第 ' + payload.turn + ' 轮开始';
      summary('Agent 准备向模型发送当前对话和可用工具。');
      break;
    case 'model_request': {
      badge.textContent = 'MODEL'; title.textContent = '向模型发送请求';
      const request = payload.request || {};
      summary((request.model || '模型') + ' · ' + (request.messages?.length || 0) + ' 条消息 · ' + (request.tools?.length || 0) + ' 个可用工具');
      content.append(disclosure('查看请求内容', 'request-' + record.seq, () => renderRequest(request, record.seq)));
      break;
    }
    case 'model_response': {
      badge.textContent = 'MODEL'; title.textContent = '模型完成本轮生成';
      const usage = payload.usage;
      summary('结束原因 ' + (payload.reason || '未知') + (usage ? ' · 输入 ' + usage.prompt_tokens + ' / 输出 ' + usage.completion_tokens + ' / 合计 ' + usage.total_tokens + ' tokens' : ''));
      break;
    }
    case 'message_added': {
      const message = payload.message || {};
      badge.textContent = message.role === 'user' ? 'USER' : message.role === 'tool' ? 'TOOL' : 'AGENT';
      title.textContent = message.role === 'user' ? '用户输入已加入对话' : message.role === 'tool' ? '工具结果已加入对话' : '助手回复已加入对话';
      if (message.role === 'tool') {
        summary((message.tool_name || '工具') + ' · 调用 ID ' + (message.tool_call_id || '未知') + (message.is_error ? ' · 错误' : '') + '。下一轮模型请求会包含这份结果。');
        content.append(textDisclosure('查看写入的内容', message.content || '', 'message-' + record.seq));
      } else if (message.content) {
        summary(message.content, 'message-text');
      }
      if (message.tool_calls?.length) {
        summary('模型请求调用 ' + message.tool_calls.map(call => call.name).join('、') + '；接下来的工具事件会执行它。');
        content.append(textDisclosure('查看工具调用参数', message.tool_calls, 'calls-' + record.seq));
      }
      break;
    }
    case 'tool_start':
      badge.textContent = 'TOOL'; title.textContent = '开始执行 ' + (payload.tool_call?.name || '工具');
      summary('调用 ID ' + (payload.tool_call?.id || '未知'));
      if (payload.tool_call?.arguments != null) content.append(textDisclosure('查看工具参数', payload.tool_call.arguments, 'args-' + record.seq));
      break;
    case 'tool_end':
      badge.textContent = payload.is_error ? 'ERROR' : 'TOOL';
      title.textContent = (payload.tool_call?.name || '工具') + (payload.is_error ? '执行失败' : '执行完成');
      summary('调用 ID ' + (payload.tool_call?.id || '未知'));
      content.append(textDisclosure('查看工具返回内容', payload.tool_result || '', 'result-' + record.seq));
      break;
    default:
      badge.textContent = 'EVENT'; title.textContent = record.type || '未知事件';
  }
  content.append(jsonDisclosure(record));
  return entry;
}

function renderRequest(request, seq) {
  const body = node('div', 'request-view');
  if (request.system_prompt) {
    body.append(node('div', 'detail-label', '系统提示词'));
    body.append(node('p', 'detail-text', request.system_prompt));
  }
  body.append(node('div', 'detail-label', '消息历史 · ' + (request.messages?.length || 0)));
  for (const message of request.messages || []) {
    const row = node('div', 'request-message');
    row.append(node('span', 'role-label', message.role || '未知'));
    row.append(node('span', 'detail-text', message.content || (message.tool_calls?.length ? '调用工具：' + message.tool_calls.map(call => call.name).join('、') : '（无文本）')));
    body.append(row);
  }
  body.append(node('div', 'detail-label', '可用工具 · ' + (request.tools?.length || 0)));
  const tools = node('div', 'tool-names');
  for (const tool of request.tools || []) tools.append(node('span', 'tool-name', tool.name));
  body.append(tools);
  body.append(textDisclosure('查看完整请求 JSON', request, 'request-json-' + seq));
  return body;
}

refreshSessions();
setInterval(refreshSessions, 2000);

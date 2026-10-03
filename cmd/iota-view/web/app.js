// 页面节点只查询一次；重绘时复用外层容器并替换其中的内容。
const shell = document.getElementById('shell');
const sessionNav = document.getElementById('sessions');
const sessionCount = document.getElementById('session-count');
const header = document.getElementById('session-header');
const timeline = document.getElementById('timeline');
const connection = document.getElementById('connection');
const toggle = document.getElementById('toggle-sidebar');
const deleteDialog = document.getElementById('delete-dialog');
const deleteConfirm = document.getElementById('delete-confirm');
const deleteCancel = document.getElementById('delete-cancel');
const deleteError = document.getElementById('delete-error');
let pendingDelete = null;
let deleting = false;
// 删除后使先前发起的目录请求失效，避免旧响应重新显示已删除会话。
let sessionsRequest = 0;

// 会话列表由目录轮询更新，不能把它当作事件流的完整内容。
let sessions = [];
// selected 与 source 对应同一会话；切换时先关闭旧连接。
let selected = null;
let source = null;
// records 只保存当前会话已收到的事件，重置通知会清空它。
let records = [];
// renderPending 防止密集流事件触发重复渲染。
let renderPending = false;
// 展开状态按事件和轮次分别记录，重绘后才能保持用户的查看位置。
const expanded = new Set();
const expandedTurns = new Set();

// 侧栏切换同步更新无障碍状态，供键盘和屏幕阅读器使用。
toggle.addEventListener('click', () => {
  const hidden = shell.classList.toggle('sidebar-hidden');
  toggle.setAttribute('aria-expanded', String(!hidden));
  toggle.setAttribute('aria-label', hidden ? '显示 session 列表' : '隐藏 session 列表');
});

// node 统一创建纯文本节点，避免把日志内容当作 HTML 执行。
function node(tag, className, value) {
  const element = document.createElement(tag);
  if (className) element.className = className;
  if (value != null) element.textContent = String(value);
  return element;
}

// formatTime 为列表显示本地时间；无效时间保留原值以便排查日志。
function formatTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value || '' : date.toLocaleString('zh-CN', { hour12: false });
}

// clockTime 保留毫秒，便于区分同一秒内密集到达的流事件。
function clockTime(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value || '';
  return date.toLocaleTimeString('zh-CN', { hour12: false }) + '.' + String(date.getMilliseconds()).padStart(3, '0');
}

// setConnection 同时更新文字和样式，避免页面显示与连接状态不一致。
function setConnection(label, state = '') {
  connection.textContent = label;
  connection.className = 'connection ' + state;
}

// disclosure 懒加载展开内容，避免大量原始请求和 JSON 同时生成 DOM。
// key 保留重绘前的展开状态。
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

// textDisclosure 将文本或对象放入可展开的预格式区域。
function textDisclosure(label, value, key) {
  return disclosure(label, key, () => node('pre', '', typeof value === 'string' ? value || '（空）' : JSON.stringify(value, null, 2)));
}

// highlightedJSON 用文本节点和 span 着色，不使用 innerHTML 处理日志数据。
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

// copyableCodeView 保留原文复制能力；高亮后的 DOM 只负责显示。
function copyableCodeView(label, code, copyText) {
  const view = node('div', 'json-view');
  const toolbar = node('div', 'json-toolbar');
  toolbar.append(node('span', 'json-toolbar-label', label));
  const copy = node('button', 'json-copy');
  copy.type = 'button';
  copy.setAttribute('aria-label', '复制' + label);
  copy.title = '复制';
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
  copy.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(copyText);
      copyLabel.textContent = '已复制';
    } catch {
      copyLabel.textContent = '复制失败';
    }
  });
  const pre = node('pre', 'json-pre');
  pre.append(code);
  view.append(toolbar, pre);
  return view;
}

// jsonDisclosure 提供单条事件的完整 JSON，便于核对摘要是否遗漏字段。
function jsonDisclosure(record) {
  return disclosure('查看 JSON · #' + record.seq, 'json-' + record.seq, () => {
    const { code, text } = highlightedJSON(record);
    return copyableCodeView('事件 #' + record.seq + ' 的 JSON', code, text);
  });
}

// rawStreamDisclosure 尝试高亮 SSE 中的 JSON；无法解析时仍展示原始文本。
function rawStreamDisclosure(record) {
  return disclosure('查看原始流消息', 'raw-chunk-' + record.seq, () => {
    const raw = record.payload?.raw_chunk || '';
    const data = /^(data:[ \t]*)(.*)$/s.exec(raw);
    let code;
    if (data) {
      try {
        code = highlightedJSON(JSON.parse(data[2])).code;
        code.prepend(document.createTextNode(data[1]));
      } catch {
        // 非 JSON 标记或损坏的消息直接按原文显示，避免丢失排查线索。
      }
    }
    return copyableCodeView('原始流消息 · #' + record.seq, code || node('code', 'json-code', raw || '（空）'), raw);
  });
}

// rawRequestDisclosure 展示真正发送的请求体，而非重新拼装的页面摘要。
function rawRequestDisclosure(record) {
  return disclosure('查看原始请求体', 'raw-request-' + record.seq, () => {
    const raw = record.payload?.raw_request || '';
    let code;
    try {
      code = highlightedJSON(JSON.parse(raw)).code;
    } catch {
      // 无法解析的请求体仍保留原文，方便定位编码错误。
    }
    return copyableCodeView('原始请求体 · #' + record.seq, code || node('code', 'json-code', raw || '（空）'), raw);
  });
}

// refreshSessions 定期刷新目录摘要；当前会话被删除时关闭旧连接并选择新会话。
async function refreshSessions() {
  const request = ++sessionsRequest;
  try {
    const response = await fetch('/api/sessions', { cache: 'no-store' });
    if (!response.ok) throw new Error('HTTP ' + response.status);
    const next = await response.json();
    if (request === sessionsRequest) updateSessions(next);
  } catch (error) {
    if (request === sessionsRequest) setConnection('无法读取目录：' + error.message, 'error');
  }
}

// updateSessions 同步列表与选择，删除当前会话时关闭事件流并清空详情。
function updateSessions(next) {
  sessions = next;
  sessionCount.textContent = String(sessions.length);
  if (selected && !sessions.some(item => item.name === selected)) {
    source?.close();
    source = null;
    selected = null;
    records = [];
    expanded.clear();
    expandedTurns.clear();
    scheduleRender();
    setConnection('尚无 session');
  }
  if (!selected && sessions.length) selectSession(sessions[0].name);
  else renderSessionList();
}

deleteCancel.addEventListener('click', () => deleteDialog.close());
deleteDialog.addEventListener('cancel', event => {
  if (deleting) event.preventDefault();
});
deleteDialog.addEventListener('close', () => {
  const name = pendingDelete?.name;
  pendingDelete = null;
  // 目录轮询会重建按钮，关闭确认框后显式恢复键盘焦点。
  const button = [...sessionNav.querySelectorAll('.session-delete')].find(item => item.dataset.sessionName === name);
  (button || sessionNav.querySelector('.session-item.active, .session-item') || toggle).focus();
});
deleteConfirm.addEventListener('click', async () => {
  if (!pendingDelete || deleting) return;
  const name = pendingDelete.name;
  deleting = true;
  deleteConfirm.disabled = deleteCancel.disabled = true;
  deleteConfirm.textContent = '正在删除…';
  deleteError.hidden = true;
  try {
    const response = await fetch('/api/sessions?name=' + encodeURIComponent(name), {
      method: 'DELETE', headers: { 'X-Iota-Delete': '1' }
    });
    if (!response.ok && response.status !== 404) throw new Error((await response.text()).trim() || 'HTTP ' + response.status);
    sessionsRequest++;
    updateSessions(sessions.filter(session => session.name !== name));
    deleteDialog.close();
  } catch (error) {
    deleteError.textContent = '删除失败：' + error.message + '。请重试。';
    deleteError.hidden = false;
  } finally {
    deleting = false;
    deleteConfirm.disabled = deleteCancel.disabled = false;
    deleteConfirm.textContent = '删除文件';
  }
});

// renderSessionList 重新生成导航，并标明当前选中的会话。
function renderSessionList() {
  sessionNav.replaceChildren();
  if (!sessions.length) {
    sessionNav.append(node('p', 'side-empty', '暂无 session。运行 iota 后，记录会出现在这里。'));
    return;
  }
  for (const session of sessions) {
    const row = node('div', 'session-row' + (session.name === selected ? ' active' : ''));
    const button = node('button', 'session-item' + (session.name === selected ? ' active' : ''));
    button.type = 'button';
    button.setAttribute('aria-current', session.name === selected ? 'page' : 'false');
    button.append(node('span', 'session-title', session.title || '尚无提问'));
    button.append(node('span', 'session-date', formatTime(session.created_at)));
    button.append(node('span', 'session-meta', (session.model || 'model') + ' · ' + session.name.slice(11, 19)));
    button.addEventListener('click', () => selectSession(session.name));
    const remove = node('button', 'session-delete');
    remove.type = 'button';
    remove.dataset.sessionName = session.name;
    remove.title = '删除 session';
    remove.setAttribute('aria-label', '删除 session：' + (session.title || session.name));
    const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    icon.setAttribute('viewBox', '0 0 24 24');
    icon.setAttribute('fill', 'none');
    icon.setAttribute('stroke', 'currentColor');
    icon.setAttribute('stroke-width', '1.8');
    icon.setAttribute('stroke-linecap', 'round');
    icon.setAttribute('stroke-linejoin', 'round');
    icon.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'M3 6h18M9 6V4h6v2M5 6l1 14h12l1-14M10 10v6M14 10v6');
    icon.append(path);
    remove.append(icon);
    remove.addEventListener('click', () => {
      pendingDelete = session;
      document.getElementById('delete-session-title').textContent = session.title || '尚无提问';
      document.getElementById('delete-filename').textContent = session.name;
      deleteError.hidden = true;
      deleteDialog.showModal();
    });
    row.append(button, remove);
    sessionNav.append(row);
  }
}

// selectSession 关闭上一条 SSE 连接并清空旧事件，避免不同会话的记录混在一起。
function selectSession(name) {
  if (selected === name) return;
  source?.close();
  selected = name;
  records = [];
  expanded.clear();
  expandedTurns.clear();
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
  source.addEventListener('reset', () => { records = []; expanded.clear(); expandedTurns.clear(); scheduleRender(); });
  if (window.innerWidth <= 700) {
    shell.classList.add('sidebar-hidden');
    toggle.setAttribute('aria-expanded', 'false');
    toggle.setAttribute('aria-label', '显示 session 列表');
  }
}

// scheduleRender 合并短时间内的多次流事件更新，减少重复构建整条时间线。
function scheduleRender() {
  if (renderPending) return;
  renderPending = true;
  setTimeout(() => { renderPending = false; render(); }, 100);
}

// reasoningChunk 兼容独立推理事件及旧日志中的原始流字段，避免重复显示推理内容。
function reasoningChunk(record) {
  if (record.type === 'reasoning_delta') return record.payload?.reasoning || '';
  if (record.type !== 'model_stream_other') return '';
  const raw = record.payload?.raw_chunk || '';
  if (!raw.startsWith('data:')) return '';
  try {
    const chunk = JSON.parse(raw.slice(5).trim());
    return (chunk.choices || []).map(choice => {
      const delta = choice.delta || {};
      if (typeof delta.reasoning === 'string' && delta.reasoning) return delta.reasoning;
      if (typeof delta.reasoning_content === 'string' && delta.reasoning_content) return delta.reasoning_content;
      return (Array.isArray(delta.reasoning_details) ? delta.reasoning_details : [])
        .filter(detail => typeof detail?.text === 'string').map(detail => detail.text).join('');
    }).join('');
  } catch {
    return '';
  }
}

// groupedEvents 仅合并同一任务、同一轮的连续片段，保留其他事件的原始顺序。
function groupedEvents() {
  const result = [];
  for (const record of records) {
    const previous = result.at(-1);
    const reasoning = reasoningChunk(record);
    const kind = reasoning ? 'reasoning' : record.type;
    if ((kind === 'text_delta' || kind === 'tool_call_delta' || kind === 'reasoning') && previous?.kind === kind &&
        previous.runID === record.run_id && previous.turn === record.payload?.turn) {
      previous.records.push(record);
      if (reasoning) previous.text += reasoning;
    } else if (kind === 'text_delta' || kind === 'tool_call_delta' || kind === 'reasoning') {
      result.push({ kind, runID: record.run_id, turn: record.payload?.turn, records: [record], text: reasoning });
    } else {
      result.push({ kind: 'event', record });
    }
  }
  return result;
}

// render 根据当前会话记录重建摘要和时间线，同时恢复展开状态。
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
  let activeTurn = null;
  for (const item of groupedEvents()) {
    const record = item.record || item.records[0];
    const turn = record.payload?.turn;
    if (record.type === 'turn_start' && turn) {
      activeTurn = { key: record.run_id + ':' + record.seq, runID: record.run_id, turn };
    } else if (!activeTurn || record.run_id !== activeTurn.runID || turn !== activeTurn.turn || record.type === 'run_end') {
      activeTurn = null;
    }
    const entry = renderEvent(item, activeTurn?.key, list);
    if (activeTurn) {
      entry.dataset.turnKey = activeTurn.key;
      if (record.type === 'turn_start') entry.dataset.turnStart = 'true';
      else entry.hidden = !expandedTurns.has(activeTurn.key);
    }
    list.append(entry);
  }
  timeline.append(list);
}

// renderEvent 按事件类型生成摘要；详细 JSON 保持可展开，避免默认淹没时间线。
function renderEvent(item, turnKey, list) {
  const record = item.record || item.records[0];
  const last = item.records?.at(-1) || record;
  const payload = record.payload || {};
  const entry = node('li', 'event-entry event-' + record.type);
  const rail = node('div', 'event-rail');
  rail.append(node('span', 'event-seq', record.seq === last.seq ? '#' + record.seq : '#' + record.seq + '–' + last.seq));
  const time = node('time', 'event-time', clockTime(record.timestamp));
  time.dateTime = record.timestamp || '';
  rail.append(time);
  if (payload.turn && record.type !== 'run_end') {
    const label = 'Turn ' + payload.turn;
    if (record.type === 'turn_start' && turnKey) {
      const button = node('button', 'event-turn-toggle', label);
      button.type = 'button';
      button.setAttribute('aria-expanded', String(expandedTurns.has(turnKey)));
      button.setAttribute('aria-label', label + '，' + (expandedTurns.has(turnKey) ? '折叠整轮' : '展开整轮'));
      button.addEventListener('click', () => {
        const open = !expandedTurns.has(turnKey);
        if (open) expandedTurns.add(turnKey);
        else expandedTurns.delete(turnKey);
        for (const row of list.children) {
          if (row.dataset.turnKey === turnKey && row.dataset.turnStart !== 'true') row.hidden = !open;
        }
        button.setAttribute('aria-expanded', String(open));
        button.setAttribute('aria-label', label + '，' + (open ? '折叠整轮' : '展开整轮'));
      });
      rail.append(button);
    } else {
      rail.append(node('span', 'event-turn-id', label));
    }
  }
  entry.append(rail, node('span', 'event-dot'));
  const content = node('details', 'event-content');
  const eventKey = 'event-' + record.seq;
  content.open = expanded.has(eventKey);
  content.addEventListener('toggle', () => {
    if (content.open) expanded.add(eventKey);
    else expanded.delete(eventKey);
  });
  const head = node('summary', 'event-head');
  const badge = node('span', 'event-badge');
  const title = node('strong', 'event-title');
  head.append(badge, title);
  content.append(head);
  entry.append(content);
  const summary = (value, className = 'event-summary') => {
    if (value) content.append(node('p', className, value));
  };

  if (item.kind === 'text_delta' || item.kind === 'tool_call_delta' || item.kind === 'reasoning') {
    const isToolCall = item.kind === 'tool_call_delta';
    const isReasoning = item.kind === 'reasoning';
    badge.textContent = isReasoning ? 'REASON' : 'STREAM';
    title.textContent = isReasoning ? '模型推理逐段到达' : isToolCall ? '工具调用逐段到达' : '模型文本逐段到达';
    summary(item.records.length + ' 个片段 · 第 ' + item.turn + ' 轮。' +
      (isReasoning ? '连续的推理片段已合并。' : isToolCall ? '完整工具调用会在稍后写入对话。' : '完整回复会在稍后写入对话。'));
    if (isToolCall) {
      const calls = new Map();
      for (const part of item.records) {
        const delta = part.payload?.tool_call_delta || {};
        const call = calls.get(delta.index) || { index: delta.index, id: '', name: '', arguments: '' };
        if (delta.id) call.id = delta.id;
        call.name += delta.name || '';
        call.arguments += delta.arguments || '';
        calls.set(delta.index, call);
      }
      content.append(textDisclosure('查看生成中的工具调用', [...calls.values()], 'stream-' + record.seq));
    } else if (isReasoning) {
      content.append(textDisclosure('查看推理内容', item.text, 'reasoning-' + record.seq));
    } else {
      content.append(textDisclosure('查看生成中的文字', item.records.map(part => part.payload?.text || '').join(''), 'stream-' + record.seq));
    }
    content.append(disclosure('查看各片段的序号、时间和 JSON', 'chunks-' + record.seq, () => {
      const rows = node('div', 'chunk-list');
      for (const part of item.records) {
        const row = node('div', 'chunk-row');
        row.append(node('span', 'mono', '#' + part.seq + ' · ' + clockTime(part.timestamp)));
        row.append(node('span', '', JSON.stringify(isReasoning ? reasoningChunk(part) : isToolCall ? part.payload?.tool_call_delta || {} : part.payload?.text || '')));
        if (part.payload?.raw_chunk) row.append(rawStreamDisclosure(part));
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
      if (payload.raw_request) content.append(rawRequestDisclosure(record));
      content.append(disclosure('查看请求内容', 'request-' + record.seq, () => renderRequest(request, record.seq)));
      break;
    }
    case 'model_stream_finish':
    case 'model_stream_done':
    case 'model_stream_other': {
      const raw = payload.raw_chunk?.trim() || '';
      let chunk;
      if (record.type === 'model_stream_other' && raw.startsWith('data:') && raw !== 'data: [DONE]') {
        try { chunk = JSON.parse(raw.slice(5).trim()); } catch { /* 未知流消息仍按原文展示。 */ }
      }
      const reason = payload.reason || chunk?.choices?.find(choice => choice.finish_reason)?.finish_reason;
      const usage = payload.usage || chunk?.usage;
      if (record.type === 'model_stream_done' || raw === 'data: [DONE]') {
        badge.textContent = 'DONE'; title.textContent = '模型流传输结束';
        summary('服务端发送 [DONE] 标记。');
      } else if (record.type === 'model_stream_finish' || reason) {
        badge.textContent = 'FINISH'; title.textContent = '模型结束生成';
        summary('结束原因 ' + (reason || '未知') + (usage ? ' · 输入 ' + usage.prompt_tokens + ' / 输出 ' + usage.completion_tokens + ' / 合计 ' + usage.total_tokens + ' tokens' : ''));
      } else {
        badge.textContent = 'STREAM'; title.textContent = '其他原始流消息';
        summary('未包含可显示的正文、推理或工具调用');
      }
      content.append(rawStreamDisclosure(record));
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

// renderRequest 展示系统提示、消息和工具摘要，并保留完整请求 JSON 供核对。
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

// 首次加载后持续轮询会话列表；单个会话的事件由 SSE 实时跟随。
refreshSessions();
setInterval(refreshSessions, 2000);

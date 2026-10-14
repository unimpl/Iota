import { useEffect, useLayoutEffect, useReducer, useRef, useState } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api, HttpError } from "./api";
import { Icon } from "./icons";
import { applyBatch, emptyModel, fromHistory, mergeHistory } from "./model";
import { RunNavigator } from "./RunNavigator";
import type { ChatItem, ChatModel, HistorySnapshot } from "./model";
import { busy } from "./types";
import type { Answer, Command, EventBatch, PendingInput, SessionSummary, State } from "./types";

const statusText: Record<string, string> = { idle: "就绪", running: "正在执行", waiting_input: "等待你的回答", cancelling: "正在停止", completed: "已完成", failed: "执行失败", aborted: "已停止", interrupted: "上次任务已中断", closed: "查看历史" };
type ModelAction = { batch: EventBatch } | { history: HistorySnapshot } | { page: HistorySnapshot } | { reset: true };
function reducer(model: ChatModel, action: ModelAction): ChatModel {
  if ("reset" in action) return emptyModel;
  if ("page" in action) return mergeHistory(model, action.page);
  return "history" in action ? fromHistory(action.history) : applyBatch(model, action.batch);
}

function RichText({ text }: { text: string }) {
  return <div className="markdown"><Markdown remarkPlugins={[remarkGfm]} components={{ a: ({ href, children }) => <a href={href} target="_blank" rel="noopener noreferrer">{children}</a> }}>{text}</Markdown></div>;
}

function Message({ item }: { item: ChatItem }) {
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState("");
  return <article id={`message-${item.id}`} data-run={item.run_id} tabIndex={-1} className={`message ${item.role}`} aria-label={item.role === "user" ? "你的消息" : item.role === "notice" ? "会话提示" : "Iota 回复"}>
    {item.reasoning && <details className="reasoning"><summary>思考过程 <Icon name="chevron" size={16} /></summary><div className="plain">{item.reasoning}</div></details>}
    {item.text && (item.role === "user" ? <div className="user-bubble">{item.text}</div> : <RichText text={item.text} />)}
    {item.tools.map(tool => <details className={`tool ${tool.status}`} key={tool.id}>
      <summary><span className="tool-mark"><Icon name={tool.status === "completed" ? "check" : "more"} size={16} /></span><span>{tool.name === "request_user_input" ? "询问用户" : tool.name}</span><span className="tool-status">{{ running: "执行中", completed: "完成", failed: "失败", interrupted: "已中断" }[tool.status]}</span><Icon name="chevron" size={16} /></summary>
      <pre>{tool.arguments}</pre>{tool.result && <pre>{tool.result}</pre>}
    </details>)}
    {item.error && <p className="error-inline">{item.error}</p>}
    {item.role === "assistant" && item.complete && item.text && <div className="message-actions"><button className="icon-button" aria-label="复制回复" title="复制回复" onClick={async () => {
      try { await navigator.clipboard.writeText(item.text); setCopied(true); setCopyError(""); }
      catch { setCopyError("无法访问剪贴板，请选择文本复制。"); }
    }}><Icon name={copied ? "check" : "copy"} size={17} /></button>{copyError && <span role="status">{copyError}</span>}</div>}
  </article>;
}

function Questionnaire({ pending, submit, stopping }: { pending: PendingInput; submit: (answers: Record<string, Answer>) => Promise<void>; stopping: boolean }) {
  const [choices, setChoices] = useState<Record<string, string>>({});
  const [texts, setTexts] = useState<Record<string, string>>({});
  const [sending, setSending] = useState(false);
  const [error, setError] = useState("");
  const structured = pending.request.format === "structured";
  return <form className="questionnaire" onSubmit={async event => {
    event.preventDefault();
    const answers: Record<string, Answer> = {};
    for (const question of pending.request.questions) {
      const text = texts[question.id] ?? "";
      if (text.trim()) answers[question.id] = { source: "custom", value: text };
      else if (structured) {
        const selected = choices[question.id] ?? question.recommended_option_id;
        answers[question.id] = { source: choices[question.id] ? "selected" : "recommended", option_id: selected };
      } else answers[question.id] = { source: "unanswered" };
    }
    setSending(true); setError("");
    try { await submit(answers); } catch (error) { setError(String(error)); setSending(false); }
  }}>
    <h2>需要你的回答</h2>
    {pending.request.questions.map(question => <fieldset key={question.id} disabled={sending || stopping}>
      <legend>{question.question}</legend>
      {structured && [...(question.options ?? [])].sort((a, b) => Number(b.id === question.recommended_option_id) - Number(a.id === question.recommended_option_id)).map(option => <label className="question-option" key={option.id}>
        <input type="radio" name={question.id} checked={(choices[question.id] ?? question.recommended_option_id) === option.id && !texts[question.id]?.trim()} onChange={() => { setChoices(current => ({ ...current, [question.id]: option.id })); setTexts(current => ({ ...current, [question.id]: "" })); }} />
        <span><strong>{option.label}</strong>{option.id === question.recommended_option_id && <span className="recommended">推荐</span>}<small>{option.description}</small></span>
      </label>)}
      <label className="answer-label">{structured ? "或填写自己的答案" : "你的回答（可留空）"}<textarea rows={2} value={texts[question.id] ?? ""} onChange={event => setTexts(current => ({ ...current, [question.id]: event.target.value }))} /></label>
    </fieldset>)}
    {error && <p className="error-inline" role="alert">{error}</p>}
    <button className="primary" type="submit" disabled={sending || stopping}>{sending ? "正在提交…" : "提交回答"}</button>
  </form>;
}

export function App() {
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [selected, setSelected] = useState<string | null>(() => localStorage.getItem("iota.session"));
  const [state, setState] = useState<State | null>(null);
  const [config, setConfig] = useState<{ model: string; cwd: string } | null>(null);
  const [cwd, setCWD] = useState("");
	const [initialMode,setInitialMode] = useState("default");
  const [draft, setDraft] = useState("");
  const [error, setError] = useState("");
  const [working, setWorking] = useState(false);
  const [loading, setLoading] = useState(false);
  const [connected, setConnected] = useState(false);
  const [sidebar, setSidebar] = useState(() => window.innerWidth > 760);
  const [menu, setMenu] = useState(false);
  const [search, setSearch] = useState("");
  const [model, dispatch] = useReducer(reducer, emptyModel);
  const [historyLoading, setHistoryLoading] = useState(false);
  const [historyError, setHistoryError] = useState("");
  const historyRetry = useRef<{ before: string } | { run: string } | null>(null);
  const [activeRun, setActiveRun] = useState("");
  const historyRequest = useRef<AbortController | null>(null);
  const pendingJump = useRef<string | null>(null);
  const scrollAnchor = useRef<{ id: string; top: number } | null>(null);
  const lastScroll = useRef(0);
  const scroller = useRef<HTMLDivElement>(null);
  const follows = useRef(true);
  const composer = useRef<HTMLTextAreaElement>(null);
  const pendingCommand = useRef<{ name: string; command: Command } | null>(null);
  const running = busy(state);
  const active = state?.open ?? false;
  const selectedSummary = sessions.find(session => session.name === selected);
  const collaboration = state?.collaboration ?? model.collaboration;
  const plan = state?.plan_preview ?? collaboration?.plan;
  const oldestLoaded = model.runs.find(run => model.loadedRuns.includes(run.id));
  const hasOlder = !!oldestLoaded && model.runs[0]?.id !== oldestLoaded.id;

  function jumpToRun(id: string) {
    const node = document.getElementById(`run-${id}`);
    const scroll = scroller.current;
    if (!node || !scroll) return;
    follows.current = false;
    scroll.scrollTop += node.getBoundingClientRect().top - scroll.getBoundingClientRect().top - 24;
    node.focus({ preventScroll: true });
    setActiveRun(id);
  }

  async function loadHistory(query: { before: string } | { run: string }) {
    if (!selected) return;
    if (historyRequest.current) {
      if ("before" in query) return;
      historyRequest.current.abort();
    }
    const abort = new AbortController();
    historyRequest.current = abort;
    setHistoryLoading(true); setHistoryError("");
    try {
      const params = new URLSearchParams({ ...query, at: String(model.sequence) });
      const page = await api<HistorySnapshot>(`sessions/${selected}/history?${params}`, undefined, "GET", abort.signal);
      if (abort.signal.aborted) return;
      follows.current = false;
      if ("run" in query) pendingJump.current = query.run;
      else {
        const scroll = scroller.current;
        const node = scroll && [...scroll.querySelectorAll<HTMLElement>(".message")].find(node => node.getBoundingClientRect().bottom > scroll.getBoundingClientRect().top);
        if (node) scrollAnchor.current = { id: node.id, top: node.getBoundingClientRect().top };
      }
      dispatch({ page });
    } catch (error) {
      if (!abort.signal.aborted) { historyRetry.current = query; setHistoryError(String(error)); }
    } finally {
      if (historyRequest.current === abort) { historyRequest.current = null; setHistoryLoading(false); }
    }
  }

  async function refreshSessions() {
    const result = await api<SessionSummary[]>("sessions");
    setSessions(result);
  }

  useEffect(() => {
    const abort = new AbortController();
    void Promise.all([api<SessionSummary[]>("sessions", undefined, "GET", abort.signal), api<{ model: string; cwd: string; mode: string }>("config", undefined, "GET", abort.signal)])
      .then(([sessions, config]) => { setSessions(sessions); setConfig(config); setCWD(config.cwd); setInitialMode(config.mode); })
      .catch(error => { if (!abort.signal.aborted) setError(String(error)); });
    return () => abort.abort();
  }, []);

  useEffect(() => {
    dispatch({ reset: true }); setState(null); setConnected(false); setMenu(false);
    historyRequest.current?.abort(); historyRequest.current = null;
    pendingJump.current = null; scrollAnchor.current = null;
    historyRetry.current = null;
    setHistoryLoading(false); setHistoryError(""); setActiveRun(""); lastScroll.current = 0;
    follows.current = true;
    if (!selected) { localStorage.removeItem("iota.session"); setLoading(false); return; }
    localStorage.setItem("iota.session", selected);
    const abort = new AbortController();
    let stream: EventSource | undefined;
    setLoading(true);
    void (async () => {
      try {
        const [history, snapshot] = await Promise.all([
          api<HistorySnapshot>(`sessions/${selected}/history`, undefined, "GET", abort.signal),
          api<State>(`sessions/${selected}/state`, undefined, "GET", abort.signal),
        ]);
        if (abort.signal.aborted) return;
        dispatch({ history });
        setState(snapshot); setLoading(false);
        stream = new EventSource(`./api/sessions/${selected}/events?after=${history.cursor}&interval_ms=1000`);
        stream.onopen = () => setConnected(true);
        stream.onerror = () => setConnected(false);
        stream.addEventListener("records", event => {
          if (abort.signal.aborted) return;
          const batch = JSON.parse((event as MessageEvent<string>).data) as EventBatch;
          dispatch({ batch });
          if (batch.records.some(record => record.type === "client_result" || record.payload?.message?.role === "user")) void refreshSessions().catch(() => {});
        });
        stream.addEventListener("state", event => { if (!abort.signal.aborted) setState(JSON.parse((event as MessageEvent<string>).data) as State); });
      } catch (error) { if (!abort.signal.aborted) { setError(String(error)); setLoading(false); if (error instanceof HttpError && error.status === 404) setSelected(null); } }
    })();
    return () => { abort.abort(); stream?.close(); historyRequest.current?.abort(); };
  }, [selected]);

  useLayoutEffect(() => {
    const scroll = scroller.current;
    if (!scroll) return;
    if (pendingJump.current) { jumpToRun(pendingJump.current); pendingJump.current = null; }
    else if (scrollAnchor.current) {
      const anchor = scrollAnchor.current;
      const node = document.getElementById(anchor.id);
      if (node) scroll.scrollTop += node.getBoundingClientRect().top - anchor.top;
      scrollAnchor.current = null;
    } else if (follows.current) { scroll.scrollTop = scroll.scrollHeight; setActiveRun(model.runs.at(-1)?.id ?? ""); }
    lastScroll.current = scroll.scrollTop;
  }, [model, state?.pending]);

  useEffect(() => {
    if (!composer.current) return;
    composer.current.style.height = "auto";
    composer.current.style.height = `${Math.min(composer.current.scrollHeight, 200)}px`;
  }, [draft]);

  async function choose(name: string | null) {
    if (working || name === selected) return;
    setWorking(true); setError("");
    try {
      if (selected) {
        try {
          const latest = await api<State>(`sessions/${selected}/state`);
          if (latest.open && !busy(latest)) await api<void>(`sessions/${selected}/close`, {});
        } catch (error) { if (!(error instanceof HttpError && error.status === 404)) throw error; }
      }
      setSelected(name); setDraft("");
      if (window.innerWidth <= 760) setSidebar(false);
      await refreshSessions();
    } catch (error) { setError(String(error)); }
    finally { setWorking(false); }
  }

  async function openSession() {
    if (!selected) return;
    setWorking(true); setError("");
    try { await api(`sessions/${selected}/open`, {}); setState(await api<State>(`sessions/${selected}/state`)); await refreshSessions(); }
    catch (error) { setError(String(error)); }
    finally { setWorking(false); }
  }

  async function command(action: Command["action"], prompt?: string, mode?: string) {
    if (working || running) return;
    setWorking(true); setError(""); setMenu(false);
    try {
      let name = selected;
      if (!name) {
        const created = await api<{ name: string }>("sessions", { cwd, mode: initialMode });
        name = created.name; setSelected(name);
      }
      if (selected && !active) throw new Error("请先打开会话，再继续操作。");
      const previous = pendingCommand.current;
      const request: Command = previous?.name === name && previous.command.action === action && previous.command.prompt === prompt && previous.command.mode === mode
        ? previous.command : { request_id: Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, "0")).join(""), action, prompt, mode, plan_hash: action === "execute" || prompt?.trim() === "/execute" ? state?.plan_hash : undefined };
      pendingCommand.current = { name, command: request };
      const result = await api<{ status: string; error?: string }>(`sessions/${name}/commands`, request);
      if (result.error) setError(result.error);
      pendingCommand.current = null;
      if (action === "run" || action === "skill_install") setDraft("");
      setState(await api<State>(`sessions/${name}/state`));
      await refreshSessions();
    } catch (error) { setError(String(error)); }
    finally { setWorking(false); }
  }

  async function submit() {
    const text = draft.trim();
    if (!text) return;
    return command("run", text);
  }

  async function release() {
    if (!selected || working) return;
    setWorking(true); setError(""); setMenu(false);
    try { await api<void>(`sessions/${selected}/close`, {}); setState(await api<State>(`sessions/${selected}/state`)); await refreshSessions(); }
    catch (error) { setError(String(error)); }
    finally { setWorking(false); }
  }

  return <div className={`app-shell ${sidebar ? "sidebar-open" : "sidebar-closed"}`}>
    {sidebar && <button className="sidebar-backdrop" aria-label="关闭会话列表" onClick={() => setSidebar(false)} />}
    <aside className="sidebar" aria-label="会话列表">
      <div className="sidebar-head"><a className="brand" href="./" aria-label="Iota 首页">Iota<span className="brand-dot" /></a><button className="icon-button" aria-label="收起侧栏" onClick={() => setSidebar(false)}><Icon name="panel" /></button></div>
      <button className="new-chat" disabled={working} onClick={() => void choose(null)}><Icon name="new" />新对话</button>
      <label className="search-label"><span className="sr-only">搜索会话</span><input placeholder="搜索对话" value={search} onChange={event => setSearch(event.target.value)} /></label>
      <div className="session-list">
        <p className="list-label">对话</p>
        {sessions.filter(session => `${session.title} ${session.cwd}`.toLowerCase().includes(search.toLowerCase())).map(session => <div className={`session-row ${session.name === selected ? "selected" : ""}`} key={session.name}>
          <button className="session-link" disabled={working} onClick={() => void choose(session.name)} title={session.cwd}><span>{session.title}</span>{session.occupied && <small>{session.open ? "Web 已打开" : "已占用"}</small>}</button>
          <button className="icon-button delete" disabled={working || session.occupied} aria-label={`删除 ${session.title}`} title={session.occupied ? "释放会话后才能删除" : "删除会话"} onClick={async () => {
            if (!window.confirm(`永久删除“${session.title}”？`)) return;
            try { await api<void>(`sessions/${session.name}`, undefined, "DELETE"); if (selected === session.name) setSelected(null); await refreshSessions(); }
            catch (error) { setError(String(error)); }
          }}><Icon name="trash" size={16} /></button>
        </div>)}
        {!sessions.length && <p className="sidebar-empty">新的对话会保存在这里。</p>}
      </div>
      <div className="sidebar-footer"><span className="avatar">I</span><div><strong>本地工作空间</strong><small>CLI 与 Web 共享会话</small></div></div>
    </aside>
    <main className="main">
      <header className="topbar"><div className="header-left">{!sidebar && <button className="icon-button" aria-label="展开侧栏" onClick={() => setSidebar(true)}><Icon name="panel" /></button>}<span className="app-name">Iota <span className="model-name">{state?.model || config?.model || ""}</span></span></div>
        <div className="header-right"><span className={`connection ${selected && !connected ? "offline" : ""}`} role="status">{selected && !connected && !loading ? "正在重连" : statusText[state?.status ?? "idle"]}</span>{selected && <div className="menu-wrap"><button className="icon-button" aria-label="会话操作" aria-expanded={menu} onClick={() => setMenu(current => !current)}><Icon name="more" /></button>{menu && <div className="menu"><button disabled={!active || running || working} onClick={() => void command("compact", "")}>压缩上下文</button><button disabled={!active || running || working} onClick={() => { if (window.confirm("清空模型上下文？历史记录仍保留。")) void command("reset"); }}>重置上下文</button><button disabled={!active || running || working} onClick={() => void command("skills_reload")}>重新加载 skills</button><button disabled={!active || running || working} onClick={() => void release()}>释放会话</button></div>}</div>}</div>
      </header>
      <div className="conversation-frame">
      {historyLoading && <p className="history-loading" role="status">正在加载历史…</p>}
      {historyError && <div className="history-error" role="alert"><span>{historyError.replace(/^Error: /, "")}</span><button className="history-retry" onClick={() => { if (historyRetry.current) void loadHistory(historyRetry.current); }}>重试加载</button></div>}
      {selected && !loading && !!model.runs.length && <RunNavigator runs={model.runs} active={activeRun} jump={id => { if (model.loadedRuns.includes(id)) jumpToRun(id); else void loadHistory({ run: id }); }} />}
      <div className={`conversation-scroll ${model.runs.length ? "with-index" : ""}`} ref={scroller} onWheel={event => {
        if (event.deltaY < 0 && (scroller.current?.scrollTop ?? 0) < 100 && hasOlder && oldestLoaded && !historyError) void loadHistory({ before: oldestLoaded.id });
      }} onScroll={() => {
        const node = scroller.current;
        if (!node) return;
        const upwards = node.scrollTop < lastScroll.current;
        lastScroll.current = node.scrollTop;
        follows.current = node.scrollHeight - node.scrollTop - node.clientHeight < 100;
        const visible = [...node.querySelectorAll<HTMLElement>(".message[data-run]")].find(item => item.getBoundingClientRect().bottom > node.getBoundingClientRect().top + 24);
        if (visible?.dataset.run) setActiveRun(visible.dataset.run);
        if (upwards && node.scrollTop < 100 && hasOlder && oldestLoaded && !historyError) void loadHistory({ before: oldestLoaded.id });
      }}>
        <div className={`conversation ${!selected ? "empty" : ""}`}>
          {!selected && <div className="welcome"><h1>今天要做什么？</h1><p>描述任务，Iota 会在你的项目中执行。</p><label className="workspace-field">工作目录<input aria-label="工作目录" value={cwd} onChange={event => setCWD(event.target.value)} placeholder="项目的绝对路径" /></label></div>}
          {loading && <p className="loading" role="status">正在读取会话…</p>}
          {selected && !loading && <div className="workspace-path" title={state?.cwd || selectedSummary?.cwd}>{state?.cwd || selectedSummary?.cwd}</div>}
          {hasOlder && oldestLoaded && <button className="history-more" disabled={historyLoading} onClick={() => void loadHistory({ before: oldestLoaded.id })}>加载更早的任务</button>}
          {model.items.filter(item => !item.run_id).map(item => <Message item={item} key={item.id} />)}
          {model.runs.map((run, index) => {
            if (!model.loadedRuns.includes(run.id)) return null;
            const previous = model.runs[index - 1];
            const gap = previous && !model.loadedRuns.includes(previous.id) && run.id !== oldestLoaded?.id;
            return <section className="run-messages" id={`run-${run.id}`} tabIndex={-1} aria-label={run.title} key={run.id}>
              {gap && <button className="history-more" disabled={historyLoading} onClick={() => void loadHistory({ before: run.id })}>加载这之前的任务</button>}
              {model.items.filter(item => item.run_id === run.id).map(item => <Message item={item} key={item.id} />)}
            </section>;
          })}
          {selected && !loading && !model.items.length && <div className="welcome"><h1>从一条消息开始</h1><p>这份会话还没有任务。</p></div>}
          {collaboration?.progress && <details className="progress"><summary>执行进度 <Icon name="chevron" size={16} /></summary><ul>{collaboration.progress.plan.map(step => <li key={step.step_id}><span className={`step-mark ${step.status}`}><Icon name={step.status === "completed" ? "check" : "more"} size={16} /></span>{step.step}</li>)}</ul></details>}
          {plan && <details className="saved-plan"><summary>当前计划 <Icon name="chevron" size={16} /></summary><RichText text={plan.content} />{state?.plan_error && <p className="error-inline">{state.plan_error}</p>}{state?.collaboration?.mode === "plan" && <button className="primary" disabled={running || working || !active || !state.plan_hash} onClick={() => void command("execute")}>批准并执行</button>}</details>}
          {state?.pending && <Questionnaire key={state.pending.id} pending={state.pending} stopping={state.status === "cancelling"} submit={async answers => { await api<void>(`sessions/${selected}/answers`, { id: state.pending?.id, response: { answers } }); }} />}
          {running && !state?.pending && <p className="activity" role="status"><span className="activity-dot" />{statusText[state?.status ?? "running"]}</p>}
        </div>
      </div>
      </div>
      <div className="composer-area">
        {error && <div className="error-banner" role="alert"><span>{error.replace(/^Error: /, "")}</span><button className="icon-button" aria-label="关闭错误提示" onClick={() => setError("")}><Icon name="close" size={16} /></button></div>}
        {!error && state?.status === "interrupted" && state.error && <p className="error-inline" role="alert">上次任务因服务端退出而中断。请检查历史和文件状态，再决定是否继续。</p>}
        {selected && !loading && !active && <div className="open-banner"><span>{state?.occupied ? "这份会话已被另一个 Agent 占用。释放后才能继续。" : "正在查看历史。打开会话后继续工作。"}</span><button className="primary" disabled={working || state?.occupied} onClick={() => void openSession()}>打开会话<Icon name="arrow" size={16} /></button></div>}
        <form className={`composer ${selected && !active ? "inactive" : ""}`} onSubmit={event => { event.preventDefault(); void submit(); }}>
          <textarea ref={composer} aria-label="发送消息" placeholder={state?.pending ? "请先回答上方的问题" : "向 Iota 发送消息"} rows={2} value={draft} disabled={working || running || loading || (!!selected && !active)} onChange={event => setDraft(event.target.value)} onKeyDown={event => { if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void submit(); } }} />
          <div className="composer-tools"><label className="mode-select"><span className="sr-only">协作模式</span><select aria-label="协作模式" value={collaboration?.mode ?? initialMode} disabled={(!!selected && !active) || running || working || !config} onChange={event => { if (selected) void command("mode", undefined, event.target.value); else setInitialMode(event.target.value); }}><option value="default">Default</option><option value="plan">Plan</option></select></label><span className="composer-hint">Shift + Enter 换行</span>
            {running ? <button type="button" className="send-button" aria-label="停止任务" disabled={state?.status === "cancelling"} onClick={async () => { try { await api<void>(`sessions/${selected}/stop`, {}); } catch (error) { setError(String(error)); } }}><Icon name="stop" size={18} /></button> : <button className="send-button" type="submit" aria-label="发送消息" disabled={!draft.trim() || working || loading || (!!selected && !active) || !config}><Icon name="send" size={20} /></button>}
          </div>
        </form>
        <p className="composer-note">{selected ? (active ? "会话已由 Web 打开；释放后可在 CLI 继续。" : "会话历史与 CLI 共享。") : "使用与 CLI 相同的配置和工具。"}</p>
      </div>
    </main>
  </div>;
}

import { useState } from "react";
import type { RunIndex } from "./model";

const labels: Record<string, string> = { running: "进行中", completed: "已完成", aborted: "已停止", failed: "失败" };

export function RunNavigator({ runs, active, jump }: { runs: RunIndex[]; active: string; jump: (id: string) => void }) {
  const [preview, setPreview] = useState<{ run: RunIndex; top: number } | null>(null);
  return <nav className="run-navigation" aria-label="对话任务索引" onMouseLeave={() => setPreview(null)} onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) setPreview(null); }}>
    <div className="run-ticks">
      {runs.map((run, index) => <button key={run.id} className={`run-tick ${active === run.id ? "current" : ""}`} aria-label={`跳转到第 ${index + 1} 次任务：${run.title}`} aria-current={active === run.id ? "location" : undefined} aria-describedby={preview?.run.id === run.id ? "run-preview" : undefined}
        onMouseEnter={event => setPreview({ run, top: event.currentTarget.getBoundingClientRect().top })}
        onFocus={event => setPreview({ run, top: event.currentTarget.getBoundingClientRect().top })}
        onClick={() => jump(run.id)}><span /></button>)}
    </div>
    {preview && <div className="run-preview" id="run-preview" role="tooltip" style={{ top: Math.max(65, Math.min(preview.top - 25, window.innerHeight - 200)) }}>
      <strong>{preview.run.title}</strong>
      <p>{preview.run.preview || (preview.run.status === "running" ? "正在生成回复…" : "本次任务没有文字回复。")}</p>
      <small>{labels[preview.run.status] ?? preview.run.status} · 点击跳转</small>
    </div>}
  </nav>;
}

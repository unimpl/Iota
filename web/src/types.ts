export interface SessionSummary {
  name: string;
  title: string;
  created_at: string;
  model: string;
  cwd: string;
  occupied: boolean;
  open: boolean;
}

export interface Question {
  id: string;
  question: string;
  recommended_option_id?: string;
  options?: { id: string; label: string; description: string }[];
}

export interface PendingInput {
  id: string;
  request: { format: "structured" | "freeform"; questions: Question[] };
  call_index: number;
  call_count: number;
}

export interface Answer {
  source: "selected" | "recommended" | "custom" | "unanswered";
  option_id?: string;
  value?: string;
}

export interface Collaboration {
  mode: "plan" | "default";
  plan?: { id: string; path: string; content: string };
  progress?: { plan: { step_id: string; step: string; status: string }[] };
}

export interface SkillCatalog {
  skills?: { name: string; description: string }[];
  diagnostics?: { path: string; error: string }[];
}

export interface State {
  open: boolean;
  occupied: boolean;
  status: string;
  request_id?: string;
  error?: string;
  cwd: string;
  model: string;
  collaboration?: Collaboration;
  skills?: SkillCatalog;
  pending?: PendingInput;
  plan_hash?: string;
  plan_error?: string;
  plan_preview?: { id: string; path: string; content: string };
}

export interface EventPayload {
  text?: string;
  reasoning?: string;
  turn?: number;
  error?: string;
  is_error?: boolean;
  reason?: string;
  tool_call?: { id: string; name: string; arguments: unknown };
  tool_result?: string;
  message?: {
    role: "user" | "assistant" | "tool";
    content?: string;
    tool_call_id?: string;
    tool_name?: string;
    is_error?: boolean;
    skill?: unknown;
    context_summary?: boolean;
    runtime_context?: boolean;
  };
  request_id?: string;
  status?: string;
  output?: string;
  action?: string;
  collaboration?: Collaboration;
}

export interface SessionRecord {
  seq: number;
  run_id?: string;
  type: string;
  payload?: EventPayload;
}

export interface Command {
  request_id: string;
  action: "run" | "compact" | "reset" | "mode" | "execute" | "skills" | "skills_reload" | "skill_install";
  prompt?: string;
  mode?: string;
  plan_hash?: string;
}

export const busy = (state: State | null) => !!state && ["running", "waiting_input", "cancelling"].includes(state.status);

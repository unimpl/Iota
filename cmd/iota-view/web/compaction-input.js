// Older logs already contain the exact serialized history in the summary request.
// This fallback exposes it without treating summary-request usage as session size.
export function inputFromSummaryRequest(request, before) {
  const content = request?.messages?.[0]?.content;
  if (typeof content !== 'string') return null;
  const start = content.indexOf('<conversation>\n');
  const end = content.indexOf('\n</conversation>', start);
  if (start < 0 || end < 0) return null;
  const text = content.slice(start + '<conversation>\n'.length, end);
  let messages;
  try { messages = JSON.parse(text); } catch { return null; }
  if (!Array.isArray(messages) || !messages.every(message => message && typeof message.role === 'string' && (message.content == null || typeof message.content === 'string'))) return null;
  let units = 0;
  for (const character of text) units += character.codePointAt(0) <= 127 ? 1 : 4;
  return {
    messages,
    user_turns: messages.filter(message => message.role === 'user' && !message.context_summary).length,
    has_previous_summary: messages.some(message => message.context_summary),
    chars: Array.from(text).length,
    usage: { tokens: Math.ceil(units / 4), estimated: true },
    kept_messages: before?.before_messages == null ? null : Math.max(0, before.before_messages - messages.length),
    kept_turns: before?.keep_recent_turns ?? null,
    source: 'summary_request'
  };
}

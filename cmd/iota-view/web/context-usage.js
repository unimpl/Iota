// Capacity comes from provider errors; character counts alone cannot establish it.
export function usageLabel(usage, limit) {
  if (!usage?.tokens) return '上下文长度未知';
  let text = (usage.estimated ? '约 ' : '') + usage.tokens.toLocaleString('zh-CN') + ' tokens';
  if (limit > 0) text += ' / ' + limit.toLocaleString('zh-CN') + ' 上限';
  if (usage.remaining_percent != null) text += ' · ' + (usage.estimated ? '约 ' : '') + usage.remaining_percent.toFixed(1) + '% 可用';
  else text += ' · 可用百分比未知';
  return text;
}

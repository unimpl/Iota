// 日志可能缺少早期的模式切换记录；同一 run 的请求模式可补全请求前的事件。
function recordedMode(record) {
  const value = record.payload?.collaboration?.mode || record.payload?.request?.mode;
  return value === 'plan' || value === 'default' ? value : null;
}

export function timelineModes(records) {
  const firstRunModes = new Map();
  for (const record of records) {
    const mode = recordedMode(record);
    if (record.run_id && mode && !firstRunModes.has(record.run_id)) firstRunModes.set(record.run_id, mode);
  }
  const observedRunModes = new Map();
  const result = new Map();
  let current = 'default';
  for (const record of records) {
    const explicit = recordedMode(record);
    const mode = explicit || (record.run_id && (observedRunModes.get(record.run_id) || firstRunModes.get(record.run_id))) || current;
    result.set(record, { mode, previous: current });
    if (record.run_id) observedRunModes.set(record.run_id, mode);
    current = mode;
  }
  return result;
}

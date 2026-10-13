export class HttpError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

export async function api<T>(path: string, body?: unknown, method = body === undefined ? "GET" : "POST", signal?: AbortSignal): Promise<T> {
  const response = await fetch(`./api/${path}`, {
    method,
    headers: body === undefined && method === "GET" ? undefined : { "Content-Type": "application/json", "X-Iota-Client": "web" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal,
  });
  if (response.status === 204) return undefined as T;
  let result: unknown;
  try { result = await response.json(); }
  catch { throw new HttpError(`服务端返回了无效响应 (${response.status})，请检查连接。`, response.status); }
  if (!response.ok) {
    const message = typeof result === "object" && result !== null && "error" in result ? String(result.error) : `请求失败 (${response.status})`;
    throw new HttpError(message, response.status);
  }
  return result as T;
}

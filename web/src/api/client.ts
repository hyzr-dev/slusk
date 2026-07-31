// Auth needs no handling here: the server answers with WWW-Authenticate Basic,
// the browser then attaches ambient credentials to every same-origin request.
// See internal/observ/security.go.

import type { ApiErrorBody } from './types';

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
    readonly body?: ApiErrorBody,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

// Best-effort parse of a failed response's body, so ApiError can carry the
// server's own message. Most endpoints (e.g. POST /api/config) send
// structured JSON with per-field validation detail; the job action endpoints
// (retry/cancel/search/delete) answer with http.Error's plain text instead
// (see internal/observ/observ.go), so a JSON parse there would throw and
// silently drop the message — fall back to the raw text as `error` so both
// shapes surface through the same `body.error` field.
async function parseErrorBody(res: Response): Promise<ApiErrorBody | undefined> {
  const text = await res.text();
  if (!text) return undefined;
  try {
    return JSON.parse(text) as ApiErrorBody;
  } catch {
    return { error: text.trim() };
  }
}

export async function apiGet<T>(path: string): Promise<T> {
  const res = await fetch(path);
  if (!res.ok) {
    throw new ApiError(res.status, `GET ${path} failed with ${res.status}`, await parseErrorBody(res));
  }
  return (await res.json()) as T;
}

export async function apiPost(path: string): Promise<void> {
  const res = await fetch(path, { method: 'POST' });
  if (!res.ok) {
    throw new ApiError(res.status, `POST ${path} failed with ${res.status}`, await parseErrorBody(res));
  }
}

export async function apiDelete(path: string): Promise<void> {
  const res = await fetch(path, { method: 'DELETE' });
  if (!res.ok) {
    throw new ApiError(res.status, `DELETE ${path} failed with ${res.status}`, await parseErrorBody(res));
  }
}

// body, when given, is sent as a JSON request body; omit it for the existing
// empty-body POST endpoints (e.g. the connection tests).
export async function apiPostJson<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method: 'POST',
    ...(body !== undefined && {
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }),
  });
  if (!res.ok) {
    throw new ApiError(res.status, `POST ${path} failed with ${res.status}`, await parseErrorBody(res));
  }
  return (await res.json()) as T;
}

// Like apiPostJson, but for the auth endpoints (setup/login, issue #279),
// which answer 204 on success rather than a resource — see
// internal/observ/auth.go's serveAuthCreate. A plain apiPostJson<void> would
// still call res.json() on that empty body and throw.
export async function apiPostJsonNoContent(path: string, body: unknown): Promise<void> {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    throw new ApiError(res.status, `POST ${path} failed with ${res.status}`, await parseErrorBody(res));
  }
}

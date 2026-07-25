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

// Best-effort JSON parse of a failed response's body, so ApiError can carry
// structured detail (e.g. per-field validation messages) when the server
// sends one, without failing the throw itself if it didn't.
async function parseErrorBody(res: Response): Promise<ApiErrorBody | undefined> {
  try {
    return (await res.json()) as ApiErrorBody;
  } catch {
    return undefined;
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

// Auth needs no handling here: the server answers with WWW-Authenticate Basic,
// the browser then attaches ambient credentials to every same-origin request.
// See internal/observ/security.go.

export class ApiError extends Error {
  constructor(readonly status: number, message: string) {
    super(message);
    this.name = 'ApiError';
  }
}

export async function apiGet<T>(path: string): Promise<T> {
  const res = await fetch(path);
  if (!res.ok) {
    throw new ApiError(res.status, `GET ${path} failed with ${res.status}`);
  }
  return (await res.json()) as T;
}

export async function apiPost(path: string): Promise<void> {
  const res = await fetch(path, { method: 'POST' });
  if (!res.ok) {
    throw new ApiError(res.status, `POST ${path} failed with ${res.status}`);
  }
}

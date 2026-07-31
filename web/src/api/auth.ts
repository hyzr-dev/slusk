// Frontend half of form-based login (issue #279). Mirrors the shape of
// queries.ts's hooks, kept in its own file since AuthGate (App.tsx) is the
// only consumer of most of it and the four endpoints here are the only
// public ones — see internal/observ/auth.go.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiGet, apiPost, apiPostJsonNoContent } from './client';
import type { SessionResponse } from './types';

export const authQueryKeys = {
  session: ['auth', 'session'] as const,
};

export interface Credentials {
  username: string;
  password: string;
}

// The SPA's boot-time gate (see AuthGate in App.tsx): whether a session
// cookie or bearer token is present, and whether any account exists yet.
// Always public (see isPrivatePath in security.go), so this never 401s on
// its own — a failure here is a genuine network/server problem, not "logged
// out", and defaultOptions.queries.retry (App.tsx) applies as normal.
export function useSession() {
  return useQuery({
    queryKey: authQueryKeys.session,
    queryFn: () => apiGet<SessionResponse>('/api/auth/session'),
  });
}

// Shared by useLogin/useSetup: both endpoints take the same credentials body
// and answer 204 with a Set-Cookie header (see internal/observ/auth.go's
// serveAuthCreate), and both need the same follow-up — refetch the session
// query so AuthGate picks up the newly authenticated state.
function useCredentialsMutation(path: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (credentials: Credentials) => apiPostJsonNoContent(path, credentials),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: authQueryKeys.session });
    },
  });
}

export function useLogin() {
  return useCredentialsMutation('/api/auth/login');
}

export function useSetup() {
  return useCredentialsMutation('/api/auth/setup');
}

export function useLogout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiPost('/api/auth/logout'),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: authQueryKeys.session });
    },
  });
}

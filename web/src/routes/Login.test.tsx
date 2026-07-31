import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { t } from '../strings';
import Login from './Login';

afterEach(() => vi.unstubAllGlobals());

function renderLogin(mode: 'login' | 'setup' = 'login') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <Login mode={mode} />
    </QueryClientProvider>,
  );
}

describe('Login', () => {
  it('rejects a short password client-side without making a request', () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    renderLogin('login');

    fireEvent.change(screen.getByPlaceholderText(t.auth.usernamePlaceholder), {
      target: { value: 'admin' },
    });
    fireEvent.change(screen.getByPlaceholderText(t.auth.passwordPlaceholder), {
      target: { value: 'short' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.auth.signIn }));

    expect(screen.getByRole('alert')).toHaveTextContent(t.auth.passwordTooShort);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('submits credentials to /api/auth/login and surfaces the server rejection on 401', async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(new Response(JSON.stringify({ error: 'invalid username or password' }), { status: 401 })),
    );
    vi.stubGlobal('fetch', fetchMock);
    renderLogin('login');

    fireEvent.change(screen.getByPlaceholderText(t.auth.usernamePlaceholder), {
      target: { value: 'admin' },
    });
    fireEvent.change(screen.getByPlaceholderText(t.auth.passwordPlaceholder), {
      target: { value: 'wrongpassword' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.auth.signIn }));

    expect(await screen.findByText('invalid username or password')).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/auth/login',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ username: 'admin', password: 'wrongpassword' }),
      }),
    );
  });

  it('clears a previous error as soon as a field changes again', () => {
    vi.stubGlobal('fetch', vi.fn());
    renderLogin('login');

    fireEvent.click(screen.getByRole('button', { name: t.auth.signIn }));
    expect(screen.getByRole('alert')).toHaveTextContent(t.auth.usernameRequired);

    fireEvent.change(screen.getByPlaceholderText(t.auth.usernamePlaceholder), {
      target: { value: 'a' },
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  describe('setup mode', () => {
    it('renders a confirm-password field and the no-reset warning, not present in login mode', () => {
      renderLogin('setup');
      expect(screen.getByText(t.auth.confirmPasswordLabel)).toBeInTheDocument();
      expect(screen.getByText(t.auth.setupWarning)).toBeInTheDocument();
      expect(screen.getByRole('heading', { name: t.auth.setupHeader })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.auth.createAccount })).toBeInTheDocument();
    });

    it('rejects a mismatched confirmation before making a request', () => {
      const fetchMock = vi.fn();
      vi.stubGlobal('fetch', fetchMock);
      renderLogin('setup');

      fireEvent.change(screen.getByPlaceholderText(t.auth.usernamePlaceholder), {
        target: { value: 'admin' },
      });
      const passwordFields = screen.getAllByPlaceholderText(t.auth.passwordPlaceholder);
      fireEvent.change(passwordFields[0], { target: { value: 'goodpassword1' } });
      fireEvent.change(passwordFields[1], { target: { value: 'different-password' } });
      fireEvent.click(screen.getByRole('button', { name: t.auth.createAccount }));

      expect(screen.getByRole('alert')).toHaveTextContent(t.auth.passwordMismatch);
      expect(fetchMock).not.toHaveBeenCalled();
    });
  });
});

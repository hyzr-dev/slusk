import { useState } from 'react';
import type { FormEvent } from 'react';
import { useLogin, useSetup } from '../api/auth';
import { ApiError } from '../api/client';
import { t } from '../strings';
import styles from './Login.module.css';

// Mirrors app.Auth's own limits (internal/app/auth.go: minPasswordBytes,
// maxPasswordBytes, maxUsernameBytes) so the common mistakes surface
// instantly instead of round-tripping to the server — but this is a fast
// path, not the authority: the server's own rejection is always shown too
// (see handleSubmit), since these constants could drift from the Go side.
const MIN_PASSWORD_BYTES = 8;
const MAX_PASSWORD_BYTES = 72;
const MAX_USERNAME_BYTES = 64;

// Byte length, not `.length` — maxPasswordBytes is a bcrypt limit measured
// in bytes, and a multi-byte character would otherwise pass a client check
// that the server then rejects.
function byteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

export type LoginMode = 'login' | 'setup';

/**
 * The login card and, with `mode="setup"`, the first-run account-creation
 * card — one component per docs/design/slusk-login.dc.html, which draws
 * both as the same 340px bordered panel with a different header/fields/button.
 * Rendered by AuthGate (App.tsx) in place of the router outlet, so it owns
 * the page's only <h1> rather than reusing SectionHeader's <h2> (that
 * component assumes a <Page> above it already supplied one).
 *
 * Two deliberate deviations from the mock, decided with the user rather than
 * missed: no "remember this session" checkbox (sessions are a fixed 90-day
 * TTL, see app.SessionTTL — there is nothing for it to toggle), and no
 * "forgot password?" link (v1 has no reset flow; a link to nowhere would be
 * exactly the kind of invented affordance CLAUDE.md's design principles rule
 * out).
 */
export default function Login({ mode }: { mode: LoginMode }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [clientError, setClientError] = useState('');

  const login = useLogin();
  const setup = useSetup();
  const mutation = mode === 'setup' ? setup : login;

  const serverError = mutation.isError
    ? mutation.error instanceof ApiError
      ? (mutation.error.body?.error ?? mutation.error.message)
      : t.auth.genericError
    : '';
  const errorText = clientError || serverError;

  function clearErrors() {
    setClientError('');
    if (mutation.isError) mutation.reset();
  }

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const trimmedUsername = username.trim();
    if (!trimmedUsername || !password) {
      setClientError(t.auth.usernameRequired);
      return;
    }
    // Length and shape rules apply to setup ONLY. They are policy for a
    // password being CREATED, and on the login form they would be three
    // separate mistakes: they claim an existing credential is malformed when
    // it is merely wrong, they hand an unauthenticated visitor the policy the
    // server deliberately withholds by answering 401 identically for a wrong
    // password and an unknown user, and — worst — they short-circuit before
    // the request, so tightening a limit later would lock out every account
    // created under the old one without the server ever being asked. There is
    // no password reset to recover from that.
    if (mode === 'setup') {
      if (trimmedUsername.length > MAX_USERNAME_BYTES) {
        setClientError(t.auth.usernameRequired);
        return;
      }
      if (byteLength(password) < MIN_PASSWORD_BYTES) {
        setClientError(t.auth.passwordTooShort);
        return;
      }
      if (byteLength(password) > MAX_PASSWORD_BYTES) {
        setClientError(t.auth.passwordTooLong);
        return;
      }
      if (password !== confirmPassword) {
        setClientError(t.auth.passwordMismatch);
        return;
      }
    }
    setClientError('');
    mutation.mutate({ username: trimmedUsername, password });
  }

  return (
    <div className={styles.root}>
      <div className={styles.brandRow}>
        <span className={styles.brandName}>{t.auth.brand}</span>
      </div>
      <div className={styles.subtitleRow}>
        <span className={styles.pulse} />
        <span className={styles.subtitle}>
          {mode === 'setup' ? t.auth.setupSubtitle : t.auth.loginSubtitle}
        </span>
      </div>

      <form className={styles.card} onSubmit={handleSubmit}>
        <div className={styles.cardHeader}>
          <span className={styles.dash} aria-hidden="true">─</span>
          <h1 className={styles.cardTitle}>
            {mode === 'setup' ? t.auth.setupHeader : t.auth.loginHeader}
          </h1>
        </div>

        <div className={styles.fields}>
          <label className={styles.field}>
            <span className={styles.label}>{t.auth.usernameLabel}</span>
            <div className={styles.inputBox}>
              <span className={styles.prompt} aria-hidden="true">&gt;</span>
              <input
                className={styles.input}
                value={username}
                onChange={(e) => {
                  setUsername(e.target.value);
                  clearErrors();
                }}
                placeholder={t.auth.usernamePlaceholder}
                autoComplete="username"
                autoFocus
              />
            </div>
          </label>

          <label className={styles.field}>
            <span className={styles.label}>{t.auth.passwordLabel}</span>
            <div className={styles.inputBox}>
              <span className={styles.prompt} aria-hidden="true">&gt;</span>
              <input
                className={styles.input}
                type="password"
                value={password}
                onChange={(e) => {
                  setPassword(e.target.value);
                  clearErrors();
                }}
                placeholder={t.auth.passwordPlaceholder}
                autoComplete={mode === 'setup' ? 'new-password' : 'current-password'}
              />
            </div>
          </label>

          {mode === 'setup' && (
            <label className={styles.field}>
              <span className={styles.label}>{t.auth.confirmPasswordLabel}</span>
              <div className={styles.inputBox}>
                <span className={styles.prompt} aria-hidden="true">&gt;</span>
                <input
                  className={styles.input}
                  type="password"
                  value={confirmPassword}
                  onChange={(e) => {
                    setConfirmPassword(e.target.value);
                    clearErrors();
                  }}
                  placeholder={t.auth.passwordPlaceholder}
                  autoComplete="new-password"
                />
              </div>
            </label>
          )}

          {mode === 'setup' && <div className={styles.warning}>{t.auth.setupWarning}</div>}

          {errorText && (
            <div className={styles.error} role="alert">
              {errorText}
            </div>
          )}
        </div>

        <div className={styles.actions}>
          <button type="submit" className={styles.submit} disabled={mutation.isPending}>
            {mutation.isPending
              ? t.auth.submitting
              : mode === 'setup'
                ? t.auth.createAccount
                : t.auth.signIn}
          </button>
        </div>
      </form>

      <div className={styles.footer}>{t.auth.footer}</div>
    </div>
  );
}

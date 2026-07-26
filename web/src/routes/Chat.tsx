import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ApiError } from '../api/client';
import { useConversations, useMarkConversationRead, useSendMessage, useThread } from '../api/queries';
import Button from '../components/tui/Button';
import EmptyState from '../components/tui/EmptyState';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import SectionHeader from '../components/tui/SectionHeader';
import { formatShortTime } from '../format';
import { t } from '../strings';
import styles from './Chat.module.css';

/**
 * Private-message chat view (issue #183): a conversation rail on the left, one
 * thread's messages plus a composer on the right. Deviates from the mock
 * (docs/design/slskdarr-tui.dc.html, the CHAT block and chatVals()) in four
 * deliberate ways:
 *
 *  1. No online/offline dot in the rail and no ONLINE/OFFLINE chip in the
 *     thread header — Conversation/PrivateMessage carry no presence field,
 *     and inventing one would show a lie (see repo memory
 *     interface-must-not-invent-data).
 *  2. A quiet "load older" control at the top of the pane when hasMore is
 *     true. The mock draws no history affordance, but the API's hasMore/
 *     before= paging exists specifically to be used.
 *  3. Sending latches into a disabled state on a 503 (sending private
 *     messages not enabled) — nothing advertises this in advance, so one
 *     failed attempt is the only way the UI can learn it.
 *  4. The composer is a real <form onSubmit>, not a bare input with
 *     onKeyDown, so Enter-to-send and a mouse click both go through one path.
 *
 * jsdom computes no layout and paints nothing (see CLAUDE.md), so the three
 * scroll behaviors below — jump to bottom on thread switch, conditional
 * autoscroll on a new message, and position-preserving prepend on
 * "load older" — cannot be exercised by this file's tests. They were verified
 * by hand in a browser; see the `verifying-ui-in-browser` skill.
 */

// Mirrors internal/core.MaxPrivateMessageBytes. web/ has no access to Go
// constants, so this is a literal copy — internal/core.MaxPrivateMessageBytes
// is the source of truth if it ever changes.
const MAX_MESSAGE_BYTES = 8192;

// How close to the bottom (in pixels) counts as "already at the bottom" for
// the autoscroll-on-new-message behavior below.
const NEAR_BOTTOM_PX = 40;

export default function Chat() {
  const { username } = useParams<{ username?: string }>();
  const navigate = useNavigate();

  const conversationsQuery = useConversations();
  const conversations = conversationsQuery.data ?? [];
  const conversationsPhase = queryPhase(conversationsQuery);
  const status503 =
    conversationsQuery.isError &&
    conversationsQuery.error instanceof ApiError &&
    conversationsQuery.error.status === 503;

  const threadQuery = useThread(username);
  const threadPhase = queryPhase(threadQuery);
  // Reverse page order (oldest page first), then reverse within each page
  // (oldest message first): the API serves everything newest-first, but the
  // pane reads top-to-bottom chronologically like any chat UI.
  const messages = useMemo(
    () => (threadQuery.data?.pages ?? []).slice().reverse().flatMap((p) => p.messages.slice().reverse()),
    [threadQuery.data],
  );

  const markRead = useMarkConversationRead();
  const markedKeyRef = useRef<string | null>(null);
  const newestIncomingId = messages.reduce(
    (max, m) => (m.direction === 'IN' && m.id > max ? m.id : max),
    0,
  );
  useEffect(() => {
    if (username === undefined || newestIncomingId === 0) return;
    const key = `${username}:${newestIncomingId}`;
    // Guards against re-posting on every THREAD_INTERVAL poll that returns
    // nothing new — only a genuinely new newest incoming message (or a
    // thread switch) changes this key.
    if (markedKeyRef.current === key) return;
    markedKeyRef.current = key;
    markRead.mutate(username);
    // markRead.mutate is intentionally omitted from deps: it is a mutation
    // function whose identity is not meaningful to this effect's guard.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [username, newestIncomingId]);

  // /chat with no thread picked yet: once the conversation list has resolved
  // and is non-empty, land on the most recently active one. `replace` is
  // mandatory — otherwise every visit to /chat becomes a back-button trap
  // through an intermediate history entry the user never asked for.
  useEffect(() => {
    if (username !== undefined) return;
    if (!hasData(conversationsPhase)) return;
    if (conversationsQuery.data === undefined || conversationsQuery.data.length === 0) return;
    navigate(`/chat/${encodeURIComponent(conversationsQuery.data[0].username)}`, { replace: true });
  }, [username, conversationsPhase, conversationsQuery.data, navigate]);

  const paneRef = useRef<HTMLDivElement>(null);
  const nearBottomRef = useRef(true);
  const prevScrollHeightRef = useRef(0);
  const pageCount = threadQuery.data?.pages.length ?? 0;
  const prevPageCountRef = useRef(pageCount);

  // 1) Thread switch: always jump to the bottom, regardless of scroll state.
  useLayoutEffect(() => {
    const el = paneRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [username]);

  // 2) A new message arrived: only follow it down if the reader was already
  // near the bottom. nearBottomRef is updated by handleScroll below, so it
  // reflects the position from *before* this render's DOM change — the only
  // way to know "was the user reading history" rather than "where did the
  // prepended/newly-taller content just land".
  const lastMessageId = messages.at(-1)?.id;
  useLayoutEffect(() => {
    const el = paneRef.current;
    if (el && nearBottomRef.current) el.scrollTop = el.scrollHeight;
  }, [lastMessageId]);

  // 3) "Load older" prepended a page: restore the reader's viewport over the
  // same content rather than letting the prepend shove it down.
  useLayoutEffect(() => {
    const el = paneRef.current;
    if (el && pageCount > prevPageCountRef.current) {
      el.scrollTop += el.scrollHeight - prevScrollHeightRef.current;
    }
    prevPageCountRef.current = pageCount;
  }, [pageCount]);

  function handleScroll() {
    const el = paneRef.current;
    if (!el) return;
    nearBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < NEAR_BOTTOM_PX;
  }

  function handleLoadOlder() {
    const el = paneRef.current;
    if (el) prevScrollHeightRef.current = el.scrollHeight;
    void threadQuery.fetchNextPage();
  }

  if (status503) {
    return <div role="status">{t.chat.disabledNotice}</div>;
  }

  return (
    <div className={styles.root}>
      <div className={styles.rail} aria-label={t.chat.railHeading}>
        <SectionHeader label={t.chat.railHeading} />
        <QueryNotice phase={conversationsPhase} />
        {hasData(conversationsPhase) && conversations.length === 0 && (
          <EmptyState message={t.chat.empty} />
        )}
        {hasData(conversationsPhase) &&
          conversations.map((c) => {
            const active = c.username === username;
            return (
              <Link
                key={c.username}
                to={`/chat/${encodeURIComponent(c.username)}`}
                className={active ? styles.rowActive : styles.row}
                aria-current={active ? 'page' : undefined}
                aria-label={t.chat.threadLabel(c.username, c.unread)}
              >
                <span className={styles.rowName}>{c.username}</span>
                {c.unread > 0 && <span className={styles.rowUnread}>{c.unread}</span>}
              </Link>
            );
          })}
      </div>

      {username !== undefined && (
        // A single key on this wrapper — not on the log div and Composer
        // separately — remounts both together on a thread switch. Two
        // sibling elements sharing the same literal key value (both keyed
        // by `username`) is invalid and confuses React's reconciliation:
        // it produces "duplicate key" warnings and can leave a stale
        // sibling's DOM node behind instead of removing it. One key on
        // their shared parent gets the same remount with no such risk.
        <div key={username} className={styles.pane}>
          <div className={styles.paneHeaderWrap}>
            <SectionHeader label={username} />
          </div>
          <div
            ref={paneRef}
            role="log"
            aria-live="polite"
            aria-relevant="additions"
            aria-label={username}
            className={styles.messages}
            onScroll={handleScroll}
          >
            {threadQuery.hasNextPage && (
              <button
                type="button"
                className={styles.loadOlder}
                onClick={handleLoadOlder}
                disabled={threadQuery.isFetchingNextPage}
              >
                {t.chat.loadOlder}
              </button>
            )}
            <QueryNotice phase={threadPhase} />
            {hasData(threadPhase) && messages.length === 0 && (
              <EmptyState message={t.chat.threadEmpty} />
            )}
            {hasData(threadPhase) &&
              messages.map((m) => (
                <div key={m.id} className={styles.messageLine}>
                  <span className={styles.messageTime}>{formatShortTime(m.sentAt)}</span>
                  <span className={m.direction === 'OUT' ? styles.whoYou : styles.whoPeer}>
                    {m.direction === 'OUT' ? t.chat.you : t.chat.peer(username)}
                  </span>
                  <span className={m.direction === 'OUT' ? styles.bodyOut : styles.bodyIn}>
                    {m.body}
                  </span>
                </div>
              ))}
          </div>
          <Composer username={username} />
        </div>
      )}
    </div>
  );
}

function Composer({ username }: { username: string }) {
  const [draft, setDraft] = useState('');
  const [sendDisabled, setSendDisabled] = useState(false);
  const [error, setError] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const send = useSendMessage(username);

  const trimmed = draft.trim();
  const bytes = new TextEncoder().encode(trimmed).length;
  const tooLong = bytes > MAX_MESSAGE_BYTES;
  const canSend = trimmed !== '' && !tooLong && !sendDisabled && !send.isPending;

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (!canSend) return;
    send.mutate(trimmed, {
      onSuccess: () => {
        setDraft('');
        setError('');
        inputRef.current?.focus();
      },
      onError: (err) => {
        const status = err instanceof ApiError ? err.status : 0;
        if (status === 503) {
          setSendDisabled(true);
          setError(t.chat.sendDisabled);
        } else if (status === 422) {
          setError(t.chat.sendRejected);
        } else {
          // Covers 502 (send failed upstream) and anything else (400,
          // network error) with the same generic copy — the draft is
          // preserved in every one of these cases so nothing is lost.
          setError(t.chat.sendFailed);
        }
      },
    });
  }

  return (
    <form className={styles.composer} onSubmit={handleSubmit}>
      <div role="status" className={error ? styles.composerError : undefined}>
        {error || (tooLong ? t.chat.tooLong : '')}
      </div>
      <div className={styles.composerRow}>
        <span aria-hidden className={styles.prompt}>
          &gt;
        </span>
        <label className={styles.visuallyHidden} htmlFor="chat-draft">
          {t.chat.composerLabel}
        </label>
        <input
          id="chat-draft"
          ref={inputRef}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder={t.chat.composerPlaceholder}
          disabled={sendDisabled}
          className={styles.input}
        />
        <Button type="submit" variant="primary" disabled={!canSend}>
          {t.chat.send}
        </Button>
      </div>
    </form>
  );
}

import { Fragment, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ApiError } from '../api/client';
import { useConversations, useMarkConversationRead, useSendMessage, useThread } from '../api/queries';
import Button from '../components/tui/Button';
import EmptyState from '../components/tui/EmptyState';
import QueryNotice, { hasData, queryPhase } from '../components/tui/QueryNotice';
import SectionHeader from '../components/tui/SectionHeader';
import { formatShortTime, localDayKey } from '../format';
import { t } from '../strings';
import styles from './Chat.module.css';

/**
 * Private-message chat view (issue #183): a conversation rail on the left, one
 * thread's messages plus a composer on the right. Deviates from the mock
 * (docs/design/slskdarr-tui.dc.html, the CHAT block and chatVals()) in three
 * deliberate ways:
 *
 *  1. A quiet "load older" control at the top of the pane when hasMore is
 *     true. The mock draws no history affordance, but the API's hasMore/
 *     before= paging exists specifically to be used.
 *  2. Sending latches into a disabled state on a 503 (sending private
 *     messages not enabled) — nothing advertises this in advance, so one
 *     failed attempt is the only way the UI can learn it.
 *  3. The composer is a real <form onSubmit>, not a bare input with
 *     onKeyDown, so Enter-to-send and a mouse click both go through one path.
 *
 * Presence follows the mock only when Conversation.online is defined. An
 * absent value is unknown or unsupported and must not be presented as
 * offline.
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

function messageDraftState(draft: string) {
  const body = draft.trim();
  return {
    body,
    tooLong: new TextEncoder().encode(body).length > MAX_MESSAGE_BYTES,
  };
}

function sendErrorState(error: unknown) {
  const status = error instanceof ApiError ? error.status : 0;
  if (status === 503) return { message: t.chat.sendDisabled, disable: true };
  if (status === 422) return { message: t.chat.sendRejected, disable: false };
  return { message: t.chat.sendFailed, disable: false };
}

// How close to the bottom (in pixels) counts as "already at the bottom" for
// the autoscroll-on-new-message behavior below.
const NEAR_BOTTOM_PX = 40;

/**
 * The `── today ──` rule between two calendar days in a thread (issue #247).
 *
 * The dashes are added here, not baked into the strings, so the decoration
 * stays a styling decision — the same split EmptyState documents. Only the two
 * nameable days get words; anything older shows its sv-SE date, which is what
 * localDayKey already returns.
 *
 * `role="separator"` rather than a bare div: this sits inside the message
 * pane's `role="log" aria-live="polite"`, so a divider appended along with the
 * first message of a new day would otherwise be announced as if it were a
 * message. The aria-label carries the day, because a screen reader reading
 * "dash dash dash" is not information.
 */
function DayDivider({ day }: { day: string }) {
  const now = new Date();
  const today = localDayKey(now.toISOString());
  // setDate, not a 24h subtraction: a DST day is 23 or 25 hours long, and
  // subtracting a fixed 86 400 000 ms across the transition can land back on
  // today or skip to the day before yesterday.
  const prev = new Date(now);
  prev.setDate(prev.getDate() - 1);
  const yesterday = localDayKey(prev.toISOString());
  const label = day === today ? t.chat.today : day === yesterday ? t.chat.yesterday : day;
  return (
    <div role="separator" aria-label={label} className={styles.dayDivider}>
      {`── ${label} ──`}
    </div>
  );
}

export default function Chat() {
  const { username } = useParams<{ username?: string }>();
  const navigate = useNavigate();
  const [starterPending, setStarterPending] = useState(false);

  const conversationsQuery = useConversations();
  const conversations = conversationsQuery.data ?? [];
  const selectedConversation = conversations.find((c) => c.username === username);
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

  // TanStack Query v5's mutate is referentially stable across renders, so it
  // can sit in the deps array below like any other value without re-firing
  // the effect on every render.
  const { mutate: markRead } = useMarkConversationRead();
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
    markRead(username);
  }, [username, newestIncomingId, markRead]);

  // /chat with no thread picked yet: once the conversation list has resolved
  // and is non-empty, land on the most recently active one. `replace` is
  // mandatory — otherwise every visit to /chat becomes a back-button trap
  // through an intermediate history entry the user never asked for.
  useEffect(() => {
    if (username !== undefined) {
      if (starterPending) setStarterPending(false);
      return;
    }
    // Starting from an empty rail owns navigation until its POST settles.
    // Otherwise the successful conversations refetch can race ahead and
    // turn the user-initiated PUSH into this effect's initial-list REPLACE.
    if (starterPending) return;
    if (!hasData(conversationsPhase)) return;
    if (conversationsQuery.data === undefined || conversationsQuery.data.length === 0) return;
    navigate(`/chat/${encodeURIComponent(conversationsQuery.data[0].username)}`, { replace: true });
  }, [username, conversationsPhase, conversationsQuery.data, navigate, starterPending]);

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
    return (
      <div role="status">
        <EmptyState message={t.chat.disabledNotice} />
      </div>
    );
  }

  return (
    <div className={styles.root}>
      <div className={styles.rail}>
        <SectionHeader label={t.chat.railHeading} />
        <NewConversation onPendingChange={setStarterPending} />
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
                aria-label={t.chat.threadLabel(c.username, c.unread, c.online)}
              >
                {c.online !== undefined && (
                  <span
                    aria-hidden="true"
                    className={`${styles.presenceDot} ${c.online ? styles.presenceOnline : styles.presenceOffline}`}
                  >
                    ■
                  </span>
                )}
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
          <SectionHeader
            label={username}
            truncateLabel
            meta={
              selectedConversation?.online !== undefined ? (
                <span
                  className={`${styles.presenceChip} ${selectedConversation.online ? styles.presenceOnline : styles.presenceOffline}`}
                >
                  {selectedConversation.online ? t.chat.online : t.chat.offline}
                </span>
              ) : undefined
            }
          />
          {threadQuery.hasNextPage && (
            // Outside the role="log" region below, deliberately: it is a
            // pagination control, not a new-message announcement, and
            // living inside aria-relevant="additions" would make a screen
            // reader read the entire freshly-prepended page aloud.
            <div className={styles.loadOlderWrap}>
              <Button
                type="button"
                variant="ghost"
                onClick={handleLoadOlder}
                disabled={threadQuery.isFetchingNextPage}
              >
                {t.chat.loadOlder}
              </Button>
            </div>
          )}
          <div
            ref={paneRef}
            role="log"
            aria-live="polite"
            aria-relevant="additions"
            aria-label={username}
            className={styles.messages}
            onScroll={handleScroll}
          >
            <QueryNotice phase={threadPhase} />
            {hasData(threadPhase) && messages.length === 0 && (
              <EmptyState message={t.chat.threadEmpty} />
            )}
            {hasData(threadPhase) &&
              messages.map((m, i) => {
                // A divider is emitted whenever the local day changes, and
                // always before the first message: a thread whose every
                // message predates today would otherwise still carry no date
                // at all, which is the whole bug (#247).
                const day = localDayKey(m.sentAt);
                const showDivider = i === 0 || day !== localDayKey(messages[i - 1].sentAt);
                return (
                  <Fragment key={m.id}>
                    {showDivider && <DayDivider day={day} />}
                    <div className={styles.messageLine}>
                      <span className={styles.messageTime}>{formatShortTime(m.sentAt)}</span>
                      <span className={m.direction === 'OUT' ? styles.whoYou : styles.whoPeer}>
                        {m.direction === 'OUT' ? t.chat.you : t.chat.peer(username)}
                      </span>
                      <span className={m.direction === 'OUT' ? styles.bodyOut : styles.bodyIn}>
                        {m.body}
                      </span>
                    </div>
                  </Fragment>
                );
              })}
          </div>
          <Composer username={username} />
        </div>
      )}
    </div>
  );
}

function NewConversation({ onPendingChange }: { onPendingChange: (pending: boolean) => void }) {
  const navigate = useNavigate();
  const [username, setUsername] = useState('');
  const [draft, setDraft] = useState('');
  const [sendDisabled, setSendDisabled] = useState(false);
  const [error, setError] = useState('');
  const trimmedUsername = username.trim();
  const message = messageDraftState(draft);
  const send = useSendMessage();
  const canSend =
    trimmedUsername !== '' &&
    message.body !== '' &&
    !message.tooLong &&
    !sendDisabled &&
    !send.isPending;

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (!canSend) return;
    onPendingChange(true);
    send.mutate(
      { username: trimmedUsername, body: message.body },
      {
        onSuccess: (_message, { username: submittedUsername }) => {
          setUsername('');
          setDraft('');
          setError('');
          navigate(`/chat/${encodeURIComponent(submittedUsername)}`);
        },
        onError: (err) => {
          onPendingChange(false);
          const errorState = sendErrorState(err);
          if (errorState.disable) setSendDisabled(true);
          setError(errorState.message);
        },
      },
    );
  }

  return (
    <form className={styles.newConversation} onSubmit={handleSubmit}>
      <label className={styles.visuallyHidden} htmlFor="new-conversation-username">
        {t.chat.newConversationUsernameLabel}
      </label>
      <input
        id="new-conversation-username"
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        placeholder={t.chat.newConversationUsernamePlaceholder}
        disabled={sendDisabled || send.isPending}
        className={styles.newConversationInput}
      />
      <label className={styles.visuallyHidden} htmlFor="new-conversation-message">
        {t.chat.newConversationMessageLabel}
      </label>
      <input
        id="new-conversation-message"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        placeholder={t.chat.newConversationMessagePlaceholder}
        disabled={sendDisabled || send.isPending}
        className={styles.newConversationInput}
      />
      <div role="status" className={error || message.tooLong ? styles.newConversationError : undefined}>
        {error || (message.tooLong ? t.chat.tooLong : '')}
      </div>
      <div className={styles.newConversationActions}>
        <Button type="submit" variant="primary" disabled={!canSend}>
          {t.chat.newConversationSubmit}
        </Button>
      </div>
    </form>
  );
}

function Composer({ username }: { username: string }) {
  const [draft, setDraft] = useState('');
  const [sendDisabled, setSendDisabled] = useState(false);
  const [error, setError] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);
  const send = useSendMessage();

  const message = messageDraftState(draft);
  const canSend = message.body !== '' && !message.tooLong && !sendDisabled && !send.isPending;

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (!canSend) return;
    send.mutate({ username, body: message.body }, {
      onSuccess: () => {
        setDraft('');
        setError('');
        inputRef.current?.focus();
      },
      onError: (err) => {
        const errorState = sendErrorState(err);
        if (errorState.disable) setSendDisabled(true);
        setError(errorState.message);
      },
    });
  }

  return (
    <form className={styles.composer} onSubmit={handleSubmit}>
      <div role="status" className={error ? styles.composerError : undefined}>
        {error || (message.tooLong ? t.chat.tooLong : '')}
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
          {/* Decoration only — see strings.ts's comment on t.chat.send — so it
              is hidden from the accessible name and the string carries only
              the label. */}
          <span aria-hidden="true">[⏎] </span>
          {t.chat.send}
        </Button>
      </div>
    </form>
  );
}

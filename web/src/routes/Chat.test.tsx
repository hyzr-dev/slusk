import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useNavigationType } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { queryKeys } from '../api/queries';
import type { Conversation, PrivateMessage, ThreadPage } from '../api/types';
import { t } from '../strings';
import Chat from './Chat';

afterEach(() => vi.unstubAllGlobals());

function makeConversation(overrides: Partial<Conversation> = {}): Conversation {
  return {
    username: 'alice',
    lastMessage: 'hi',
    lastMessageAt: '2026-01-01T00:00:00Z',
    lastDirection: 'IN',
    unread: 0,
    total: 1,
    ...overrides,
  };
}

function makeMessage(overrides: Partial<PrivateMessage> = {}): PrivateMessage {
  return {
    id: 1,
    username: 'alice',
    direction: 'IN',
    body: 'hi',
    sentAt: '2026-01-01T00:00:00Z',
    read: false,
    admin: false,
    ...overrides,
  };
}

// The infinite-query cache shape TanStack Query expects: pageParams mirrors
// what getNextPageParam would have produced, i.e. each page's cursor is the
// previous page's oldest (= last, since pages are newest-first) message id.
function seedThread(client: QueryClient, username: string, pages: ThreadPage[]) {
  client.setQueryData(queryKeys.thread(username), {
    pages,
    pageParams: pages.map((_, i) => (i === 0 ? 0 : pages[i - 1].messages.at(-1)!.id)),
  });
}

function newClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

function stubFetchIndefinitely() {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})));
}

function okJson(body: unknown, status = 200): Response {
  return {
    ok: true,
    status,
    text: () => Promise.resolve(JSON.stringify(body)),
    json: () => Promise.resolve(body),
  } as Response;
}

function errJson(status: number, body: unknown): Response {
  return {
    ok: false,
    status,
    text: () => Promise.resolve(JSON.stringify(body)),
    json: () => Promise.resolve(body),
  } as Response;
}

function renderChat(path: string, queryClient: QueryClient) {
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="chat/:username?" element={<Chat />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('Chat query states', () => {
  it('shows the loading line, not an empty state, before the first fetch resolves', () => {
    stubFetchIndefinitely();
    renderChat('/chat/alice', newClient());
    expect(screen.getAllByText(t.query.loading).length).toBeGreaterThan(0);
    expect(screen.queryByText(t.chat.empty, { exact: false })).not.toBeInTheDocument();
  });

  it('shows the disabled notice on a 503, with no rail and no composer', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(errJson(503, { error: 'not enabled' }))));
    renderChat('/chat', newClient());
    expect(await screen.findByText(t.chat.disabledNotice)).toBeInTheDocument();
    expect(screen.queryByText(t.chat.railHeading)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.chat.composerLabel)).not.toBeInTheDocument();
  });

  it('shows an empty state with no composer and does not redirect when there are no conversations', async () => {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, []);
    stubFetchIndefinitely();
    renderChat('/chat', client);
    expect(await screen.findByText(t.chat.empty, { exact: false })).toBeInTheDocument();
    expect(screen.queryByLabelText(t.chat.composerLabel)).not.toBeInTheDocument();
  });
});

// Renders alongside Chat inside the same route element so the last
// navigation's action type (PUSH/REPLACE/POP) is observable — proving the
// redirect used `replace: true` and did not leave /chat as a back-button
// trap, without depending on react-router-dom's data-router APIs (which
// pull in a Request/AbortSignal implementation jsdom does not support).
function NavigationTypeProbe() {
  return <div data-testid="nav-type">{useNavigationType()}</div>;
}

describe('Chat redirect', () => {
  it('redirects /chat to the first conversation with replace', async () => {
    const client = newClient();
    const conversations = [
      makeConversation({ username: 'alice' }),
      makeConversation({ username: 'bob' }),
      makeConversation({ username: 'carol' }),
    ];
    client.setQueryData(queryKeys.conversations, conversations);
    seedThread(client, 'alice', [{ username: 'alice', messages: [], hasMore: false }]);
    stubFetchIndefinitely();

    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/chat']}>
          <Routes>
            <Route
              path="chat/:username?"
              element={
                <>
                  <Chat />
                  <NavigationTypeProbe />
                </>
              }
            />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    await screen.findByRole('heading', { level: 2, name: 'alice' });
    await waitFor(() => expect(screen.getByTestId('nav-type')).toHaveTextContent('REPLACE'));
  });
});

describe('Chat rail', () => {
  it('lists conversations in API order with unread digits and an active marker', async () => {
    const client = newClient();
    const conversations = [
      makeConversation({ username: 'alice', unread: 0 }),
      makeConversation({ username: 'bob', unread: 2 }),
      makeConversation({ username: 'carol', unread: 0 }),
    ];
    client.setQueryData(queryKeys.conversations, conversations);
    seedThread(client, 'bob', [{ username: 'bob', messages: [], hasMore: false }]);
    stubFetchIndefinitely();
    renderChat('/chat/bob', client);

    const bobLink = await screen.findByRole('link', { name: t.chat.threadLabel('bob', 2) });
    expect(bobLink).toHaveAttribute('aria-current', 'page');
    expect(within(bobLink).getByText('2')).toBeInTheDocument();

    const aliceLink = screen.getByRole('link', { name: t.chat.threadLabel('alice', 0) });
    expect(aliceLink).not.toHaveAttribute('aria-current');
    expect(within(aliceLink).queryByText('0')).not.toBeInTheDocument();
  });

  it('navigates and swaps messages when a different thread is clicked', async () => {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, [
      makeConversation({ username: 'alice' }),
      makeConversation({ username: 'bob' }),
    ]);
    seedThread(client, 'alice', [
      { username: 'alice', messages: [makeMessage({ id: 1, body: 'from alice' })], hasMore: false },
    ]);
    seedThread(client, 'bob', [
      { username: 'bob', messages: [makeMessage({ id: 2, body: 'from bob' })], hasMore: false },
    ]);
    stubFetchIndefinitely();
    renderChat('/chat/alice', client);

    expect(await screen.findByText('from alice')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('link', { name: t.chat.threadLabel('bob', 0) }));
    expect(await screen.findByText('from bob')).toBeInTheDocument();
    expect(screen.queryByText('from alice')).not.toBeInTheDocument();
  });
});

describe('Chat message ordering', () => {
  it('renders one newest-first page as the reverse of API order', async () => {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, [makeConversation({ username: 'alice' })]);
    seedThread(client, 'alice', [
      {
        username: 'alice',
        // API order: newest first.
        messages: [
          makeMessage({ id: 3, body: 'third' }),
          makeMessage({ id: 2, body: 'second' }),
          makeMessage({ id: 1, body: 'first' }),
        ],
        hasMore: false,
      },
    ]);
    stubFetchIndefinitely();
    renderChat('/chat/alice', client);

    await screen.findByText('first');
    const order = screen.getAllByText(/^(first|second|third)$/).map((el) => el.textContent);
    expect(order).toEqual(['first', 'second', 'third']);
  });

  it('renders an older page above the newer page after loading more', async () => {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, [makeConversation({ username: 'alice' })]);
    seedThread(client, 'alice', [
      {
        username: 'alice',
        messages: [makeMessage({ id: 6, body: 'six' }), makeMessage({ id: 5, body: 'five' }), makeMessage({ id: 4, body: 'four' })],
        hasMore: true,
      },
      {
        username: 'alice',
        messages: [makeMessage({ id: 3, body: 'three' }), makeMessage({ id: 2, body: 'two' }), makeMessage({ id: 1, body: 'one' })],
        hasMore: false,
      },
    ]);
    stubFetchIndefinitely();
    renderChat('/chat/alice', client);

    await screen.findByText('one');
    const order = screen.getAllByText(/^(one|two|three|four|five|six)$/).map((el) => el.textContent);
    expect(order).toEqual(['one', 'two', 'three', 'four', 'five', 'six']);
  });
});

describe('Chat message attribution', () => {
  it('labels an OUT message as <you> and an IN message as the peer', async () => {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, [makeConversation({ username: 'alice' })]);
    seedThread(client, 'alice', [
      {
        username: 'alice',
        messages: [
          makeMessage({ id: 2, direction: 'OUT', body: 'my reply' }),
          makeMessage({ id: 1, direction: 'IN', body: 'their message' }),
        ],
        hasMore: false,
      },
    ]);
    stubFetchIndefinitely();
    renderChat('/chat/alice', client);

    await screen.findByText('my reply');
    expect(screen.getByText(t.chat.you)).toBeInTheDocument();
    expect(screen.getByText(t.chat.peer('alice'))).toBeInTheDocument();
  });
});

describe('Chat load-older control', () => {
  it('renders only when hasMore is true', async () => {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, [makeConversation({ username: 'alice' })]);
    seedThread(client, 'alice', [
      { username: 'alice', messages: [makeMessage({ id: 1 })], hasMore: true },
    ]);
    stubFetchIndefinitely();
    renderChat('/chat/alice', client);
    expect(await screen.findByText(t.chat.loadOlder)).toBeInTheDocument();
  });

  it('does not render when hasMore is false', async () => {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, [makeConversation({ username: 'alice' })]);
    seedThread(client, 'alice', [
      { username: 'alice', messages: [makeMessage({ id: 1 })], hasMore: false },
    ]);
    stubFetchIndefinitely();
    renderChat('/chat/alice', client);
    await screen.findByText('hi');
    expect(screen.queryByText(t.chat.loadOlder)).not.toBeInTheDocument();
  });
});

describe('Chat composer', () => {
  function setUpSendableChat(fetchMock: ReturnType<typeof vi.fn>) {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, [makeConversation({ username: 'alice' })]);
    seedThread(client, 'alice', [{ username: 'alice', messages: [], hasMore: false }]);
    vi.stubGlobal('fetch', fetchMock);
    renderChat('/chat/alice', client);
    return client;
  }

  function draftInput() {
    return screen.getByLabelText(t.chat.composerLabel);
  }

  function sendButton() {
    return screen.getByRole('button', { name: t.chat.send });
  }

  it('disables send with an empty draft, enables it once typing, and POSTs the trimmed body', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && url === '/api/messages/alice') {
        return Promise.resolve(
          okJson(makeMessage({ id: 99, direction: 'OUT', body: 'hello' }), 201),
        );
      }
      if (url.startsWith('/api/messages/alice')) {
        return Promise.resolve(okJson({ username: 'alice', messages: [], hasMore: false }));
      }
      return Promise.resolve(okJson([makeConversation({ username: 'alice' })]));
    });
    setUpSendableChat(fetchMock);

    await screen.findByLabelText(t.chat.composerLabel);
    expect(sendButton()).toBeDisabled();

    fireEvent.change(draftInput(), { target: { value: 'hello' } });
    expect(sendButton()).not.toBeDisabled();

    fireEvent.click(sendButton());

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/messages/alice',
        expect.objectContaining({ method: 'POST' }),
      ),
    );
    const call = fetchMock.mock.calls.find(
      ([url, init]) => url === '/api/messages/alice' && (init as RequestInit)?.method === 'POST',
    );
    expect(call).toBeDefined();
    const init = call![1] as RequestInit;
    expect(JSON.parse(init.body as string)).toEqual({ body: 'hello' });
  });

  it('rejects a draft over the byte limit measured in UTF-8 bytes, not characters', async () => {
    const fetchMock = vi.fn((_url: string, _init?: RequestInit) => Promise.resolve(okJson([])));
    setUpSendableChat(fetchMock);
    await screen.findByLabelText(t.chat.composerLabel);

    // 9000 ASCII bytes.
    fireEvent.change(draftInput(), { target: { value: 'a'.repeat(9000) } });
    expect(sendButton()).toBeDisabled();
    expect(screen.getByText(t.chat.tooLong)).toBeInTheDocument();

    // 5000 characters of 'é', 2 bytes each in UTF-8 = 10000 bytes — over the
    // limit despite being well under it by character count.
    fireEvent.change(draftInput(), { target: { value: 'é'.repeat(5000) } });
    expect(sendButton()).toBeDisabled();
    expect(screen.getByText(t.chat.tooLong)).toBeInTheDocument();

    expect(fetchMock.mock.calls.some(([, init]) => (init as RequestInit)?.method === 'POST')).toBe(
      false,
    );
  });

  it('latches into a disabled state on a 503 send response and preserves the draft', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (init?.method === 'POST') return Promise.resolve(errJson(503, { error: 'not enabled' }));
      if (url.startsWith('/api/messages/alice')) {
        return Promise.resolve(okJson({ username: 'alice', messages: [], hasMore: false }));
      }
      return Promise.resolve(okJson([makeConversation({ username: 'alice' })]));
    });
    setUpSendableChat(fetchMock);
    await screen.findByLabelText(t.chat.composerLabel);

    fireEvent.change(draftInput(), { target: { value: 'hello' } });
    fireEvent.click(sendButton());

    expect(await screen.findByText(t.chat.sendDisabled)).toBeInTheDocument();
    expect(draftInput()).toBeDisabled();
    expect(sendButton()).toBeDisabled();
    expect(draftInput()).toHaveValue('hello');
  });

  it('shows a send-failed message on 502 but keeps the composer enabled and the draft intact', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (init?.method === 'POST') return Promise.resolve(errJson(502, { error: 'send failed' }));
      if (url.startsWith('/api/messages/alice')) {
        return Promise.resolve(okJson({ username: 'alice', messages: [], hasMore: false }));
      }
      return Promise.resolve(okJson([makeConversation({ username: 'alice' })]));
    });
    setUpSendableChat(fetchMock);
    await screen.findByLabelText(t.chat.composerLabel);

    fireEvent.change(draftInput(), { target: { value: 'hello' } });
    fireEvent.click(sendButton());

    expect(await screen.findByText(t.chat.sendFailed)).toBeInTheDocument();
    expect(draftInput()).not.toBeDisabled();
    expect(draftInput()).toHaveValue('hello');
  });

  it('clears the draft on a successful send', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (init?.method === 'POST') {
        return Promise.resolve(
          okJson(makeMessage({ id: 99, direction: 'OUT', body: 'hello' }), 201),
        );
      }
      if (url.startsWith('/api/messages/alice')) {
        return Promise.resolve(okJson({ username: 'alice', messages: [], hasMore: false }));
      }
      return Promise.resolve(okJson([makeConversation({ username: 'alice' })]));
    });
    setUpSendableChat(fetchMock);
    await screen.findByLabelText(t.chat.composerLabel);

    fireEvent.change(draftInput(), { target: { value: 'hello' } });
    fireEvent.click(sendButton());

    await waitFor(() => expect(draftInput()).toHaveValue(''));
  });

  it('clears a typed draft when switching threads', async () => {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, [
      makeConversation({ username: 'alice' }),
      makeConversation({ username: 'bob' }),
    ]);
    seedThread(client, 'alice', [{ username: 'alice', messages: [], hasMore: false }]);
    seedThread(client, 'bob', [{ username: 'bob', messages: [], hasMore: false }]);
    stubFetchIndefinitely();
    renderChat('/chat/alice', client);

    await screen.findByLabelText(t.chat.composerLabel);
    fireEvent.change(draftInput(), { target: { value: 'unsent' } });
    expect(draftInput()).toHaveValue('unsent');

    fireEvent.click(screen.getByRole('link', { name: t.chat.threadLabel('bob', 0) }));
    await screen.findByLabelText(t.chat.composerLabel);
    expect(draftInput()).toHaveValue('');
  });
});

describe('Chat mark-read', () => {
  it('marks the conversation read exactly once, not again on a poll that returns nothing new', async () => {
    vi.useFakeTimers();
    const readCalls: string[] = [];
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && url === '/api/messages/alice/read') {
        readCalls.push(url);
        return Promise.resolve(okJson({ marked: 1 }));
      }
      if (url.startsWith('/api/messages/alice')) {
        return Promise.resolve(
          okJson({
            username: 'alice',
            messages: [makeMessage({ id: 1, direction: 'IN', body: 'hi' })],
            hasMore: false,
          }),
        );
      }
      return Promise.resolve(okJson([makeConversation({ username: 'alice', unread: 1 })]));
    });
    vi.stubGlobal('fetch', fetchMock);

    const client = newClient();
    client.setQueryData(queryKeys.conversations, [makeConversation({ username: 'alice', unread: 1 })]);
    seedThread(client, 'alice', [
      { username: 'alice', messages: [makeMessage({ id: 1, direction: 'IN', body: 'hi' })], hasMore: false },
    ]);
    renderChat('/chat/alice', client);

    await vi.waitFor(() => expect(readCalls).toHaveLength(1));

    // Advance past THREAD_INTERVAL so a poll fires; the mock returns the
    // same single message again ("nothing new"), which must not re-trigger
    // the read POST.
    await vi.advanceTimersByTimeAsync(3500);
    expect(readCalls).toHaveLength(1);

    vi.useRealTimers();
  });
});

describe('Chat XSS safety', () => {
  it('renders a script-tag body as literal text, never as markup', async () => {
    const client = newClient();
    client.setQueryData(queryKeys.conversations, [makeConversation({ username: 'alice' })]);
    seedThread(client, 'alice', [
      {
        username: 'alice',
        messages: [makeMessage({ id: 1, body: '<script>alert(1)</script>' })],
        hasMore: false,
      },
    ]);
    stubFetchIndefinitely();
    const { container } = renderChat('/chat/alice', client);

    expect(await screen.findByText('<script>alert(1)</script>')).toBeInTheDocument();
    expect(container.querySelector('script')).toBeNull();
  });
});

describe('Chat username encoding', () => {
  it('round-trips a username containing a slash and a space', async () => {
    const username = 'foo/bar baz';
    const encoded = encodeURIComponent(username);
    const fetchMock = vi.fn((url: string) => {
      if (url.startsWith(`/api/messages/${encoded}`)) {
        return Promise.resolve(okJson({ username, messages: [], hasMore: false }));
      }
      return Promise.resolve(okJson([makeConversation({ username })]));
    });
    vi.stubGlobal('fetch', fetchMock);

    const client = newClient();
    client.setQueryData(queryKeys.conversations, [makeConversation({ username })]);
    renderChat(`/chat/${encoded}`, client);

    const link = await screen.findByRole('link', { name: t.chat.threadLabel(username, 0) });
    expect(link).toHaveAttribute('href', `/chat/${encoded}`);

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(`/api/messages/${encoded}?before=0`),
    );
  });
});

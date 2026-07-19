const STATUS_LABEL = { queued: 'Köad', active: 'Aktiv', stalled: 'Stannad', done: 'Klar', failed: 'Misslyckad' };

// STATE_LABEL covers the pipeline's 7 job states (spec 2026-07-06).
const STATE_LABEL = {
  WANTED: 'Väntar',
  SELECTING: 'Väljer kandidat',
  DOWNLOADING: 'Laddar ner',
  IMPORTING: 'Importerar',
  DONE: 'Klar',
  FAILED: 'Misslyckad',
  CANCELLED: 'Avbruten',
};

// CANDIDATE_STATE_LABEL covers core.CandidateState, shown in the job detail
// panel's candidate history (including NEW/cached candidates not yet tried).
const CANDIDATE_STATE_LABEL = {
  NEW: 'Ej provad',
  ACTIVE: 'Pågår',
  SUCCEEDED: 'Lyckades',
  FAILED: 'Misslyckades',
};

const EVENT_LABEL = {
  search: 'Sökte',
  search_fallback: 'Sökte (fallback)',
  candidate_selected: 'Kandidat vald',
  candidate_rejected: 'Kandidat avvisad',
  attempt_failed: 'Försök misslyckades',
  attempt_succeeded: 'Försök lyckades',
  transfer_stalled: 'Överföring stannade',
  import_ok: 'Import genomförd',
  import_rejected: 'Import avvisad',
  job_failed: 'Jobb misslyckades',
};

let jobs = [];
let searchTerm = '';
let statusFilter = '';

// --- View / hash routing ---

let currentView = 'overview';
let detailJobId = null;
const jobEventsCache = {};

function fmtBytes(n) {
  if (!n) return '0 MB';
  return (n / (1024 * 1024)).toFixed(1) + ' MB';
}

function pct(job) {
  if (!job.bytesTotal) return 0;
  return Math.round((job.bytesDone / job.bytesTotal) * 100);
}

async function fetchJobs() {
  try {
    const res = await fetch('/api/jobs');
    if (!res.ok) return; // keep showing last-good data on a transient error
    jobs = await res.json();
    render();
  } catch (e) {
    // network error: keep showing last-good data
  }
}

function statCards() {
  const counts = { queued: 0, active: 0, stalled: 0, done: 0, failed: 0 };
  for (const j of jobs) counts[j.status] = (counts[j.status] || 0) + 1;
  const el = document.getElementById('stat-cards');
  el.innerHTML = ['queued', 'active', 'stalled', 'done'].map(s =>
    `<div class="card"><div class="label">${STATUS_LABEL[s]}</div><div class="value">${counts[s] || 0}</div></div>`
  ).join('');
}

// --- Module health (authoritative per-module runtime state from /status) ---

let moduleDetails = {};

async function fetchStatus() {
  try {
    const res = await fetch('/status');
    if (!res.ok) return; // keep showing last-good data on a transient error
    const data = await res.json();
    moduleDetails = data.moduleDetails || {};
    moduleHealthRows();
  } catch (e) {
    // network error: keep showing last-good data
  }
}

function moduleHealthRows() {
  const el = document.getElementById('module-health-body');
  if (!el) return;
  const names = Object.keys(moduleDetails).sort();
  el.innerHTML = names.map(name => {
    const status = moduleDetails[name] || {};
    const never = !status.lastAttempt;
    const lastAttempt = never ? null : new Date(status.lastAttempt);
    const failures = status.consecutiveFailures || 0;
    const unhealthy = !status.live || failures >= 3;
    let label = never ? 'Har aldrig körts' : lastAttempt.toLocaleTimeString('sv-SE');
    if (failures > 0) label += ` (${failures} fel i rad)`;
    return `
      <tr>
        <td>${escapeHtml(name)}</td>
        <td class="${unhealthy ? 'module-stale' : ''}" title="${escapeHtml(status.lastError || '')}">${label}</td>
      </tr>
    `;
  }).join('');
}

function overviewActiveRows() {
  const active = jobs.filter(j => j.status === 'active');
  const body = document.getElementById('overview-active-body');
  body.innerHTML = active.map(j => `
    <tr>
      <td>${escapeHtml(j.title)}<br><span style="color:#7c828d;font-size:11.5px;">${escapeHtml(j.artist)}</span></td>
      <td>${escapeHtml(j.peer)}</td>
      <td><div class="bar"><div class="bar-fill" style="width:${pct(j)}%"></div></div></td>
    </tr>
  `).join('');
}

function matchesSearch(j) {
  if (!searchTerm) return true;
  const hay = ('#' + j.id + ' ' + j.title + ' ' + j.artist + ' ' + j.peer).toLowerCase();
  return hay.includes(searchTerm.toLowerCase());
}

function matchesStatusFilter(j) {
  if (!statusFilter) return true;
  return j.status === statusFilter;
}

function matchesFilters(j) {
  return matchesSearch(j) && matchesStatusFilter(j);
}

// sleepingBadge renders a "sover till HH:MM" badge when a job's not_before
// (backoff) is still in the future; not_before in the past just means the
// job is runnable again and has no display relevance.
function sleepingBadge(j) {
  if (!j.notBefore) return '';
  const t = new Date(j.notBefore);
  if (t <= new Date()) return '';
  return `Sover till ${t.toLocaleTimeString('sv-SE', { hour: '2-digit', minute: '2-digit' })}`;
}

function jobDetailLine(j) {
  const parts = [];
  if (j.failReason) parts.push(`Fel: ${escapeHtml(j.failReason)}`);
  if (j.nextAttemptAt) parts.push(`Nästa försök: ${new Date(j.nextAttemptAt).toLocaleString('sv-SE')}`);
  if (j.maxCandidates > 0 && (j.status === 'queued' || j.status === 'failed')) {
    parts.push(`Kandidat ${j.candidatesTried}/${j.maxCandidates}`);
  }
  if (j.retries > 0) parts.push(`${j.retries} sökförsök`);
  const sleeping = sleepingBadge(j);
  if (sleeping) parts.push(sleeping);
  if (!parts.length) return '';
  return `<br><span style="color:#7c828d;font-size:11.5px;">${parts.join(' · ')}</span>`;
}

// jobDetails caches per-job detail (attempt/transfer history) fetched lazily
// when the job detail page is opened, keyed by job id, so re-rendering while
// viewing doesn't refetch.
const jobDetails = {};

async function loadJobDetail(id) {
  try {
    const res = await fetch(`/api/jobs/${id}/detail`);
    if (!res.ok) return;
    jobDetails[id] = await res.json();
    if (detailJobId === id) renderJobDetail();
  } catch (e) {
    // network error: leave any previously loaded detail in place
  }
}

async function loadJobEvents(id) {
  try {
    const res = await fetch(`/api/jobs/${id}/events`);
    if (!res.ok) return;
    jobEventsCache[id] = await res.json();
    if (detailJobId === id) renderJobDetail();
  } catch (e) {
    // network error: leave any previously loaded events in place
  }
}

function attemptHistoryHtml(id) {
  const detail = jobDetails[id];
  if (!detail) return '<div style="color:#7c828d;">Laddar…</div>';
  if (!detail.attempts || !detail.attempts.length) return '<div style="color:#7c828d;">Inga försök ännu.</div>';
  return detail.attempts.map(a => `
    <div class="detail-attempt">
      <div><strong>${escapeHtml(a.username)}</strong> — ${escapeHtml(CANDIDATE_STATE_LABEL[a.state] || a.state)}${a.failReason ? ` (${escapeHtml(a.failReason)})` : ''}</div>
      <div style="color:#7c828d;font-size:11px;">${new Date(a.createdAt).toLocaleString('sv-SE')} · ${a.fileCount} filer</div>
      ${(a.transfers || []).map(t => `
        <div class="detail-transfer mono">${escapeHtml(t.filename)} — ${escapeHtml(t.state)}
          (${fmtBytes(t.bytesDone)} / ${fmtBytes(t.bytesTotal)}${t.retries ? `, ${t.retries} försök` : ''})</div>
      `).join('')}
    </div>
  `).join('');
}

function queueRows() {
  const filtered = jobs.filter(matchesFilters);
  const body = document.getElementById('queue-body');
  body.innerHTML = filtered.map(j => `
    <tr class="job-row" data-id="${j.id}">
      <td class="mono">#${j.id}</td>
      <td><span class="pill ${j.status}">${STATE_LABEL[j.state] || j.status}</span></td>
      <td>${escapeHtml(j.title)}<br><span style="color:#7c828d;font-size:11.5px;">${escapeHtml(j.artist)}</span>${jobDetailLine(j)}</td>
      <td>${escapeHtml(j.peer)}</td>
      <td>${j.bytesTotal ? `<div style="font-size:11px;color:#7c828d;margin-bottom:3px;">${fmtBytes(j.bytesDone)} / ${fmtBytes(j.bytesTotal)}</div>` : ''}<div class="bar"><div class="bar-fill" style="width:${pct(j)}%"></div></div></td>
    </tr>
  `).join('');

  body.querySelectorAll('tr.job-row').forEach(tr => {
    tr.addEventListener('click', () => {
      const id = Number(tr.getAttribute('data-id'));
      location.hash = '#/jobs/' + id;
    });
  });
}

// --- Job detail page ---

function jobEventsHtml(id) {
  const list = jobEventsCache[id];
  if (!list) return '<div style="color:#7c828d;">Laddar…</div>';
  if (!list.length) return '<div style="color:#7c828d;">Inga händelser.</div>';
  return `
    <table>
      <thead><tr><th>Tid</th><th>Händelse</th><th>Detalj</th></tr></thead>
      <tbody>
        ${list.map(e => `
          <tr>
            <td class="mono" style="white-space:nowrap;">${new Date(e.createdAt).toLocaleString('sv-SE')}</td>
            <td>${escapeHtml(EVENT_LABEL[e.event] || e.event)}</td>
            <td>${escapeHtml(e.detail)}</td>
          </tr>
        `).join('')}
      </tbody>
    </table>
  `;
}

function renderJobDetail() {
  if (detailJobId == null) return;
  const id = detailJobId;
  const j = jobs.find(x => x.id === id);
  const detail = jobDetails[id];
  const el = document.getElementById('view-job-detail');

  let header;
  if (j) {
    header = `
      <span class="pill ${j.status}">${STATE_LABEL[j.state] || j.status}</span>
      <h1 style="display:inline;margin-left:8px;">Jobb #${id}</h1>
      <div style="margin-top:10px;">${escapeHtml(j.title)}<br><span style="color:#7c828d;font-size:11.5px;">${escapeHtml(j.artist)}</span></div>
      <div style="color:#7c828d;font-size:11.5px;margin-top:4px;">Peer: ${escapeHtml(j.peer) || '—'}</div>
      <div style="color:#7c828d;font-size:11.5px;margin-top:4px;">Nedladdat: ${fmtBytes(j.bytesDone)} / ${fmtBytes(j.bytesTotal)}</div>
    `;
  } else if (detail) {
    header = `
      <h1 style="display:inline;">Jobb #${id}</h1>
      <span style="color:#7c828d;font-size:11.5px;margin-left:8px;">${escapeHtml(STATE_LABEL[detail.state] || detail.state)}</span>
      <div style="margin-top:10px;">${escapeHtml(detail.title)}<br><span style="color:#7c828d;font-size:11.5px;">${escapeHtml(detail.artist)}</span></div>
    `;
  } else {
    header = `<h1>Jobb #${id}</h1><div style="color:#7c828d;">Laddar…</div>`;
  }

  const isFailed = (j && j.state === 'FAILED') || (detail && detail.state === 'FAILED');

  el.innerHTML = `
    <a class="joblink" href="#" id="job-detail-back">← Tillbaka</a>
    <div style="margin-top:12px;">${header}</div>
    <div style="margin-top:14px;">
      <button class="action" data-cancel="${id}">Avbryt</button>
      ${isFailed ? `<button class="action" data-retry="${id}">Försök igen</button>` : ''}
    </div>
    <h1 style="margin-top:22px;">Försökshistorik</h1>
    <div>${attemptHistoryHtml(id)}</div>
    <h1 style="margin-top:22px;">Händelser</h1>
    <div>${jobEventsHtml(id)}</div>
  `;

  const back = document.getElementById('job-detail-back');
  back.addEventListener('click', (ev) => {
    ev.preventDefault();
    currentView = 'queue';
    location.hash = '';
  });

  el.querySelectorAll('button[data-cancel]').forEach(btn => {
    btn.addEventListener('click', async () => {
      await fetch(`/api/jobs/${id}/cancel`, { method: 'POST' });
      await fetchJobs();
      loadJobDetail(id);
      loadJobEvents(id);
    });
  });
  el.querySelectorAll('button[data-retry]').forEach(btn => {
    btn.addEventListener('click', async () => {
      await fetch(`/api/jobs/${id}/retry`, { method: 'POST' });
      delete jobDetails[id]; // candidate history was wiped by the retry, refetch
      await fetchJobs();
      loadJobDetail(id);
      loadJobEvents(id);
    });
  });
}

// --- Events (Händelser) ---

let events = [];
let eventFilterTerm = '';

async function fetchEvents() {
  try {
    const res = await fetch('/api/events?limit=200');
    if (!res.ok) return; // keep showing last-good data on a transient error
    events = await res.json();
    eventRows();
  } catch (e) {
    // network error: keep showing last-good data
  }
}

function matchesEventFilter(e) {
  if (!eventFilterTerm) return true;
  const hay = (e.event + ' ' + e.detail + ' ' + e.jobId).toLowerCase();
  return hay.includes(eventFilterTerm.toLowerCase());
}

function eventRows() {
  const filtered = events.filter(matchesEventFilter);
  const body = document.getElementById('events-body');
  body.innerHTML = filtered.map(e => `
    <tr>
      <td class="mono" style="white-space:nowrap;">${new Date(e.createdAt).toLocaleString('sv-SE')}</td>
      <td class="mono"><a class="joblink" href="#/jobs/${e.jobId}">#${e.jobId}</a></td>
      <td>${escapeHtml(EVENT_LABEL[e.event] || e.event)}</td>
      <td>${escapeHtml(e.detail)}</td>
    </tr>
  `).join('');
}

function setupEventFilter() {
  const input = document.getElementById('event-filter');
  input.addEventListener('input', () => {
    eventFilterTerm = input.value;
    eventRows();
  });
}

// --- Peers ---

let peers = [];
let peerSortKey = 'score';
let peerSortDesc = true;
let expandedPeer = null;

async function fetchPeers() {
  try {
    const res = await fetch('/api/peers');
    if (!res.ok) return; // keep showing last-good data on a transient error
    peers = await res.json();
    peerRows();
  } catch (e) {
    // network error: keep showing last-good data
  }
}

function peerRows() {
  const sorted = [...peers].sort((a, b) => {
    const d = (a[peerSortKey] || 0) - (b[peerSortKey] || 0);
    return peerSortDesc ? -d : d;
  });
  const body = document.getElementById('peers-body');
  body.innerHTML = sorted.map(p => {
    const rows = [`
      <tr class="job-row" data-peer="${escapeHtml(p.username)}">
        <td>${escapeHtml(p.username)}</td>
        <td class="mono">${p.score.toFixed(2)}</td>
        <td class="mono">${p.successCount}</td>
        <td class="mono">${p.failCount}</td>
      </tr>
    `];
    if (expandedPeer === p.username) {
      const artists = p.artists || [];
      rows.push(`
        <tr class="detail-row">
          <td colspan="4">
            ${artists.length ? artists.map(a => `
              <div class="detail-transfer mono">Artist #${a.artistId} — poäng ${a.score.toFixed(2)}, ${a.successCount} lyckade, ${a.failCount} misslyckade</div>
            `).join('') : '<div style="color:#7c828d;">Ingen artistspecifik historik.</div>'}
          </td>
        </tr>
      `);
    }
    return rows.join('');
  }).join('');

  body.querySelectorAll('tr.job-row').forEach(tr => {
    tr.addEventListener('click', () => {
      const username = tr.getAttribute('data-peer');
      expandedPeer = expandedPeer === username ? null : username;
      peerRows();
    });
  });
}

function setupPeerSort() {
  document.querySelectorAll('th.sortable').forEach(th => {
    th.addEventListener('click', () => {
      const key = th.getAttribute('data-sort');
      if (peerSortKey === key) {
        peerSortDesc = !peerSortDesc;
      } else {
        peerSortKey = key;
        peerSortDesc = true;
      }
      peerRows();
    });
  });
}

function escapeHtml(s) {
  const div = document.createElement('div');
  div.textContent = s === undefined || s === null ? '' : String(s);
  return div.innerHTML;
}

function render() {
  statCards();
  overviewActiveRows();
  queueRows();
  if (detailJobId != null) renderJobDetail();
}

function showTab(name) {
  currentView = name;
  document.querySelectorAll('nav button[data-view]').forEach(b => {
    b.classList.toggle('active', b.getAttribute('data-view') === name);
  });
  document.querySelectorAll('.view').forEach(v => {
    v.classList.toggle('active', v.id === 'view-' + name);
  });
}

function showJobDetail(id) {
  detailJobId = id;
  document.querySelectorAll('nav button[data-view]').forEach(b => b.classList.remove('active'));
  document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
  document.getElementById('view-job-detail').classList.add('active');
  if (!jobDetails[id]) loadJobDetail(id);
  loadJobEvents(id);
  renderJobDetail();
}

function router() {
  const m = location.hash.match(/^#\/jobs\/(\d+)$/);
  if (m) {
    showJobDetail(Number(m[1]));
  } else {
    detailJobId = null;
    showTab(currentView);
  }
}

function setupNav() {
  document.querySelectorAll('nav button[data-view]').forEach(btn => {
    btn.addEventListener('click', () => {
      currentView = btn.getAttribute('data-view');
      if (location.hash) {
        location.hash = ''; // triggers hashchange -> router -> showTab
      } else {
        showTab(currentView);
      }
    });
  });
}

function setupSearch() {
  const input = document.getElementById('search');
  input.addEventListener('input', () => {
    searchTerm = input.value;
    queueRows();
  });
}

function setupStatusFilter() {
  const select = document.getElementById('status-filter');
  select.addEventListener('change', () => {
    statusFilter = select.value;
    queueRows();
  });
}

setupNav();
setupSearch();
setupStatusFilter();
setupEventFilter();
setupPeerSort();
fetchJobs();
fetchEvents();
fetchPeers();
fetchStatus();
window.addEventListener('hashchange', router);
router();
setInterval(fetchJobs, 3000);
setInterval(fetchEvents, 3000);
setInterval(fetchPeers, 5000);
setInterval(fetchStatus, 5000);
setInterval(() => {
  if (detailJobId != null) {
    loadJobDetail(detailJobId);
    loadJobEvents(detailJobId);
  }
}, 3000);

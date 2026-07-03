const STATUS_LABEL = { queued: 'Köad', active: 'Aktiv', stalled: 'Stannad', done: 'Klar', failed: 'Misslyckad' };

const STATE_LABEL = {
  DISCOVERED: 'Upptäckt',
  SEARCHING: 'Söker',
  SELECTING: 'Väljer kandidat',
  DOWNLOADING: 'Laddar ner',
  VERIFYING: 'Verifierar',
  IMPORTING: 'Importerar',
  COMPLETED: 'Klar',
  COOLDOWN: 'Väntar',
  FAILED: 'Misslyckad',
  CANCELLED: 'Avbruten',
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
  const hay = (j.title + ' ' + j.artist + ' ' + j.peer).toLowerCase();
  return hay.includes(searchTerm.toLowerCase());
}

function matchesStatusFilter(j) {
  if (!statusFilter) return true;
  return j.status === statusFilter;
}

function matchesFilters(j) {
  return matchesSearch(j) && matchesStatusFilter(j);
}

function jobDetailLine(j) {
  const parts = [];
  if (j.failReason) parts.push(`Fel: ${escapeHtml(j.failReason)}`);
  if (j.nextAttemptAt) parts.push(`Nästa försök: ${new Date(j.nextAttemptAt).toLocaleString('sv-SE')}`);
  if (j.maxCandidates > 0 && (j.status === 'queued' || j.status === 'failed')) {
    parts.push(`Kandidat ${j.candidatesTried}/${j.maxCandidates}`);
  }
  if (!parts.length) return '';
  return `<br><span style="color:#7c828d;font-size:11.5px;">${parts.join(' · ')}</span>`;
}

let expandedId = null;
// jobDetails caches per-job detail (attempt/transfer history) fetched lazily
// on expand, keyed by job id, so re-rendering while expanded doesn't refetch.
const jobDetails = {};

async function loadJobDetail(id) {
  try {
    const res = await fetch(`/api/jobs/${id}/detail`);
    if (!res.ok) return;
    jobDetails[id] = await res.json();
    if (expandedId === id) queueRows();
  } catch (e) {
    // network error: leave any previously loaded detail in place
  }
}

function attemptHistoryHtml(id) {
  const detail = jobDetails[id];
  if (!detail) return '<div style="color:#7c828d;">Laddar…</div>';
  if (!detail.attempts || !detail.attempts.length) return '<div style="color:#7c828d;">Inga försök ännu.</div>';
  return detail.attempts.map(a => `
    <div class="detail-attempt">
      <div><strong>${escapeHtml(a.username)}</strong> — ${escapeHtml(a.state)}${a.failReason ? ` (${escapeHtml(a.failReason)})` : ''}</div>
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
  body.innerHTML = filtered.map(j => {
    const rows = [`
      <tr class="job-row" data-id="${j.id}">
        <td><span class="pill ${j.status}">${STATE_LABEL[j.state] || j.status}</span></td>
        <td>${escapeHtml(j.title)}<br><span style="color:#7c828d;font-size:11.5px;">${escapeHtml(j.artist)}</span>${jobDetailLine(j)}</td>
        <td>${escapeHtml(j.peer)}</td>
        <td>${j.bytesTotal ? `<div style="font-size:11px;color:#7c828d;margin-bottom:3px;">${fmtBytes(j.bytesDone)} / ${fmtBytes(j.bytesTotal)}</div>` : ''}<div class="bar"><div class="bar-fill" style="width:${pct(j)}%"></div></div></td>
        <td></td>
      </tr>
    `];
    if (expandedId === j.id) {
      rows.push(`
        <tr class="detail-row">
          <td colspan="5">
            <div>Peer: ${escapeHtml(j.peer) || '—'}</div>
            <div>Nedladdat: ${fmtBytes(j.bytesDone)} / ${fmtBytes(j.bytesTotal)}</div>
            <button class="action" data-cancel="${j.id}">Avbryt</button>
            <div style="margin-top:12px;">${attemptHistoryHtml(j.id)}</div>
          </td>
        </tr>
      `);
    }
    return rows.join('');
  }).join('');

  body.querySelectorAll('tr.job-row').forEach(tr => {
    tr.addEventListener('click', () => {
      const id = Number(tr.getAttribute('data-id'));
      if (expandedId === id) {
        expandedId = null;
      } else {
        expandedId = id;
        if (!jobDetails[id]) loadJobDetail(id); // fetched lazily, once per expand
      }
      queueRows();
    });
  });
  body.querySelectorAll('button[data-cancel]').forEach(btn => {
    btn.addEventListener('click', async (ev) => {
      ev.stopPropagation();
      const id = btn.getAttribute('data-cancel');
      await fetch(`/api/jobs/${id}/cancel`, { method: 'POST' });
      await fetchJobs();
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
      <td class="mono">#${e.jobId}</td>
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
}

function setupNav() {
  document.querySelectorAll('nav button[data-view]').forEach(btn => {
    btn.addEventListener('click', () => {
      document.querySelectorAll('nav button[data-view]').forEach(b => b.classList.remove('active'));
      document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
      btn.classList.add('active');
      document.getElementById('view-' + btn.getAttribute('data-view')).classList.add('active');
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
setInterval(fetchJobs, 3000);
setInterval(fetchEvents, 3000);
setInterval(fetchPeers, 5000);

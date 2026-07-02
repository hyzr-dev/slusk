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
          </td>
        </tr>
      `);
    }
    return rows.join('');
  }).join('');

  body.querySelectorAll('tr.job-row').forEach(tr => {
    tr.addEventListener('click', () => {
      const id = Number(tr.getAttribute('data-id'));
      expandedId = expandedId === id ? null : id;
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

function escapeHtml(s) {
  const div = document.createElement('div');
  div.textContent = s || '';
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
fetchJobs();
setInterval(fetchJobs, 3000);

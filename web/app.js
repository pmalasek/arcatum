// Arcatum web UI. Vanilla JS on purpose: the whole point of embedding the UI is that
// the server stays one self-contained binary, with no build step to keep in sync.
'use strict';

const API = '/api/v1';
const LIST_REFRESH_MS = 5000; // how often the tables refresh
const TAIL_POLL_MS = 1000;    // how often a running job's output is polled

const el = (id) => document.getElementById(id);
const conn = el('conn');

let listTimer = null;
let tailTimer = null;
let currentView = 'runs';
let currentRun = null;
let tailOffset = 0;

// --- helpers ----------------------------------------------------------------

async function api(path, opts) {
  const res = await fetch(API + path, opts);
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  conn.textContent = '';
  return res.status === 204 ? null : res.json();
}

// A failed request is usually a missing/expired admin certificate, so say that
// instead of leaving stale tables on screen.
function showError(err) {
  conn.textContent = 'nedostupné: ' + err.message;
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

const ZERO_TIME = '0001-01-01T00:00:00Z';

function fmtTime(iso) {
  if (!iso || iso.startsWith('0001-01-01')) return '—';
  const d = new Date(iso);
  return d.toLocaleString('cs-CZ', { dateStyle: 'short', timeStyle: 'medium' });
}

function fmtDuration(startIso, endIso) {
  if (!startIso || startIso === ZERO_TIME) return '—';
  const start = new Date(startIso);
  const end = (endIso && endIso !== ZERO_TIME) ? new Date(endIso) : new Date();
  const ms = end - start;
  if (ms < 0) return '—';
  if (ms < 1000) return ms + ' ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + ' s';
  const m = Math.floor(s / 60);
  return `${m} min ${Math.round(s % 60)} s`;
}

function fmtBytes(n) {
  if (!n) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + units[i];
}

function badge(status) {
  return `<span class="badge ${esc(status)}">${esc(status)}</span>`;
}

// --- views ------------------------------------------------------------------

async function loadRuns() {
  const runs = await api('/runs?limit=100');
  const body = el('runs-body');
  if (!runs || runs.length === 0) {
    body.innerHTML = '<tr><td colspan="8" class="empty">žádné běhy</td></tr>';
    return;
  }
  body.innerHTML = runs.map((r) => `
    <tr class="clickable" data-run="${esc(r.id)}">
      <td class="mono">${esc(r.id)}</td>
      <td>${esc(r.instance_id)}</td>
      <td>${badge(r.status)}</td>
      <td class="num">${r.exit_code}</td>
      <td class="num">${fmtBytes(r.bytes)}</td>
      <td>${fmtTime(r.started_at)}</td>
      <td class="num">${fmtDuration(r.started_at, r.ended_at)}</td>
      <td><button class="action" data-open="${esc(r.id)}">výstup</button></td>
    </tr>`).join('');
}

async function loadInstances() {
  const list = await api('/instances');
  const body = el('instances-body');
  if (!list || list.length === 0) {
    body.innerHTML = '<tr><td colspan="6" class="empty">žádné instance</td></tr>';
    return;
  }
  body.innerHTML = list.map((i) => `
    <tr>
      <td class="mono">${esc(i.id)}</td>
      <td>${esc(i.script)}</td>
      <td>${esc(i.runner_id)}</td>
      <td>${fmtTime(i.next_run)}</td>
      <td class="mono" data-repo="${esc(i.id)}">…</td>
      <td><button class="action" data-trigger="${esc(i.id)}">spustit teď</button></td>
    </tr>`).join('');

  // Repository sizes are a separate call per instance; fill them in as they arrive.
  for (const cell of body.querySelectorAll('[data-repo]')) {
    const id = cell.dataset.repo;
    api(`/instances/${encodeURIComponent(id)}/repo`)
      .then((info) => {
        cell.textContent = info.exists
          ? `${fmtBytes(info.bytes)} · ${info.snapshots} snap.`
          : '—';
      })
      .catch(() => { cell.textContent = '—'; });
  }
}

// shortFp abbreviates a fingerprint; the full value is in the title attribute so it can
// still be compared against the host.
function shortFp(fp) {
  if (!fp) return '';
  return `<span title="${esc(fp)}">${esc(fp.slice(0, 16))}…</span>`;
}

async function loadRunners() {
  const list = await api('/runners');
  const body = el('runners-body');
  if (!list || list.length === 0) {
    body.innerHTML = '<tr><td colspan="6" class="empty">žádné runnery</td></tr>';
    return;
  }
  body.innerHTML = list.map((r) => {
    // A pending runner is waiting for approval — that is the moment to check the
    // fingerprint and source address against the host being installed.
    const pending = r.status === 'pending';
    const actions = pending
      ? `<button class="action" data-approve="${esc(r.id)}">schválit</button>
         <button class="action" data-reject="${esc(r.id)}">zamítnout</button>`
      : '';
    const ident = pending
      ? (r.enroll_ip ? `z ${esc(r.enroll_ip)}` : '')
      : shortFp(r.cert_fingerprint);
    return `
    <tr>
      <td class="mono">${esc(r.id)}</td>
      <td>${badge(r.status)}</td>
      <td>${esc(r.os)}/${esc(r.arch)}</td>
      <td class="mono">${ident}</td>
      <td>${fmtTime(r.last_seen)}</td>
      <td>${actions}</td>
    </tr>`;
  }).join('');
}

const loaders = { runs: loadRuns, instances: loadInstances, runners: loadRunners };

async function refresh() {
  if (currentView === 'detail') return;
  try {
    await loaders[currentView]();
  } catch (err) {
    showError(err);
  }
}

function showView(name) {
  currentView = name;
  for (const v of ['runs', 'instances', 'runners', 'detail']) {
    el('view-' + v).classList.toggle('hidden', v !== name);
  }
  for (const tab of document.querySelectorAll('.tab')) {
    tab.classList.toggle('active', tab.dataset.view === name);
  }
  if (name !== 'detail') {
    stopTail();
    refresh();
  }
}

// --- run detail with live tail ---------------------------------------------

function stopTail() {
  if (tailTimer) { clearTimeout(tailTimer); tailTimer = null; }
  currentRun = null;
}

async function openRun(runID) {
  stopTail();
  currentRun = runID;
  tailOffset = 0;
  el('detail-title').textContent = runID;
  el('log').textContent = '';
  el('detail-meta').innerHTML = '';
  showView('detail');

  try {
    const run = await api('/runs/' + encodeURIComponent(runID));
    renderRunMeta(run);
  } catch (err) {
    showError(err);
    return;
  }
  pollTail();
}

function renderRunMeta(run) {
  el('detail-status').className = 'badge ' + run.status;
  el('detail-status').textContent = run.status;
  el('detail-meta').innerHTML = `
    <dt>instance</dt><dd>${esc(run.instance_id)}</dd>
    <dt>skript</dt><dd>${esc(run.script)}</dd>
    <dt>runner</dt><dd>${esc(run.runner_id)}</dd>
    <dt>návratový kód</dt><dd>${run.exit_code}</dd>
    <dt>přeneseno</dt><dd>${fmtBytes(run.bytes)}</dd>
    <dt>začátek</dt><dd>${fmtTime(run.started_at)}</dd>
    <dt>trvání</dt><dd>${fmtDuration(run.started_at, run.ended_at)}</dd>
    ${run.err ? `<dt>chyba</dt><dd>${esc(run.err)}</dd>` : ''}`;
}

// pollTail fetches output from where the last poll stopped, so nothing is re-sent and
// nothing is missed. It keeps polling until the server reports the run is finished.
async function pollTail() {
  if (!currentRun) return;
  const runID = currentRun;
  const stream = el('stream').value;
  try {
    const res = await api(`/runs/${encodeURIComponent(runID)}/tail`
      + `?offset=${tailOffset}&stream=${encodeURIComponent(stream)}`);
    if (currentRun !== runID) return; // user navigated away mid-request

    if (res.data) {
      const log = el('log');
      log.appendChild(document.createTextNode(res.data));
      tailOffset = res.offset;
      if (el('follow').checked) log.scrollTop = log.scrollHeight;
    } else {
      tailOffset = res.offset;
    }
    el('detail-status').className = 'badge ' + res.status;
    el('detail-status').textContent = res.status;

    if (res.done) {
      // Refresh the metadata once more so the final duration and byte count show.
      try { renderRunMeta(await api('/runs/' + encodeURIComponent(runID))); } catch (_) {}
      return;
    }
  } catch (err) {
    showError(err);
  }
  tailTimer = setTimeout(pollTail, TAIL_POLL_MS);
}

// --- events -----------------------------------------------------------------

document.querySelector('nav').addEventListener('click', (e) => {
  const tab = e.target.closest('.tab');
  if (tab) showView(tab.dataset.view);
});

el('back').addEventListener('click', () => showView('runs'));

el('stream').addEventListener('change', () => {
  // Switching stream restarts the tail from the beginning of that stream.
  if (!currentRun) return;
  const runID = currentRun;
  stopTail();
  currentRun = runID;
  tailOffset = 0;
  el('log').textContent = '';
  pollTail();
});

document.addEventListener('click', async (e) => {
  const open = e.target.closest('[data-open]');
  if (open) {
    e.stopPropagation();
    openRun(open.dataset.open);
    return;
  }
  const trigger = e.target.closest('[data-trigger]');
  if (trigger) {
    const id = trigger.dataset.trigger;
    trigger.disabled = true;
    trigger.textContent = 'zařazeno…';
    try {
      await api(`/instances/${encodeURIComponent(id)}/run`, { method: 'POST' });
      // The job starts at the runner's next check-in; the Runs tab will show it.
      setTimeout(() => showView('runs'), 400);
    } catch (err) {
      showError(err);
      trigger.disabled = false;
      trigger.textContent = 'spustit teď';
    }
    return;
  }
  // Approving a runner signs its certificate request; rejecting discards it.
  const approve = e.target.closest('[data-approve]');
  const reject = e.target.closest('[data-reject]');
  if (approve || reject) {
    const btn = approve || reject;
    const id = btn.dataset.approve || btn.dataset.reject;
    const action = approve ? 'approve' : 'reject';
    if (reject && !confirm(`Zamítnout runner "${id}"? Žádost bude zahozena.`)) return;
    btn.disabled = true;
    try {
      await api(`/runners/${encodeURIComponent(id)}/${action}`, { method: 'POST' });
      await loadRunners();
    } catch (err) {
      showError(err);
      btn.disabled = false;
    }
    return;
  }
  const row = e.target.closest('tr.clickable');
  if (row) openRun(row.dataset.run);
});

// --- start ------------------------------------------------------------------

refresh();
listTimer = setInterval(refresh, LIST_REFRESH_MS);

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

// Certificates expire silently: a runner simply stops checking in, and the browser
// simply stops connecting. These thresholds decide when to start saying so.
const EXPIRY_WARN_DAYS = 30;
const EXPIRY_ALERT_DAYS = 7;

function daysUntil(iso) {
  if (!iso || iso.startsWith('0001-01-01')) return null;
  return Math.floor((new Date(iso) - new Date()) / 86400000);
}

// fmtExpiry renders an expiry date, coloured once it is close.
function fmtExpiry(iso) {
  const days = daysUntil(iso);
  if (days === null) return '<span class="muted">—</span>';
  const date = new Date(iso).toLocaleDateString('cs-CZ');
  if (days < 0) return `<span class="expiry alert">vypršel ${esc(date)}</span>`;
  const cls = days <= EXPIRY_ALERT_DAYS ? 'alert' : (days <= EXPIRY_WARN_DAYS ? 'warn' : '');
  return `<span class="expiry ${cls}">${esc(date)}<span class="days"> (${days} d)</span></span>`;
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
    // Revoking is the compromise response: it drops the runner back to pending, so its
    // certificate stops working everywhere and it has to be re-established.
    const actions = pending
      ? `<button class="action" data-approve="${esc(r.id)}">schválit</button>
         <button class="action" data-reject="${esc(r.id)}">zamítnout</button>`
      : `<button class="action" data-revoke="${esc(r.id)}">zneplatnit</button>`;
    const ident = pending
      ? (r.enroll_ip ? `z ${esc(r.enroll_ip)}` : '')
      : shortFp(r.cert_fingerprint);
    return `
    <tr>
      <td class="mono">${esc(r.id)}</td>
      <td>${badge(r.status)}</td>
      <td>${esc(r.os)}/${esc(r.arch)}</td>
      <td class="mono">${ident}</td>
      <td>${fmtExpiry(r.cert_not_after)}</td>
      <td>${fmtTime(r.last_seen)}</td>
      <td>${actions}</td>
    </tr>`;
  }).join('');

  // Runner certificates are all issued around the same time, so they also expire around
  // the same time — worth one collected warning rather than a surprise.
  const expiring = list.filter((r) => {
    const d = daysUntil(r.cert_not_after);
    return d !== null && d <= EXPIRY_WARN_DAYS;
  });
  setWarning('runners', expiring.length === 0 ? null : {
    level: expiring.some((r) => daysUntil(r.cert_not_after) <= EXPIRY_ALERT_DAYS) ? 'alert' : 'warn',
    text: `Certifikát vyprší u ${expiring.length} runner${expiring.length === 1 ? 'u' : 'ů'}: `
      + expiring.map((r) => `${r.id} (${daysUntil(r.cert_not_after)} d)`).join(', ')
      + '. Vydej nový: arcatum-ca runner -id <id>, nebo runner zamítni a nech ho znovu požádat.',
  });
}

// --- expiry warnings --------------------------------------------------------

// Warnings are keyed so each source can set or clear its own without clobbering others.
const warnings = new Map();

function setWarning(key, warning) {
  if (warning) warnings.set(key, warning);
  else warnings.delete(key);
  const box = el('warnings');
  box.classList.toggle('hidden', warnings.size === 0);
  box.innerHTML = [...warnings.values()]
    .map((w) => `<div class="warning ${esc(w.level)}">${esc(w.text)}</div>`)
    .join('');
}

// checkIdentity warns about the operator's own certificate and the server's. The admin
// certificate is the one that bites first: a year by default, and when it lapses the
// browser just stops connecting with no explanation.
async function checkIdentity() {
  let me;
  try {
    me = await api('/whoami');
  } catch (err) {
    return; // the tables will surface the connection problem
  }
  if (!me.secured) {
    setWarning('dev', {
      level: 'warn',
      text: 'Server běží bez mTLS — spojení není šifrované ani ověřené. Určeno jen pro vývoj.',
    });
    return;
  }
  setWarning('me', me.days_left > EXPIRY_WARN_DAYS ? null : {
    level: me.days_left <= EXPIRY_ALERT_DAYS ? 'alert' : 'warn',
    text: me.days_left < 0
      ? `Tvůj certifikát (${me.name}) vypršel.`
      : `Tvůj certifikát (${me.name}) vyprší za ${me.days_left} d. Vydej nový: `
        + `arcatum-ca admin -name ${me.name}`,
  });
  setWarning('server', me.server_days_left > EXPIRY_WARN_DAYS ? null : {
    level: me.server_days_left <= EXPIRY_ALERT_DAYS ? 'alert' : 'warn',
    text: me.server_days_left < 0
      ? 'Certifikát serveru vypršel — runnery mu přestanou věřit.'
      : `Certifikát serveru vyprší za ${me.server_days_left} d. Vydej nový: `
        + 'arcatum-ca server -hosts <adresy>',
  });
}

// --- restore ----------------------------------------------------------------

// Browsing happens against the repository on the server, so a restore does not depend on
// the backed-up host being reachable — which is often exactly why you need one.
const restore = { instance: '', snapshot: '', path: '/' };

async function loadRestore() {
  const sel = el('restore-instance');
  if (sel.options.length <= 1) {
    const list = await api('/instances');
    sel.innerHTML = '<option value="">— vyber —</option>'
      + list.map((i) => `<option value="${esc(i.id)}">${esc(i.id)}</option>`).join('');
  }
  if (!restore.instance) {
    el('restore-body').innerHTML = '<tr><td colspan="5" class="empty">vyber instanci</td></tr>';
  }
}

async function loadSnapshots() {
  const sel = el('restore-snapshot');
  el('restore-info').textContent = '';
  if (!restore.instance) {
    sel.innerHTML = '<option value="">…</option>';
    return;
  }
  sel.innerHTML = '<option value="">načítám…</option>';
  let snaps;
  try {
    snaps = await api(`/instances/${encodeURIComponent(restore.instance)}/snapshots`);
  } catch (err) {
    sel.innerHTML = '<option value="">—</option>';
    el('restore-body').innerHTML =
      `<tr><td colspan="5" class="empty">${esc(err.message)}</td></tr>`;
    return;
  }
  if (!snaps || snaps.length === 0) {
    sel.innerHTML = '<option value="">žádné snapshoty</option>';
    el('restore-body').innerHTML =
      '<tr><td colspan="5" class="empty">tato instance ještě nic nezálohovala</td></tr>';
    return;
  }
  sel.innerHTML = snaps.map((s) =>
    `<option value="${esc(s.id)}">${fmtTime(s.time)} · ${esc(s.short_id)}</option>`).join('');
  restore.snapshot = snaps[0].id;
  // Start at the directory the snapshot was taken from rather than at "/".
  restore.path = (snaps[0].paths && snaps[0].paths[0]) || '/';
  el('restore-info').textContent = `${snaps.length} snapshot${snaps.length === 1 ? '' : 'ů'}`;
  await loadRestoreDir();
}

function renderCrumbs() {
  const parts = restore.path.split('/').filter(Boolean);
  let acc = '';
  const crumbs = [`<a href="#" data-dir="/">/</a>`];
  for (const p of parts) {
    acc += '/' + p;
    crumbs.push(`<a href="#" data-dir="${esc(acc)}">${esc(p)}</a>`);
  }
  el('restore-crumbs').innerHTML = crumbs.join('<span class="sep">/</span>');
}

async function loadRestoreDir() {
  if (!restore.instance || !restore.snapshot) return;
  renderCrumbs();
  const body = el('restore-body');
  body.innerHTML = '<tr><td colspan="5" class="empty">načítám…</td></tr>';

  const base = `/instances/${encodeURIComponent(restore.instance)}`
    + `/snapshots/${encodeURIComponent(restore.snapshot)}`;
  let res;
  try {
    res = await api(`${base}/ls?path=${encodeURIComponent(restore.path)}`);
  } catch (err) {
    body.innerHTML = `<tr><td colspan="5" class="empty">${esc(err.message)}</td></tr>`;
    return;
  }
  const dl = (p, archive) => API + base + '/download?path=' + encodeURIComponent(p)
    + (archive ? '&archive=tar' : '');

  const rows = [];
  // Offer the current directory as a tar, which is how you get a whole tree back.
  if (restore.path && restore.path !== '/') {
    rows.push(`<tr>
      <td class="mono">.</td><td>adresář</td><td></td><td></td>
      <td><a class="action" href="${esc(dl(restore.path, true))}">stáhnout jako .tar</a></td>
    </tr>`);
  }
  for (const e of res.entries) {
    const isDir = e.type === 'dir';
    rows.push(`<tr${isDir ? ' class="clickable" data-dir="' + esc(e.path) + '"' : ''}>
      <td class="mono">${isDir ? '📁 ' : ''}${esc(e.name)}</td>
      <td>${isDir ? 'adresář' : esc(e.type)}</td>
      <td class="num">${isDir ? '' : fmtBytes(e.size)}</td>
      <td>${isDir ? '' : fmtTime(e.mtime)}</td>
      <td><a class="action" href="${esc(dl(e.path, isDir))}">${isDir ? 'stáhnout .tar' : 'stáhnout'}</a></td>
    </tr>`);
  }
  if (rows.length === 0) {
    body.innerHTML = '<tr><td colspan="5" class="empty">prázdný adresář</td></tr>';
    return;
  }
  if (res.truncated) {
    rows.push('<tr><td colspan="5" class="empty">výpis je zkrácený — příliš mnoho položek</td></tr>');
  }
  body.innerHTML = rows.join('');
}

el('restore-instance').addEventListener('change', async (e) => {
  restore.instance = e.target.value;
  restore.snapshot = '';
  restore.path = '/';
  await loadSnapshots();
});

el('restore-snapshot').addEventListener('change', async (e) => {
  restore.snapshot = e.target.value;
  await loadRestoreDir();
});

el('restore-crumbs').addEventListener('click', async (e) => {
  const a = e.target.closest('[data-dir]');
  if (!a) return;
  e.preventDefault();
  restore.path = a.dataset.dir;
  await loadRestoreDir();
});

const loaders = {
  runs: loadRuns,
  instances: loadInstances,
  restore: loadRestore,
  runners: loadRunners,
};

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
  for (const v of ['runs', 'instances', 'restore', 'runners', 'detail']) {
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
  // Approving a runner signs its certificate request; rejecting discards it; revoking
  // invalidates an issued certificate and sends the runner back to pending.
  const approve = e.target.closest('[data-approve]');
  const reject = e.target.closest('[data-reject]');
  const revoke = e.target.closest('[data-revoke]');
  if (approve || reject || revoke) {
    const btn = approve || reject || revoke;
    const id = btn.dataset.approve || btn.dataset.reject || btn.dataset.revoke;
    const action = approve ? 'approve' : (reject ? 'reject' : 'revoke');
    if (reject && !confirm(`Zamítnout runner "${id}"? Žádost bude zahozena.`)) return;
    if (revoke && !confirm(
      `Zneplatnit certifikát runneru "${id}"?\n\n`
      + 'Jeho certifikát okamžitě přestane platit všude včetně přístupu k zálohám. '
      + 'Runner přejde do stavu "pending" a sám požádá o nový — pak ho musíš znovu schválit.')) return;
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
  if (e.target.closest('#revoke-all')) {
    if (!confirm(
      'Zneplatnit certifikáty VŠECH runnerů?\n\n'
      + 'Použij při podezření na kompromitaci CA. Všechny runnery přejdou do stavu '
      + '"pending" a zálohování se zastaví, dokud je znovu neschválíš.')) return;
    try {
      const res = await api('/runners/revoke-all', { method: 'POST' });
      alert(`Zneplatněno certifikátů: ${res.revoked}`);
      await loadRunners();
    } catch (err) {
      showError(err);
    }
    return;
  }
  // A download link must not also trigger the row's navigation.
  if (e.target.closest('a.action')) return;

  const row = e.target.closest('tr.clickable');
  if (!row) return;
  if (row.dataset.dir) {
    // Restore view: descend into the directory.
    restore.path = row.dataset.dir;
    await loadRestoreDir();
    return;
  }
  if (row.dataset.run) openRun(row.dataset.run);
});

// --- start ------------------------------------------------------------------

checkIdentity();
refresh();
listTimer = setInterval(refresh, LIST_REFRESH_MS);

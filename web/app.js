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
let me = null; // identity from /whoami: who I am and what I may do

// --- helpers ----------------------------------------------------------------

// ApiError carries the HTTP code so callers can tell 401 (not logged in) from other errors.
class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

// errorText pulls the text worth showing out of a response: `parameter "password" is
// required` is more useful than `400 Bad Request`. Part of the API answers with
// {"error": "..."}, part with plain text from http.Error — so the body is read first and
// JSON only attempted from it, and neither reason gets lost.
async function errorText(res) {
  const raw = (await res.text().catch(() => '')).trim();
  // An empty body says nothing, and an error page from a reverse proxy is noise here
  // rather than an explanation; in both cases the status code is what is left.
  if (!raw || raw.length > 500) return `${res.status} ${res.statusText}`;
  try {
    const body = JSON.parse(raw);
    if (body && body.error) return body.error;
  } catch (_) {
    return raw;
  }
  return `${res.status} ${res.statusText}`;
}

async function api(path, opts) {
  const res = await fetch(API + path, opts);
  if (res.status === 401) {
    // The session expired (or never started) — the login screen is more useful than
    // tables with stale data.
    showLogin('Your session expired, please sign in again.');
    throw new ApiError(401, 'not signed in');
  }
  if (!res.ok) throw new ApiError(res.status, await errorText(res));
  conn.textContent = '';
  return res.status === 204 ? null : res.json();
}

// api with a JSON body, for POST/PUT.
function apiSend(path, method, payload) {
  return api(path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

// A failed request usually means the server is unreachable or refused the action, so
// say that instead of leaving stale tables on screen.
function showError(err) {
  conn.textContent = 'unavailable: ' + err.message;
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

const ZERO_TIME = '0001-01-01T00:00:00Z';

// hasTime tells a real timestamp from Go's unset one. `omitempty` does not drop a zero
// time.Time, so the field always arrives — and "0001-01-01T00:00:00Z" is a truthy string,
// which makes a plain `if (iso)` read an unset time as a moment two millennia ago.
function hasTime(iso) {
  return !!iso && !iso.startsWith('0001-01-01');
}

function fmtTime(iso) {
  if (!hasTime(iso)) return '—';
  const d = new Date(iso);
  return d.toLocaleString('en-GB', { dateStyle: 'short', timeStyle: 'medium' });
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

// How far along a backup is on its way to the off-site server. An empty value is not a
// fault but "we do not know": replication may be off, or the run may be older than it —
// hence a dash, not red. Words instead of the API's states, so the column can be read.
const REPLICA_LABEL = {
  done: ['success', 'sent', 'the backup is on the off-site server'],
  syncing: ['running', 'sending', 'the transfer is in progress'],
  pending: ['pending', 'queued', 'waiting to be sent to the off-site server'],
  failed: ['failed', 'error', 'the transfer failed, it is retried automatically'],
};

function replicaBadge(status) {
  const cell = REPLICA_LABEL[status];
  if (!cell) return '<span class="muted" title="no replication">—</span>';
  return `<span class="badge ${cell[0]}" title="${esc(cell[2])}">${esc(cell[1])}</span>`;
}

// --- views ------------------------------------------------------------------

async function loadRuns() {
  const runs = await api('/runs?limit=100');
  const body = el('runs-body');
  if (!runs || runs.length === 0) {
    body.innerHTML = '<tr><td colspan="9" class="empty">no runs</td></tr>';
    return;
  }
  body.innerHTML = runs.map((r) => `
    <tr class="clickable" data-run="${esc(r.id)}">
      <td class="mono">${esc(r.id)}</td>
      <td>${esc(r.instance_id)}</td>
      <td>${badge(r.status)}</td>
      <td>${replicaBadge(r.replica_status)}</td>
      <td class="num">${r.exit_code}</td>
      <td class="num">${fmtBytes(r.data_bytes || r.bytes)}</td>
      <td>${fmtTime(r.started_at)}</td>
      <td class="num">${fmtDuration(r.started_at, r.ended_at)}</td>
      <td>
        <button class="action" data-open="${esc(r.id)}">output</button>
        ${stopButton(r)}
      </td>
    </tr>`).join('');
}

// stopButton is offered only for an unfinished run and only to an admin. After being
// asked for it stays visible but inactive: the runner asks every few seconds, so there is
// a delay between the click and the end of the run, and without this feedback the button
// looks like it did nothing.
function stopButton(run) {
  if (run.status !== 'running' && run.status !== 'pending') return '';
  if (run.cancel_requested) {
    return '<button class="action admin-only" disabled>stopping…</button>';
  }
  return `<button class="action admin-only" data-cancel="${esc(run.id)}">stop</button>`;
}

// cancelRun asks for a run to be stopped. The confirmation is deliberate: the backup in
// progress is discarded and the next one happens on schedule.
async function cancelRun(runID) {
  if (!confirm(`Stop run ${runID}? The backup in progress will be discarded.`)) return;
  try {
    await api(`/runs/${encodeURIComponent(runID)}/cancel`, { method: 'POST' });
  } catch (err) {
    showError(err);
    return;
  }
  refresh();
  if (currentRun === runID) {
    try { renderRunMeta(await api('/runs/' + encodeURIComponent(runID))); } catch (_) {}
  }
}

// syncRows redraws the table body row by row instead of throwing it away and building it
// again. A row whose content has not changed therefore stays put together with its button
// — refreshing every few seconds otherwise makes it disappear and reappear right under
// the cursor you are aiming at that button with.
//
// keyOf says what counts as the same row across refreshes; htmlOf produces its cells;
// initRow receives a newly created row so it can add what does not change with the
// content (class, data-*).
function syncRows(body, items, keyOf, htmlOf, initRow) {
  const existing = new Map();
  for (const tr of [...body.children]) {
    if (tr.dataset.key === undefined) tr.remove(); // "loading…" and similar filler
    else existing.set(tr.dataset.key, tr);
  }
  let prev = null;
  for (const item of items) {
    const key = String(keyOf(item));
    const html = htmlOf(item);
    let tr = existing.get(key);
    if (tr) {
      existing.delete(key);
      if (tr.dataset.html !== html) {
        tr.innerHTML = html;
        tr.dataset.html = html;
      }
    } else {
      tr = document.createElement('tr');
      tr.dataset.key = key;
      tr.dataset.html = html;
      tr.innerHTML = html;
      if (initRow) initRow(tr, item);
    }
    const want = prev ? prev.nextSibling : body.firstChild;
    if (tr !== want) body.insertBefore(tr, want);
    prev = tr;
  }
  for (const tr of existing.values()) tr.remove();
}

// markRowStale drops the remembered html of a row that was touched by hand — when a
// button was disabled, for instance. Without it syncRows would see the row as unchanged
// and leave it that way for good.
function markRowStale(node) {
  const tr = node.closest('tr');
  if (tr) delete tr.dataset.html;
}

// The repository figure is a separate request per instance and takes as long as reaching
// the repository takes. It is therefore kept aside and refreshed at its own, slower pace:
// fetching it with every list refresh meant the column flicked through "…" back to the
// value with the table jumping about in width.
const REPO_REFRESH_MS = 60000;
const repoCache = new Map(); // instance id -> { text, at }
let instanceList = [];       // the last loaded list, so it can be redrawn without a request

async function loadInstances() {
  instanceList = (await api('/instances')) || [];
  renderInstances();
  refreshRepoInfo();
}

function renderInstances() {
  const body = el('instances-body');
  if (instanceList.length === 0) {
    body.innerHTML = '<tr><td colspan="6" class="empty">no instances</td></tr>';
    return;
  }
  syncRows(body, instanceList, (i) => i.id, (i) => {
    const repo = repoCache.get(i.id);
    return `
      <td class="mono">${esc(i.id)}</td>
      <td>${esc(i.script)}</td>
      <td>${esc(i.runner_id)}</td>
      <td>${fmtTime(i.next_run)}</td>
      <td class="mono repo">${esc(repo ? repo.text : '…')}</td>
      <td class="actions">
        <button class="action" data-trigger="${esc(i.id)}">run now</button>
        <button class="action admin-only" data-copy="${esc(i.id)}">copy</button>
      </td>`;
  }, (tr, i) => {
    tr.className = 'clickable';
    tr.dataset.instance = i.id;
  });
}

function refreshRepoInfo() {
  const now = Date.now();
  for (const inst of instanceList) {
    const cached = repoCache.get(inst.id);
    if (cached && now - cached.at < REPO_REFRESH_MS) continue;
    // Record it right away so the same instance is not asked about again while the answer
    // is still in flight.
    repoCache.set(inst.id, { text: cached ? cached.text : '…', at: now });
    api(`/instances/${encodeURIComponent(inst.id)}/repo`)
      .then((info) => setRepoText(inst.id, storedSummary(info)))
      .catch(() => setRepoText(inst.id, '—'));
  }
  // There is no point keeping deleted instances in memory.
  const live = new Set(instanceList.map((i) => i.id));
  for (const id of [...repoCache.keys()]) {
    if (!live.has(id)) repoCache.delete(id);
  }
}

function setRepoText(id, text) {
  repoCache.set(id, { text, at: Date.now() });
  if (currentView === 'instances') renderInstances();
}

// storedSummary describes what an instance has stored. File backups have a restic
// repository, database ones rotated dumps — without the second branch an instance that
// has every backup it ever asked for looks as if it had none.
function storedSummary(info) {
  if (info.exists) return `${fmtBytes(info.bytes)} · ${info.snapshots} snap.${offsiteSuffix(info.replica)}`;
  const d = info.dumps;
  if (d && d.count > 0) {
    return `${fmtBytes(d.bytes)} · ${d.count} dump${d.count === 1 ? '' : 's'}${offsiteSuffix(info.replica)}`;
  }
  return '—';
}

// For a file backup the transfer status does not hang off a single run but off the
// repository — without this suffix an instance backed up by restic would say nothing
// about replication. Plain text, because the cell is escaped (the instance id is user
// input); the colour and the details are carried by the card under Administration and the
// warning above the table.
const OFFSITE_MARK = { done: '✓', syncing: '⇅', pending: '⏳', failed: '✗' };

function offsiteSuffix(status) {
  const mark = OFFSITE_MARK[status];
  return mark ? ` · offsite ${mark}` : '';
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
  const date = new Date(iso).toLocaleDateString('en-GB');
  if (days < 0) return `<span class="expiry alert">expired ${esc(date)}</span>`;
  const cls = days <= EXPIRY_ALERT_DAYS ? 'alert' : (days <= EXPIRY_WARN_DAYS ? 'warn' : '');
  return `<span class="expiry ${cls}">${esc(date)}<span class="days"> (${days} d)</span></span>`;
}

// Installing a runner is the one thing that otherwise sends you into the documentation:
// the command contains the server address, and the server knows it better than a person
// copying it down. The instructions therefore belong on the page where the runner is
// approved anyway.
let installInfo = null;

async function renderInstallGuide() {
  if (installInfo) return; // the table refreshes every 5 s, this is static
  const box = el('install-body');
  try {
    installInfo = await api('/install');
  } catch (err) {
    box.innerHTML = `<p class="empty">${esc(err.message)}</p>`;
    return;
  }
  if (!installInfo.enabled) {
    box.innerHTML = '<p class="empty">The bootstrap listener is not running — '
      + '<span class="mono">[bootstrap] listen</span> is missing from '
      + '<span class="mono">server.toml</span>. Without it a new machine has nowhere to '
      + 'download the installer from: it cannot reach the mTLS port without a certificate.</p>';
    return;
  }
  box.innerHTML = `
    <ol>
      <li>On the target machine run as root:
        <div class="cmd">${esc(installInfo.command)}</div>
        <div class="hint">It downloads the binary, generates a private key on the spot,
          installs the <span class="mono">arcatum-runner</span> systemd service and sends a
          certificate request. The key never leaves the machine, only the request is sent.</div>
      </li>
      <li>The runner appears in the table above as ${badge('pending')}. Its id is the
        <span class="mono">hostname -s</span> of that machine.</li>
      <li>Before approving it, check that it matches: hovering over the id shows the
        certificate fingerprint, the source IP of the request is in the column next to the
        status. Compare both against the machine you have just installed — approving it
        issues it a certificate.</li>
      <li>Then assign it what to back up on the <b>Instances</b> tab.</li>
    </ol>
    <p class="hint">Until then the service just keeps retrying, which is fine. Progress on the
      machine: <span class="mono">journalctl -u arcatum-runner -f</span></p>`;
}

async function loadRunners() {
  renderInstallGuide(); // no await: the table does not wait for the instructions
  const list = await api('/runners');
  const body = el('runners-body');
  if (!list || list.length === 0) {
    body.innerHTML = '<tr><td colspan="6" class="empty">no runners</td></tr>';
    return;
  }
  body.innerHTML = list.map((r) => {
    // A pending runner is waiting for approval — that is the moment to check the
    // fingerprint and source address against the host being installed.
    const pending = r.status === 'pending';
    // Revoking is the compromise response: it drops the runner back to pending, so its
    // certificate stops working everywhere and it has to be re-established.
    const actions = pending
      ? `<button class="action" data-approve="${esc(r.id)}">approve</button>
         <button class="action" data-reject="${esc(r.id)}">reject</button>`
      : `<button class="action" data-revoke="${esc(r.id)}">revoke</button>`;
    const ident = pending
      ? (r.enroll_ip ? `from ${esc(r.enroll_ip)}` : '')
      : (r.version ? esc(r.version) : '<span class="muted">—</span>');
    return `
    <tr>
      <td class="mono" title="${esc(r.cert_fingerprint || '')}">${esc(r.id)}</td>
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
    text: `The certificate expires on ${expiring.length} runner${expiring.length === 1 ? '' : 's'}: `
      + expiring.map((r) => `${r.id} (${daysUntil(r.cert_not_after)} d)`).join(', ')
      + '. Issue a new one: arcatum-ca runner -id <id>, or reject the runner and let it ask again.',
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

// applyIdentity shows who is logged in and hides what their role does not allow. The
// server enforces the same split (webAdmin in users.go); this only keeps a viewer from
// clicking buttons that would come back 403.
function applyIdentity(id) {
  me = id;
  el('me-name').textContent = id.name || '';
  const role = el('me-role');
  role.textContent = id.role || '';
  role.className = 'badge ' + (id.role || '');
  document.body.classList.toggle('viewer', id.role !== 'admin');
}

// certWarnings reports certificates that are about to expire. The operator's own
// certificate is only relevant when they authenticated with one — on the web listener
// they log in with a password, and the certificate that then matters is the server's:
// once it lapses, every runner stops trusting it.
function certWarnings(id) {
  if (!id.secured) {
    setWarning('dev', {
      level: 'warn',
      text: 'The server is running without mTLS — runners are not authenticated and jobs '
        + 'are not signed. Intended for development only.',
    });
    return;
  }
  setWarning('dev', null);
  setWarning('me', (id.auth !== 'certificate' || id.days_left > EXPIRY_WARN_DAYS) ? null : {
    level: id.days_left <= EXPIRY_ALERT_DAYS ? 'alert' : 'warn',
    text: id.days_left < 0
      ? `Your certificate (${id.name}) has expired.`
      : `Your certificate (${id.name}) expires in ${id.days_left} d. Issue a new one: `
        + `arcatum-ca admin -name ${id.name}`,
  });
  setWarning('server', id.server_days_left > EXPIRY_WARN_DAYS ? null : {
    level: id.server_days_left <= EXPIRY_ALERT_DAYS ? 'alert' : 'warn',
    text: id.server_days_left < 0
      ? 'The server certificate has expired — runners will stop trusting it.'
      : `The server certificate expires in ${id.server_days_left} d. Issue a new one: `
        + 'arcatum-ca server -hosts <addresses>',
  });
}

// --- login ------------------------------------------------------------------

// Runners authenticate with a certificate, people with a username and password: the web
// UI therefore starts by asking "who am I" and, depending on the answer, shows either the
// application or the login screen.

function showLogin(message) {
  stopPolling();
  stopTail();
  me = null;
  el('app').classList.add('hidden');
  el('login').classList.remove('hidden');
  el('login-error').textContent = message || '';
  el('login-pass').value = '';
  el('login-user').focus();
}

function showApp() {
  el('login').classList.add('hidden');
  el('app').classList.remove('hidden');
}

// startSession loads the identity and starts the application. Used both after logging in
// and when opening the page with a valid cookie.
async function startSession() {
  const id = await api('/whoami'); // 401 → showLogin, see api()
  applyIdentity(id);
  showApp();
  certWarnings(id);
  showView('runs');
  startPolling();
}

// Logging in does not go through api(): a 401 here means "wrong username or password",
// not "the session expired", and must therefore not redraw the screen.
async function submitLogin(event) {
  event.preventDefault();
  const btn = el('login-submit');
  const username = el('login-user').value.trim();
  const password = el('login-pass').value;
  btn.disabled = true;
  el('login-error').textContent = '';
  try {
    const res = await fetch(API + '/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password }),
    });
    if (!res.ok) throw new ApiError(res.status, await errorText(res));
    el('login-pass').value = '';
    await startSession();
  } catch (err) {
    el('login-error').textContent = err.message;
  } finally {
    btn.disabled = false;
  }
}

async function logout() {
  try {
    await api('/logout', { method: 'POST' });
  } catch (err) {
    if (err.status !== 401) showError(err); // 401 = already signed out, nothing to do
  }
  showLogin('Signed out.');
}

// --- account: your own password ----------------------------------------------

async function changeOwnPassword() {
  const btn = el('pw-save');
  const current = el('pw-current').value;
  const next = el('pw-new').value;
  const again = el('pw-again').value;
  el('account-error').textContent = '';
  if (next !== again) {
    el('account-error').textContent = 'the new passwords do not match';
    return;
  }
  btn.disabled = true;
  try {
    await apiSend('/password', 'POST', { current, new: next });
    for (const id of ['pw-current', 'pw-new', 'pw-again']) el(id).value = '';
    // The server ended all sessions including this one — signing in confirms the new password.
    showLogin('Password changed. Sign in with the new password.');
  } catch (err) {
    if (err.status !== 401) el('account-error').textContent = err.message;
  } finally {
    btn.disabled = false;
  }
}

// --- users -------------------------------------------------------------------

// userNote shows the result of an action below the table: an error, or the generated
// password the server sends only once and keeps nowhere in readable form.
function userNote(html, isError) {
  const box = el('users-error');
  box.classList.toggle('hidden', !html);
  box.classList.toggle('err', !!isError);
  box.innerHTML = html || '';
}

async function loadUsers() {
  const list = await api('/users');
  const body = el('users-body');
  if (!list || list.length === 0) {
    body.innerHTML = '<tr><td colspan="6" class="empty">no users</td></tr>';
    return;
  }
  body.innerHTML = list.map((u) => {
    const self = me && u.username === me.name;
    const toRole = u.role === 'admin' ? 'viewer' : 'admin';
    return `
    <tr>
      <td class="mono">${esc(u.username)}${self ? ' <span class="muted">(that is you)</span>' : ''}</td>
      <td>${badge(u.role)}</td>
      <td>${u.disabled ? '<span class="expiry alert">disabled</span>' : '<span class="expiry">active</span>'}</td>
      <td>${fmtTime(u.created_at)}</td>
      <td>${fmtTime(u.last_login)}</td>
      <td>
        <button class="action" data-reset="${esc(u.username)}">new password</button>
        <button class="action" data-role="${esc(u.username)}" data-to="${toRole}">to ${toRole}</button>
        ${u.disabled
          ? `<button class="action" data-enable="${esc(u.username)}">enable</button>`
          : `<button class="action" data-disable="${esc(u.username)}">disable</button>`}
        ${self ? '' : `<button class="action danger" data-deluser="${esc(u.username)}">delete</button>`}
      </td>
    </tr>`;
  }).join('');
}

// createUser adds an account. Without a password given, the server generates one and
// returns it — that is the only moment a password ever leaves Arcatum.
async function createUser() {
  const name = el('new-user-name').value.trim();
  const role = el('new-user-role').value;
  const password = el('new-user-pass').value;
  if (!name) {
    userNote('enter a user name', true);
    return;
  }
  const btn = el('new-user');
  btn.disabled = true;
  try {
    const res = await apiSend('/users', 'POST', { username: name, role, password });
    el('new-user-name').value = '';
    el('new-user-pass').value = '';
    userNote(res.password
      ? `User <b>${esc(res.username)}</b> created. Password (shown only now): `
        + `<span class="generated">${esc(res.password)}</span>`
      : `User <b>${esc(res.username)}</b> created.`);
    await loadUsers();
  } catch (err) {
    if (err.status !== 401) userNote(esc(err.message), true);
  } finally {
    btn.disabled = false;
  }
}

// updateUser sends one change (role, disabling, a new password) and redraws the table.
async function updateUser(name, payload, note) {
  try {
    const res = await apiSend(`/users/${encodeURIComponent(name)}`, 'PUT', payload);
    userNote(res.password
      ? `New password for user <b>${esc(name)}</b> (shown only now): `
        + `<span class="generated">${esc(res.password)}</span>`
      : note || '');
    await loadUsers();
  } catch (err) {
    if (err.status !== 401) userNote(esc(err.message), true);
  }
}

// The form for a new password on someone else's account. The password can be typed in —
// having the server generate it is one option, not the only route.
function openPasswordReset(name) {
  el('pw-reset-user').textContent = name;
  el('pw-reset').dataset.user = name;
  el('pw-reset').classList.remove('hidden');
  el('pw-reset-value').value = '';
  el('pw-reset-value').focus();
  userNote('');
}

function closePasswordReset() {
  el('pw-reset').classList.add('hidden');
  el('pw-reset').dataset.user = '';
  el('pw-reset-value').value = '';
}

// submitPasswordReset sends the password given, or asks the server to generate one. The
// longer check (the minimum length) is done by the server, so it applies equally to the
// API and the UI.
async function submitPasswordReset(generate) {
  const name = el('pw-reset').dataset.user;
  if (!name) return;
  const value = el('pw-reset-value').value;
  if (!generate && !value) {
    userNote('enter a password, or have one generated', true);
    el('pw-reset-value').focus();
    return;
  }
  const payload = generate ? { generate_password: true } : { password: value };
  closePasswordReset();
  await updateUser(name, payload, `The password of user <b>${esc(name)}</b> was changed.`);
}

async function deleteUser(name) {
  try {
    await api(`/users/${encodeURIComponent(name)}`, { method: 'DELETE' });
    userNote(`User <b>${esc(name)}</b> deleted.`);
    await loadUsers();
  } catch (err) {
    if (err.status !== 401) userNote(esc(err.message), true);
  }
}

// --- restore ----------------------------------------------------------------

// Browsing happens against the repository on the server, so a restore does not depend on
// the backed-up host being reachable — which is often exactly why you need one.
const restore = { instance: '', snapshot: '', path: '/' };

async function loadRestore() {
  const sel = el('restore-instance');
  if (sel.options.length <= 1) {
    const list = await api('/instances');
    sel.innerHTML = '<option value="">— pick —</option>'
      + list.map((i) => `<option value="${esc(i.id)}">${esc(i.id)}</option>`).join('');
  }
  if (!restore.instance) {
    el('restore-body').innerHTML = '<tr><td colspan="5" class="empty">pick an instance</td></tr>';
  }
}

// loadSnapshots decides what is actually being restored. A file instance has a restic
// repository to browse; a database one has rotated dumps, where a "restore" is simply
// downloading a single file. Without this switch a database instance only showed an error
// about a repository that never existed.
async function loadSnapshots() {
  const sel = el('restore-snapshot');
  el('restore-info').textContent = '';
  el('restore-crumbs').innerHTML = '';
  if (!restore.instance) {
    sel.innerHTML = '<option value="">…</option>';
    return;
  }
  sel.innerHTML = '<option value="">loading…</option>';
  sel.disabled = false; // a previous pick of a database instance may have disabled it

  let info = null;
  try {
    info = await api(`/instances/${encodeURIComponent(restore.instance)}/repo`);
  } catch (_) { /* decided by the snapshots below */ }
  if (info && !info.exists && info.dumps && info.dumps.count > 0) {
    await loadDumps();
    return;
  }
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
    sel.innerHTML = '<option value="">no snapshots</option>';
    el('restore-body').innerHTML =
      '<tr><td colspan="5" class="empty">this instance has not backed anything up yet</td></tr>';
    return;
  }
  sel.innerHTML = snaps.map((s) =>
    `<option value="${esc(s.id)}">${fmtTime(s.time)} · ${esc(s.short_id)}</option>`).join('');
  restore.snapshot = snaps[0].id;
  // Start at the directory the snapshot was taken from rather than at "/".
  restore.path = (snaps[0].paths && snaps[0].paths[0]) || '/';
  el('restore-info').textContent = `${snaps.length} snapshot${snaps.length === 1 ? '' : 's'}`;
  await loadRestoreDir();
}

// loadDumps lists an instance's stored dumps. A dump is one opaque artifact — nobody goes
// inside it, a restore means downloading the whole thing — so there is nothing to browse.
async function loadDumps() {
  const sel = el('restore-snapshot');
  const body = el('restore-body');
  sel.innerHTML = '<option value="">— dumps —</option>';
  sel.disabled = true;

  let dumps;
  try {
    dumps = await api(`/instances/${encodeURIComponent(restore.instance)}/dumps`);
  } catch (err) {
    body.innerHTML = `<tr><td colspan="5" class="empty">${esc(err.message)}</td></tr>`;
    return;
  }
  if (!dumps || dumps.length === 0) {
    body.innerHTML = '<tr><td colspan="5" class="empty">this instance has no stored dump yet</td></tr>';
    return;
  }
  const total = dumps.reduce((sum, d) => sum + d.bytes, 0);
  el('restore-info').textContent =
    `${dumps.length} dump${dumps.length === 1 ? '' : 's'} · ${fmtBytes(total)}`;
  body.innerHTML = dumps.map((d) => `
    <tr>
      <td class="mono">${esc(d.run_id)}</td>
      <td>dump</td>
      <td class="num">${fmtBytes(d.bytes)}</td>
      <td>${fmtTime(d.ended_at)}</td>
      <td><a class="action" href="${API}/runs/${encodeURIComponent(d.run_id)}/data" download>download</a></td>
    </tr>`).join('');
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
  body.innerHTML = '<tr><td colspan="5" class="empty">loading…</td></tr>';

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
      <td class="mono">.</td><td>directory</td><td></td><td></td>
      <td><a class="action" href="${esc(dl(restore.path, true))}">download as .tar</a></td>
    </tr>`);
  }
  for (const e of res.entries) {
    const isDir = e.type === 'dir';
    rows.push(`<tr${isDir ? ' class="clickable" data-dir="' + esc(e.path) + '"' : ''}>
      <td class="mono">${isDir ? '📁 ' : ''}${esc(e.name)}</td>
      <td>${isDir ? 'directory' : esc(e.type)}</td>
      <td class="num">${isDir ? '' : fmtBytes(e.size)}</td>
      <td>${isDir ? '' : fmtTime(e.mtime)}</td>
      <td><a class="action" href="${esc(dl(e.path, isDir))}">${isDir ? 'download .tar' : 'download'}</a></td>
    </tr>`);
  }
  if (rows.length === 0) {
    body.innerHTML = '<tr><td colspan="5" class="empty">empty directory</td></tr>';
    return;
  }
  if (res.truncated) {
    rows.push('<tr><td colspan="5" class="empty">the listing is truncated — too many entries</td></tr>');
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

// --- instance form ----------------------------------------------------------

// Instances are edited here rather than in a JSON file on the server: the form is built
// from the parameters the script declares, so a missing password or a mistyped name is
// caught on save, and secrets are encrypted from the moment they are stored.
let scriptsCache = null;
let editing = null;     // instance id being edited, null when creating
let copyingFrom = null; // instance the form was seeded from, null when not copying

async function scripts() {
  if (!scriptsCache) scriptsCache = await api('/scripts');
  return scriptsCache;
}

function fieldRow(label, inner, hint) {
  return `<div class="field"><label>${esc(label)}</label><div>${inner}`
    + (hint ? `<div class="hint">${esc(hint)}</div>` : '') + '</div></div>';
}

// runnerField offers the runners the server knows about. Typing an id by hand means
// hitting the certificate CN on the first try — and a typo does not give an error, it
// gives an instance that simply never runs, because no runner checks in for it.
async function runnerField(current) {
  let list;
  try {
    list = await api('/runners');
  } catch (_) {
    // An unavailable list must not block creating an instance.
    return textRunnerField(current, 'the runner list could not be loaded — fill the id in by '
      + 'hand, it must match the CN of the runner certificate');
  }
  // There is no point offering rejected runners; pending ones yes — an instance can be
  // prepared before it gets approved.
  const rank = { approved: 0, pending: 1 };
  const opts = (list || [])
    .filter((r) => r.status === 'approved' || r.status === 'pending')
    .sort((a, b) => (rank[a.status] - rank[b.status]) || a.id.localeCompare(b.id))
    .map((r) => ({ id: r.id, label: r.status === 'approved' ? r.id : `${r.id} (awaiting approval)` }));

  if (!opts.length && !current) {
    return textRunnerField(current, 'no runner has registered yet — the id must match the CN '
      + 'of the runner certificate');
  }
  // An instance may point at a runner that has since been rejected or deleted. Overwriting
  // it just because it disappeared from the list would be worse than keeping it on offer.
  if (current && !opts.some((o) => o.id === current)) {
    opts.unshift({ id: current, label: `${current} (not in the list)` });
  }
  const html = '<select id="f-runner">'
    + (current ? '' : '<option value="">— pick a runner —</option>')
    + opts.map((o) =>
        `<option value="${esc(o.id)}"${o.id === current ? ' selected' : ''}>${esc(o.label)}</option>`).join('')
    + '</select>';
  return { html, hint: 'the runner that will execute the instance' };
}

function textRunnerField(current, hint) {
  return { html: `<input id="f-runner" value="${esc(current)}" placeholder="web-01">`, hint };
}

// openInstanceForm opens the form over instance id, or an empty one when id is null.
// With opts.copy the id is taken as a template: the form is filled in from it, but a new
// instance is being created — a second database on the same server then differs by two
// fields rather than by filling everything in again.
async function openInstanceForm(id, opts) {
  const copy = !!(opts && opts.copy) && !!id;
  editing = copy ? null : (id || null);
  copyingFrom = copy ? id : null;
  el('form-error').textContent = '';
  el('form-title').textContent = copy ? `Copy of instance ${id}` : (id || 'New instance');
  el('form-delete').classList.toggle('hidden', !editing);
  showView('instance-form');

  const list = await scripts();
  let inst = null;
  if (id) {
    const all = await api('/instances');
    inst = all.find((i) => i.id === id) || null;
  }
  const chosen = inst ? inst.script : (list[0] && list[0].name) || '';
  const runner = await runnerField(inst ? inst.runner_id : '');

  el('form-body').innerHTML =
    fieldRow('id', editing
      ? `<input id="f-id" value="${esc(editing)}" disabled>`
      : `<input id="f-id" placeholder="${esc(copy ? id + '-2' : 'mysql-web01')}">`,
      editing ? 'the id cannot be changed'
        : (copy ? `a new id — the rest is prefilled from ${id}, passwords included`
          : 'a short identifier, also used as the repository name'))
    + fieldRow('script', `<select id="f-script">${list.map((sc) =>
        `<option value="${esc(sc.name)}"${sc.name === chosen ? ' selected' : ''}>${esc(sc.name)} (${esc(sc.type)})</option>`).join('')}</select>`)
    + fieldRow('runner', runner.html, runner.hint)
    + fieldRow('schedule', `
        <select id="f-freq">
          ${['daily', 'weekly', 'monthly'].map((f) =>
            `<option value="${f}"${inst && inst.schedule.frequency === f ? ' selected' : ''}>${f}</option>`).join('')}
        </select>
        <input id="f-time" value="${esc(inst ? inst.schedule.time : '02:00')}" placeholder="02:00" size="6">
        <input id="f-weekdays" value="${esc(inst && inst.schedule.weekdays ? inst.schedule.weekdays.join(',') : '')}" placeholder="mon,thu" size="12">
        <input id="f-day" value="${inst && inst.schedule.day ? inst.schedule.day : ''}" placeholder="day 1-28" size="8">
        <input id="f-tz" value="${esc(inst ? inst.schedule.timezone || '' : '')}" placeholder="Europe/Prague" size="16">`,
        'weekdays applies to weekly, day to monthly')
    + fieldRow('timeout', `<input id="f-timeout" value="${esc(inst ? inst.timeout : '')}" placeholder="1h" size="8">`)
    + fieldRow('backup retention', `
        <input id="f-keep-last" type="number" min="0" value="${keepDefault(inst, 'keep_last', 7)}" size="4">
        <span class="hint">last</span>
        <input id="f-keep-days" type="number" min="0" value="${keepDefault(inst, 'keep_days', 0)}" size="4">
        <span class="hint">days</span>`,
        'applies only to scripts that stream data (a database dump); 0 = unlimited, '
        + 'both at once means "at least this many copies and at least this old"')
    + '<div id="f-params"></div>';

  el('f-script').addEventListener('change', () => { renderParams(inst); toggleRetention(); });
  renderParams(inst);
  toggleRetention();
  if (copy) el('f-id').focus(); // the only thing a copy is still missing
}

// keepDefault prefills the retention for a new instance. For an existing one the stored
// value is left alone — otherwise merely opening the form would start deleting backups.
function keepDefault(inst, field, fallback) {
  if (inst) return inst[field] || 0;
  return fallback;
}

// toggleRetention hides the retention for scripts that produce no dumps. With restic the
// retention is set through the keep_daily/keep_weekly parameters, which the runner uses.
async function toggleRetention() {
  const list = await scripts();
  const sc = list.find((s) => s.name === el('f-script').value);
  const row = el('f-keep-last').closest('.field');
  if (row) row.classList.toggle('hidden', !sc || sc.capture !== 'stream');
}

// renderParams builds the inputs for whatever the selected script declares.
async function renderParams(inst) {
  const list = await scripts();
  const name = el('f-script').value;
  const sc = list.find((s) => s.name === name);
  const box = el('f-params');
  if (!sc || sc.params.length === 0) {
    box.innerHTML = '<div class="field"><label>parameters</label><div class="hint">this script declares none</div></div>';
    return;
  }
  const same = inst && inst.script === name;
  box.innerHTML = sc.params.map((p) => {
    // A stored secret comes back masked; leaving the field untouched keeps it. A field
    // that has nothing stored yet starts on the manifest's default, so what will be saved
    // is on screen rather than implied — including the placeholder repository password.
    const value = same
      ? (p.secret ? (inst.secrets[p.name] || '') : (inst.params[p.name] || ''))
      : (p.default || '');
    const hint = [
      p.type || 'string',
      p.required ? 'required' : 'optional',
      p.secret ? 'secret' : '',
      p.default ? `default ${p.default}` : '',
      // The API never lets a secret value out, so even in a copy only the mask is on
      // screen — this says it will be carried over anyway, and that it can be overwritten.
      (p.secret && same && copyingFrom) ? `taken over from ${copyingFrom}` : '',
    ].filter(Boolean).join(' · ');
    return fieldRow(p.name,
      `<input data-param="${esc(p.name)}" data-secret="${p.secret}" value="${esc(value)}"`
      + `${p.secret ? ' placeholder="(unchanged)"' : ''}>`, hint);
  }).join('');
}

// keepValue reads a retention field; empty and nonsensical alike mean 0, i.e. unlimited.
function keepValue(id) {
  const n = parseInt(el(id).value, 10);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

function collectInstance() {
  const params = {};
  const secrets = {};
  for (const input of el('f-params').querySelectorAll('[data-param]')) {
    const name = input.dataset.param;
    const value = input.value;
    if (input.dataset.secret === 'true') {
      // An untouched secret field is left out, so the stored value survives.
      if (value !== '') secrets[name] = value;
    } else if (value !== '') {
      params[name] = value;
    }
  }
  const weekdays = el('f-weekdays').value.split(',').map((s) => s.trim()).filter(Boolean);
  const day = parseInt(el('f-day').value, 10);
  return {
    id: el('f-id').value.trim(),
    // Given a template, the server fills in the secrets that only reached the form as a mask.
    copy_from: copyingFrom || '',
    script: el('f-script').value,
    runner_id: el('f-runner').value.trim(),
    timeout: el('f-timeout').value.trim(),
    keep_last: keepValue('f-keep-last'),
    keep_days: keepValue('f-keep-days'),
    params, secrets,
    schedule: {
      frequency: el('f-freq').value,
      time: el('f-time').value.trim(),
      weekdays: weekdays.length ? weekdays : null,
      day: Number.isFinite(day) ? day : 0,
      timezone: el('f-tz').value.trim(),
    },
  };
}

// --- key rotation -----------------------------------------------------------

// Rotating a long-lived key means running an overlap window: new material is published,
// runners adopt it by themselves, and the window is closed once the server can confirm
// everyone has moved. This view shows how far along each of the three is.
async function loadRotation() {
  const box = el('rotation-body');
  let st;
  try {
    st = await api('/rotation');
  } catch (err) {
    box.innerHTML = `<p class="empty">${esc(err.message)}</p>`;
    return;
  }

  const secretsRotating = st.secrets_keys > 1;
  const secretsDone = st.secrets_pending === 0;
  const cas = st.trusted_cas || [];
  const caRotating = cas.length > 1;
  const stale = (st.runners_on_old_ca || []).concat(st.runners_unknown_ca || []);

  box.innerHTML = `
    <div class="rot">
      <h3>Secrets — master key</h3>
      <dl class="meta">
        <dt>current key</dt><dd>${st.secrets_key_id ? esc(st.secrets_key_id) : '<span class="muted">none (plaintext)</span>'}</dd>
        <dt>keys loaded</dt><dd>${st.secrets_keys}${secretsRotating ? ' <span class="expiry warn">rotation in progress</span>' : ''}</dd>
        <dt>not re-encrypted</dt><dd>${st.secrets_pending}${secretsDone ? '' : ' <span class="expiry warn">still to re-encrypt</span>'}</dd>
      </dl>
      ${st.secrets_key_id ? `<div class="section-foot">
        <button id="rekey" class="action">re-encrypt everything with the current key</button>
        <span class="hint">safe to run repeatedly — values already re-encrypted are skipped</span>
      </div>` : ''}
    </div>

    <div class="rot">
      <h3>Job signing</h3>
      <dl class="meta">
        <dt>trusted keys</dt><dd>${st.signing_keys}${st.signing_keys > 1
          ? ' <span class="expiry warn">rotation in progress — once the runners have moved, remove the old one from previous_keys</span>' : ''}</dd>
      </dl>
    </div>

    <div class="rot">
      <h3>Certificate authority</h3>
      <dl class="meta">
        <dt>issuing under</dt><dd>${st.signing_ca ? esc(st.signing_ca) : '<span class="muted">—</span>'}</dd>
        <dt>trusted CAs</dt><dd>${cas.length === 0 ? '<span class="muted">—</span>' : cas.map((c) =>
          `${esc(c.common_name)}${c.is_signer ? ' <span class="expiry">(issuing)</span>' : ''}`
          + ` <span class="days">· valid for ${c.days_left} d</span>`).join('<br>')}</dd>
        ${caRotating || stale.length ? `<dt>runners on the old CA</dt><dd>${
          stale.length === 0 ? '<span class="expiry">none</span>' : esc(stale.join(', '))}</dd>` : ''}
      </dl>
      ${caRotating ? `<div class="section-foot">
        ${st.safe_to_drop_old_ca
          ? '<span class="expiry">All runners are on the current CA — the old one can be removed from the bundle.</span>'
          : '<span class="expiry warn">Do not remove the old CA yet: some runners still rely on it.</span>'}
      </div>` : ''}
    </div>`;
}

// --- administration ---------------------------------------------------------

// Three operations concerning Arcatum itself rather than what it backs up: download the
// configuration, upload it back, and empty the server of everything it has collected.
//
// The import and the reset are both destructive, each in its own way, so neither can be
// launched with a single click. The import first shows what it would do and is confirmed
// only afterwards; the reset is confirmed by typing a word. Keys are in neither of them —
// those sit on disk and the web UI does not touch them.

// pendingArchive holds the uploaded file between the check and the confirmation. The
// server receives the same bytes twice: the second time with confirm, so what is
// confirmed is exactly what was checked, not whatever is in the form by then.
let pendingArchive = null;

function filenameFrom(header) {
  const m = /filename="([^"]+)"/.exec(header || '');
  return m ? m[1] : '';
}

// exportConfig downloads through fetch rather than a link: if the server answered with an
// error or an expired session, the browser would show it as a bare page instead of a
// message inside the web UI.
async function exportConfig(btn) {
  const note = el('config-export-note');
  btn.disabled = true;
  note.textContent = 'preparing…';
  try {
    const res = await fetch(API + '/config/export');
    if (res.status === 401) {
      showLogin('Your session expired, please sign in again.');
      return;
    }
    if (!res.ok) throw new Error(await errorText(res));
    const blob = await res.blob();
    const name = filenameFrom(res.headers.get('Content-Disposition')) || 'arcatum-config.zip';
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
    note.textContent = `downloaded: ${name} (${fmtBytes(blob.size)})`;
  } catch (err) {
    note.textContent = 'failed: ' + err.message;
  } finally {
    btn.disabled = false;
  }
}

function clearConfigPlan() {
  pendingArchive = null;
  const box = el('config-plan');
  if (box) box.innerHTML = '';
}

// checkConfigArchive sends the archive without confirm — the server verifies it and
// answers with a plan, but changes nothing.
async function checkConfigArchive(btn) {
  const input = el('config-file');
  const file = input.files && input.files[0];
  const box = el('config-plan');
  if (!file) {
    box.innerHTML = '<p class="empty">pick an archive file first</p>';
    return;
  }
  btn.disabled = true;
  box.innerHTML = '<p class="empty">checking…</p>';
  try {
    const bytes = await file.arrayBuffer();
    const plan = await postArchive(bytes, false);
    pendingArchive = bytes;
    renderConfigPlan(plan, file.name);
  } catch (err) {
    pendingArchive = null;
    if (err.status !== 401) box.innerHTML = `<p class="empty">archive refused: ${esc(err.message)}</p>`;
  } finally {
    btn.disabled = false;
  }
}

// apply = false is a check only: the server verifies the archive, computes the difference
// and writes nothing.
function postArchive(bytes, apply) {
  return api('/config/import' + (apply ? '?confirm=replace-all' : ''), {
    method: 'POST',
    headers: { 'Content-Type': 'application/zip' },
    body: bytes,
  });
}

// diffText describes one table in one sentence. The names are listed, not just the counts
// — "−3 will disappear" helps nobody until they know which.
function diffText(d) {
  const parts = [];
  if (d.added.length) parts.push(`<b>+${d.added.length}</b> added (${d.added.map(esc).join(', ')})`);
  if (d.changed.length) parts.push(`<b>~${d.changed.length}</b> changed (${d.changed.map(esc).join(', ')})`);
  if (d.removed.length) {
    parts.push(`<span class="expiry alert"><b>−${d.removed.length}</b> will disappear</span>`
      + ` (${d.removed.map(esc).join(', ')})`);
  }
  if (d.unchanged) parts.push(`${d.unchanged} unchanged`);
  return parts.length ? parts.join(' · ') : '<span class="muted">unchanged</span>';
}

function renderConfigPlan(plan, fileName) {
  const m = plan.manifest || {};
  el('config-plan').innerHTML = `
    <dl class="meta">
      <dt>archive</dt><dd>${esc(fileName)}</dd>
      <dt>created</dt><dd>${fmtTime(m.created_at)}${m.host ? ' on ' + esc(m.host) : ''}
        ${m.arcatum ? `<span class="days">· Arcatum ${esc(m.arcatum)}</span>` : ''}</dd>
      <dt>instances</dt><dd>${diffText(plan.instances)}</dd>
      <dt>users</dt><dd>${diffText(plan.users)}</dd>
      <dt>runners</dt><dd>${diffText(plan.runners)}</dd>
    </dl>
    ${(plan.warnings || []).map((wtext) =>
      `<div class="warning warn">${esc(wtext)}</div>`).join('')}
    <div class="section-foot">
      <input id="config-confirm" placeholder="REPLACE" size="12" autocapitalize="characters"
             spellcheck="false" autocomplete="off">
      <button id="config-apply" class="action danger">replace the configuration</button>
      <span class="hint">to confirm, type REPLACE · the current configuration saves itself
        into backup_dir/config-backups beforehand</span>
    </div>
    <div id="config-note" class="hint"></div>`;
}

async function applyConfigArchive(btn) {
  const note = el('config-note');
  if (!pendingArchive) {
    note.textContent = 'the archive is no longer loaded, check it again';
    return;
  }
  if (el('config-confirm').value.trim().toUpperCase() !== 'REPLACE') {
    note.textContent = 'type REPLACE into the field';
    return;
  }
  btn.disabled = true;
  note.textContent = 'replacing…';
  try {
    const plan = await postArchive(pendingArchive, true);
    clearConfigPlan();
    // The import deleted every session, this one included — the login screen is the only
    // thing it makes sense to show now.
    showLogin(`Configuration replaced (${plan.instances.added.length + plan.instances.unchanged
      + plan.instances.changed.length} instances). Sign in again.`);
  } catch (err) {
    if (err.status !== 401) note.textContent = 'failed: ' + err.message;
    btn.disabled = false;
  }
}

// loadAdmin only refreshes the reset summary. An import plan in progress is left alone —
// overwriting it under someone's hands mid-confirmation is the last thing anyone wants here.
async function loadAdmin() {
  await loadReplicaCard();
  const box = el('reset-summary');
  let st;
  try {
    st = await api('/reset');
  } catch (err) {
    box.innerHTML = `<p class="empty">${esc(err.message)}</p>`;
    return;
  }
  box.innerHTML = `
    <dl class="meta">
      <dt>will be deleted</dt><dd>${st.runs} runs · ${fmtBytes(st.log_bytes)} of logs
        · ${fmtBytes(st.data_bytes)} of dumps · ${st.repositories} restic repositories</dd>
      <dt>will stay</dt><dd>${st.kept.instances} instances · ${st.kept.users} users
        · ${st.kept.runners} runners · the keys on disk</dd>
    </dl>`;
}

async function runReset(btn) {
  const note = el('reset-note');
  if (el('reset-confirm').value.trim().toUpperCase() !== 'DELETE') {
    note.textContent = 'type DELETE into the field';
    return;
  }
  if (!confirm('Delete all backups, dumps, logs and the run history?\n\n'
    + 'Only the keys, users, instances and runners will stay. This cannot be undone '
    + 'and the data is nowhere else — Arcatum is the last place it sits.')) return;
  btn.disabled = true;
  note.textContent = 'deleting…';
  try {
    const res = await api('/reset?confirm=delete-all-backups', { method: 'POST' });
    el('reset-confirm').value = '';
    note.textContent = `deleted: ${res.runs} runs, ${res.repositories} repositories`
      + `, ${fmtBytes(res.log_bytes + res.data_bytes)} of data`;
    await loadAdmin();
  } catch (err) {
    if (err.status !== 401) note.textContent = 'failed: ' + err.message;
  } finally {
    btn.disabled = false;
  }
}

// --- off-site replica -------------------------------------------------------

// The replica status is fetched continuously, not only on the Administration tab: a
// transfer outage should be visible from anywhere, otherwise an operator would learn about
// it only at the moment they reached for the off-site copy. This also feeds the warning
// above the table and the reasons in the run detail.
let replicaState = null;

async function refreshReplica() {
  try {
    replicaState = await api('/replica');
  } catch (err) {
    if (err.status === 401) return;
    return; // an unavailable API is reported by showError; a second message would only mask the first
  }
  replicaFailures.clear();
  for (const it of replicaState.failing || []) {
    replicaFailures.set(`${it.kind}:${it.key}`, it);
  }
  replicaWarning(replicaState);
}

// When an outage should shout. A brief hiccup on the link catches up on its own and
// reporting it would only teach people to ignore warnings; an hour without a connection,
// or an item that repeatedly failed to transfer, already means the backups have no second
// copy.
const REPLICA_DOWN_ALERT_MS = 3600000;
const REPLICA_BACKLOG_WARN_MS = 7200000;

function replicaWarning(st) {
  if (!st || !st.enabled) { setWarning('replica', null); return; }
  // Whether the link is down is `healthy`, not a timestamp: Go serializes an unset time as
  // 0001-01-01, which is a perfectly truthy string over here and would date the outage to
  // the year one. down_since only decides how long to say it has lasted.
  const downMs = (!st.healthy && st.link && hasTime(st.link.down_since))
    ? Date.now() - new Date(st.link.down_since) : 0;
  if (!st.healthy) {
    setWarning('replica', {
      level: downMs >= REPLICA_DOWN_ALERT_MS ? 'alert' : 'warn',
      text: `The off-site replica (${st.target}) is unreachable`
        + `${downMs > 0 ? ` (for ${fmtAge(downMs)})` : ''}`
        + `${st.counts.pending + st.counts.failed > 0
          ? ` — ${st.counts.pending + st.counts.failed} items are waiting to be sent` : ''}`
        + `. Backups here keep running and the queue catches up once the link is repaired. `
        + `Last error: ${st.link.last_err || '—'}`,
    });
    return;
  }
  if (st.counts.failed > 0) {
    setWarning('replica', {
      level: 'alert',
      text: `${st.counts.failed} items are failing to transfer to the off-site server (${st.target}). `
        + `Last error: ${st.link.last_err || (st.failing && st.failing[0] && st.failing[0].err) || '—'}`,
    });
    return;
  }
  // Same trap as down_since: with nothing queued the server sends the zero time, and
  // "waiting since the year one" would put a permanent warning above every tab.
  const oldest = hasTime(st.counts.oldest_pending_at)
    ? Date.now() - new Date(st.counts.oldest_pending_at) : 0;
  if (oldest > REPLICA_BACKLOG_WARN_MS) {
    setWarning('replica', {
      level: 'warn',
      text: `The off-site replica is falling behind: the oldest untransferred item has been `
        + `waiting ${fmtAge(oldest)} (${st.counts.pending} queued).`,
    });
    return;
  }
  setWarning('replica', null);
}

function fmtAge(ms) {
  const min = Math.floor(ms / 60000);
  if (min < 60) return `${min} min`;
  const hours = Math.floor(min / 60);
  if (hours < 48) return `${hours} h`;
  return `${Math.floor(hours / 24)} d`;
}

async function loadReplicaCard() {
  const box = el('replica-summary');
  await refreshReplica();
  const st = replicaState;
  if (!st) { box.innerHTML = '<p class="empty">the status could not be loaded</p>'; return; }
  if (!st.enabled) {
    box.innerHTML = '<p class="empty">The off-site replica is not configured — see '
      + '<code>[replica]</code> in <code>server.toml</code>.</p>';
    return;
  }
  const health = st.healthy
    ? '<span class="badge success">reachable</span>'
    : `<span class="badge failed">unreachable</span> <span class="hint">since ${fmtTime(st.link.down_since)}</span>`;
  box.innerHTML = `
    <dl class="meta">
      <dt>target</dt><dd class="mono">${esc(st.target)}</dd>
      <dt>link</dt><dd>${health}</dd>
      <dt>last success</dt><dd>${fmtTime(st.link.last_ok_at)}</dd>
      <dt>queue</dt><dd>${st.counts.pending} waiting · ${st.counts.syncing} sending
        · ${st.counts.done} done · ${st.counts.failed} failing</dd>
      <dt>mode</dt><dd>${st.mirror ? 'mirror (deletions propagate to the replica)' : 'append only'}
        · ${st.include_keys ? 'including the keys and the database' : 'without the keys'}</dd>
      ${st.link.last_err ? `<dt>last error</dt><dd>${esc(st.link.last_err)}</dd>` : ''}
    </dl>
    ${replicaFailingList(st)}`;
}

function replicaFailingList(st) {
  if (!st.failing || st.failing.length === 0) return '';
  return '<table class="sub"><thead><tr><th>item</th><th>attempts</th><th>error</th></tr></thead><tbody>'
    + st.failing.map((it) => `<tr>
        <td class="mono">${esc(it.kind)}:${esc(it.key)}</td>
        <td class="num">${it.attempts}</td>
        <td>${esc(it.err || '')}</td>
      </tr>`).join('')
    + '</tbody></table>';
}

async function replicaAction(path, btn, doing) {
  const note = el('replica-note');
  btn.disabled = true;
  note.textContent = doing;
  try {
    await api(path, { method: 'POST' });
    note.textContent = 'queued, the transfer runs in the background';
    await loadReplicaCard();
  } catch (err) {
    if (err.status !== 401) note.textContent = 'failed: ' + err.message;
  } finally {
    btn.disabled = false;
  }
}

const loaders = {
  runs: loadRuns,
  instances: loadInstances,
  restore: loadRestore,
  runners: loadRunners,
  rotation: loadRotation,
  users: loadUsers,
  admin: loadAdmin,
};

const VIEWS = ['runs', 'instances', 'instance-form', 'restore', 'runners', 'rotation',
  'users', 'admin', 'account', 'detail'];

async function refresh() {
  if (!me) return; // signed out: nothing to fetch for
  // The off-site replica status is fetched on every refresh whichever tab is open: a
  // warning about a transfer outage must not wait for someone to visit Administration.
  if (currentView !== 'admin') refreshReplica();
  const load = loaders[currentView];
  if (!load) return; // the detail, the form and the account do not refresh periodically
  try {
    await load();
  } catch (err) {
    if (err.status !== 401) showError(err);
  }
}

function startPolling() {
  if (listTimer) return;
  listTimer = setInterval(refresh, LIST_REFRESH_MS);
}

function stopPolling() {
  if (listTimer) { clearInterval(listTimer); listTimer = null; }
}

function showView(name) {
  currentView = name;
  for (const v of VIEWS) {
    el('view-' + v).classList.toggle('hidden', v !== name);
  }
  for (const tab of document.querySelectorAll('.tab')) {
    tab.classList.toggle('active', tab.dataset.view === name);
  }
  if (name === 'users') userNote('');
  if (name !== 'admin') clearConfigPlan(); // an import in progress is dropped when leaving the tab
  closePasswordReset(); // a password change in progress is dropped when leaving the table
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
  renderStopButton(run);
  el('detail-meta').innerHTML = `
    <dt>instance</dt><dd>${esc(run.instance_id)}</dd>
    <dt>script</dt><dd>${esc(run.script)}</dd>
    <dt>runner</dt><dd>${esc(run.runner_id)}</dd>
    <dt>exit code</dt><dd>${run.exit_code}</dd>
    ${dataRow(run)}
    <dt>log</dt><dd>${fmtBytes(run.bytes)}</dd>
    <dt>started</dt><dd>${fmtTime(run.started_at)}</dd>
    <dt>duration</dt><dd>${fmtDuration(run.started_at, run.ended_at)}</dd>
    ${replicaRow(run)}
    ${run.err ? `<dt>error</dt><dd>${esc(run.err)}</dd>` : ''}`;
}

// replicaRow says whether this particular backup is on the off-site server too. For a
// waiting or failing item the reason is shown as well — without it "queued" is a mere
// claim and the operator has to look elsewhere for why nothing is moving.
function replicaRow(run) {
  const status = run.replica_status;
  if (!status) return '';
  // A run's data is carried either by its own directory (a dump) or by the instance's
  // repository (restic), so both are looked up — in the same order the server picks the
  // status in.
  const it = replicaFailures.get(`run:${run.id}`) || replicaFailures.get(`repo:${run.instance_id}`);
  let detail = '';
  if (status !== 'done' && it) {
    detail = ` <span class="hint">${esc(it.err || '')}</span>`;
    if (it.attempts > 1) detail += ` <span class="hint">(attempt ${it.attempts})</span>`;
  }
  return `<dt>offsite</dt><dd>${replicaBadge(status)}${detail}</dd>`;
}

// The failing queue items, so the run detail can say why. Filled from the replica status,
// which is fetched for the warning anyway — a second request per run would add nothing.
const replicaFailures = new Map();

// renderStopButton drives the button in the detail header. Hidden for a finished run,
// inactive once the stop is already under way — the runner asks every few seconds.
function renderStopButton(run) {
  const btn = el('detail-stop');
  const stoppable = run.status === 'running' || run.status === 'pending';
  btn.classList.toggle('hidden', !stoppable);
  btn.disabled = !!run.cancel_requested;
  btn.textContent = run.cancel_requested ? 'stopping…' : 'stop';
  btn.dataset.run = run.id;
}

// dataRow shows the backed-up data separately from the log. It is offered for download
// only after a successful run — the server discards an unfinished dump, so the link would
// otherwise lead nowhere.
function dataRow(run) {
  if (!run.data_bytes) return '';
  const size = fmtBytes(run.data_bytes);
  if (run.status !== 'success') {
    return `<dt>data</dt><dd>${size} <span class="hint">(the run failed, the data was discarded)</span></dd>`;
  }
  const href = `${API}/runs/${encodeURIComponent(run.id)}/data`;
  return `<dt>data</dt><dd>${size} · <a href="${href}" download>download</a></dd>`;
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

el('login-form').addEventListener('submit', submitLogin);
el('logout').addEventListener('click', logout);
el('open-account').addEventListener('click', () => {
  el('account-error').textContent = '';
  showView('account');
});
el('account-back').addEventListener('click', () => showView('runs'));
el('pw-save').addEventListener('click', changeOwnPassword);
el('new-user').addEventListener('click', createUser);
el('pw-reset-save').addEventListener('click', () => submitPasswordReset(false));
el('pw-reset-generate').addEventListener('click', () => submitPasswordReset(true));
el('pw-reset-cancel').addEventListener('click', closePasswordReset);
el('pw-reset-value').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') submitPasswordReset(false);
  if (e.key === 'Escape') closePasswordReset();
});

el('config-export').addEventListener('click', (e) => exportConfig(e.currentTarget));
el('config-check').addEventListener('click', (e) => checkConfigArchive(e.currentTarget));
el('reset-run').addEventListener('click', (e) => runReset(e.currentTarget));
el('replica-sync').addEventListener('click', (e) =>
  replicaAction('/replica/sync', e.currentTarget, 'queueing…'));
el('replica-retry').addEventListener('click', (e) =>
  replicaAction('/replica/retry', e.currentTarget, 'retrying…'));
// A new file means a new plan: the previous one belonged to a different archive.
el('config-file').addEventListener('change', clearConfigPlan);

el('back').addEventListener('click', () => showView('runs'));
el('detail-stop').addEventListener('click', (e) => cancelRun(e.currentTarget.dataset.run));

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
  const stop = e.target.closest('[data-cancel]');
  if (stop) {
    e.stopPropagation(); // so the click does not open the run detail as well
    cancelRun(stop.dataset.cancel);
    return;
  }
  const copy = e.target.closest('[data-copy]');
  if (copy) {
    e.stopPropagation(); // so the click does not also open the template for editing
    await openInstanceForm(copy.dataset.copy, { copy: true });
    return;
  }
  const trigger = e.target.closest('[data-trigger]');
  if (trigger) {
    const id = trigger.dataset.trigger;
    trigger.disabled = true;
    trigger.textContent = 'queued…';
    markRowStale(trigger); // so the row is rebuilt from scratch on the next refresh
    try {
      await api(`/instances/${encodeURIComponent(id)}/run`, { method: 'POST' });
      // The job starts at the runner's next check-in; the Runs tab will show it.
      setTimeout(() => showView('runs'), 400);
    } catch (err) {
      showError(err);
      trigger.disabled = false;
      trigger.textContent = 'run now';
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
    if (reject && !confirm(`Reject runner "${id}"? The request will be discarded.`)) return;
    if (revoke && !confirm(
      `Revoke the certificate of runner "${id}"?\n\n`
      + 'Its certificate stops being valid everywhere immediately, including access to the '
      + 'backups. The runner moves to the "pending" state and asks for a new one itself — '
      + 'you then have to approve it again.')) return;
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
  // User management: a new password, a role change, disabling/enabling, deletion.
  const reset = e.target.closest('[data-reset]');
  if (reset) {
    openPasswordReset(reset.dataset.reset);
    return;
  }
  const roleBtn = e.target.closest('[data-role]');
  if (roleBtn) {
    const name = roleBtn.dataset.role;
    const to = roleBtn.dataset.to;
    if (!confirm(`Change the role of user "${name}" to "${to}"?`)) return;
    await updateUser(name, { role: to }, `The role of user <b>${esc(name)}</b> is now ${esc(to)}.`);
    return;
  }
  const disable = e.target.closest('[data-disable]');
  const enable = e.target.closest('[data-enable]');
  if (disable || enable) {
    const name = (disable || enable).dataset.disable || enable.dataset.enable;
    if (disable && !confirm(`Disable user "${name}"?\n\n`
      + 'They will not be able to sign in and their current sessions end. The account stays.')) return;
    await updateUser(name, { disabled: !!disable },
      `User <b>${esc(name)}</b> is ${disable ? 'disabled' : 'active'}.`);
    return;
  }
  const delUser = e.target.closest('[data-deluser]');
  if (delUser) {
    const name = delUser.dataset.deluser;
    if (!confirm(`Delete user "${name}"?`)) return;
    await deleteUser(name);
    return;
  }
  if (e.target.closest('#new-instance')) {
    await openInstanceForm(null);
    return;
  }
  if (e.target.closest('#form-back')) {
    showView('instances');
    return;
  }
  if (e.target.closest('#form-save')) {
    const btn = e.target.closest('#form-save');
    const payload = collectInstance();
    btn.disabled = true;
    try {
      if (editing) {
        await api(`/instances/${encodeURIComponent(editing)}`,
          { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
      } else {
        await api('/instances',
          { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) });
      }
      showView('instances');
    } catch (err) {
      el('form-error').textContent = err.message;
    }
    btn.disabled = false;
    return;
  }
  if (e.target.closest('#form-delete')) {
    if (!editing || !confirm(`Delete instance "${editing}"?\n\n`
      + 'The backups in the repository stay — only the configuration is deleted.')) return;
    try {
      await api(`/instances/${encodeURIComponent(editing)}`, { method: 'DELETE' });
      showView('instances');
    } catch (err) {
      el('form-error').textContent = err.message;
    }
    return;
  }
  // Confirming the import. The button only comes into being with the plan, so it is
  // waited for here.
  const applyConfig = e.target.closest('#config-apply');
  if (applyConfig) {
    await applyConfigArchive(applyConfig);
    return;
  }
  if (e.target.closest('#rekey')) {
    const btn = e.target.closest('#rekey');
    btn.disabled = true;
    btn.textContent = 're-encrypting…';
    try {
      const res = await api('/secrets/rekey', { method: 'POST' });
      alert(`Values re-encrypted: ${res.secrets}\nAlready current: ${res.skipped}`
        + (res.remaining ? `\nUnreadable: ${res.remaining} — is the old key missing from previous_keys?` : ''));
      await loadRotation();
    } catch (err) {
      showError(err);
      btn.disabled = false;
      btn.textContent = 're-encrypt everything with the current key';
    }
    return;
  }
  if (e.target.closest('#revoke-all')) {
    if (!confirm(
      'Revoke the certificates of ALL runners?\n\n'
      + 'Use this when the CA is suspected of being compromised. Every runner moves to the '
      + '"pending" state and backups stop until you approve them again.')) return;
    try {
      const res = await api('/runners/revoke-all', { method: 'POST' });
      alert(`Certificates revoked: ${res.revoked}`);
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
  if (row.dataset.instance) {
    await openInstanceForm(row.dataset.instance);
    return;
  }
  if (row.dataset.dir) {
    // Restore view: descend into the directory.
    restore.path = row.dataset.dir;
    await loadRestoreDir();
    return;
  }
  if (row.dataset.run) openRun(row.dataset.run);
});

// --- start ------------------------------------------------------------------

// Opening the page starts by asking for the identity: with a valid cookie it carries
// straight on, otherwise api() shows the login screen.
startSession().catch((err) => {
  if (err.status !== 401) showLogin(err.message);
});

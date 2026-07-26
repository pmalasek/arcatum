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
    <tr class="clickable" data-instance="${esc(i.id)}">
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

// --- instance form ----------------------------------------------------------

// Instances are edited here rather than in a JSON file on the server: the form is built
// from the parameters the script declares, so a missing password or a mistyped name is
// caught on save, and secrets are encrypted from the moment they are stored.
let scriptsCache = null;
let editing = null; // instance id being edited, null when creating

async function scripts() {
  if (!scriptsCache) scriptsCache = await api('/scripts');
  return scriptsCache;
}

function fieldRow(label, inner, hint) {
  return `<div class="field"><label>${esc(label)}</label><div>${inner}`
    + (hint ? `<div class="hint">${esc(hint)}</div>` : '') + '</div></div>';
}

async function openInstanceForm(id) {
  editing = id || null;
  el('form-error').textContent = '';
  el('form-title').textContent = id ? id : 'Nová instance';
  el('form-delete').classList.toggle('hidden', !id);
  showView('instance-form');

  const list = await scripts();
  let inst = null;
  if (id) {
    const all = await api('/instances');
    inst = all.find((i) => i.id === id) || null;
  }
  const chosen = inst ? inst.script : (list[0] && list[0].name) || '';

  el('form-body').innerHTML =
    fieldRow('id', id
      ? `<input id="f-id" value="${esc(id)}" disabled>`
      : '<input id="f-id" placeholder="mysql-web01">',
      id ? 'id nelze změnit' : 'krátký identifikátor, používá se i jako název repozitáře')
    + fieldRow('skript', `<select id="f-script">${list.map((sc) =>
        `<option value="${esc(sc.name)}"${sc.name === chosen ? ' selected' : ''}>${esc(sc.name)} (${esc(sc.type)})</option>`).join('')}</select>`)
    + fieldRow('runner', `<input id="f-runner" value="${esc(inst ? inst.runner_id : '')}" placeholder="web-01">`,
        'musí odpovídat CN certifikátu runneru')
    + fieldRow('rozvrh', `
        <select id="f-freq">
          ${['daily', 'weekly', 'monthly'].map((f) =>
            `<option value="${f}"${inst && inst.schedule.frequency === f ? ' selected' : ''}>${f}</option>`).join('')}
        </select>
        <input id="f-time" value="${esc(inst ? inst.schedule.time : '02:00')}" placeholder="02:00" size="6">
        <input id="f-weekdays" value="${esc(inst && inst.schedule.weekdays ? inst.schedule.weekdays.join(',') : '')}" placeholder="mon,thu" size="12">
        <input id="f-day" value="${inst && inst.schedule.day ? inst.schedule.day : ''}" placeholder="den 1-28" size="8">
        <input id="f-tz" value="${esc(inst ? inst.schedule.timezone || '' : '')}" placeholder="Europe/Prague" size="16">`,
        'weekdays platí pro weekly, den pro monthly')
    + fieldRow('timeout', `<input id="f-timeout" value="${esc(inst ? inst.timeout : '')}" placeholder="1h" size="8">`)
    + '<div id="f-params"></div>';

  el('f-script').addEventListener('change', () => renderParams(inst));
  renderParams(inst);
}

// renderParams builds the inputs for whatever the selected script declares.
async function renderParams(inst) {
  const list = await scripts();
  const name = el('f-script').value;
  const sc = list.find((s) => s.name === name);
  const box = el('f-params');
  if (!sc || sc.params.length === 0) {
    box.innerHTML = '<div class="field"><label>parametry</label><div class="hint">tento skript žádné nedeklaruje</div></div>';
    return;
  }
  const same = inst && inst.script === name;
  box.innerHTML = sc.params.map((p) => {
    // A stored secret comes back masked; leaving the field untouched keeps it.
    const value = same
      ? (p.secret ? (inst.secrets[p.name] || '') : (inst.params[p.name] || ''))
      : '';
    const hint = [
      p.type || 'string',
      p.required ? 'povinné' : 'nepovinné',
      p.secret ? 'secret' : '',
      p.default ? `default ${p.default}` : '',
    ].filter(Boolean).join(' · ');
    return fieldRow(p.name,
      `<input data-param="${esc(p.name)}" data-secret="${p.secret}" value="${esc(value)}"`
      + `${p.secret ? ' placeholder="(nezměněno)"' : ''}>`, hint);
  }).join('');
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
    script: el('f-script').value,
    runner_id: el('f-runner').value.trim(),
    timeout: el('f-timeout').value.trim(),
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
      <h3>Secrets — master klíč</h3>
      <dl class="meta">
        <dt>aktuální klíč</dt><dd>${st.secrets_key_id ? esc(st.secrets_key_id) : '<span class="muted">žádný (plaintext)</span>'}</dd>
        <dt>načtených klíčů</dt><dd>${st.secrets_keys}${secretsRotating ? ' <span class="expiry warn">rotace probíhá</span>' : ''}</dd>
        <dt>nepřešifrováno</dt><dd>${st.secrets_pending}${secretsDone ? '' : ' <span class="expiry warn">zbývá přešifrovat</span>'}</dd>
      </dl>
      ${st.secrets_key_id ? `<div class="section-foot">
        <button id="rekey" class="action">přešifrovat vším aktuálním klíčem</button>
        <span class="hint">bezpečné spustit opakovaně — už přešifrované hodnoty přeskočí</span>
      </div>` : ''}
    </div>

    <div class="rot">
      <h3>Podpis úloh</h3>
      <dl class="meta">
        <dt>důvěryhodných klíčů</dt><dd>${st.signing_keys}${st.signing_keys > 1
          ? ' <span class="expiry warn">rotace probíhá — až runnery přejdou, odeber starý z previous_keys</span>' : ''}</dd>
      </dl>
    </div>

    <div class="rot">
      <h3>Certifikační autorita</h3>
      <dl class="meta">
        <dt>vydává se pod</dt><dd>${st.signing_ca ? esc(st.signing_ca) : '<span class="muted">—</span>'}</dd>
        <dt>důvěryhodné CA</dt><dd>${cas.length === 0 ? '<span class="muted">—</span>' : cas.map((c) =>
          `${esc(c.common_name)}${c.is_signer ? ' <span class="expiry">(vydávající)</span>' : ''}`
          + ` <span class="days">· platí ${c.days_left} d</span>`).join('<br>')}</dd>
        ${caRotating || stale.length ? `<dt>runnery na staré CA</dt><dd>${
          stale.length === 0 ? '<span class="expiry">žádné</span>' : esc(stale.join(', '))}</dd>` : ''}
      </dl>
      ${caRotating ? `<div class="section-foot">
        ${st.safe_to_drop_old_ca
          ? '<span class="expiry">Všechny runnery jsou na aktuální CA — starou lze z bundle odebrat.</span>'
          : '<span class="expiry warn">Starou CA zatím neodebírej: některé runnery na ni ještě spoléhají.</span>'}
      </div>` : ''}
    </div>`;
}

const loaders = {
  runs: loadRuns,
  instances: loadInstances,
  restore: loadRestore,
  runners: loadRunners,
  rotation: loadRotation,
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
  for (const v of ['runs', 'instances', 'instance-form', 'restore', 'runners', 'rotation', 'detail']) {
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
    if (!editing || !confirm(`Smazat instanci "${editing}"?\n\n`
      + 'Zálohy v repozitáři zůstanou — maže se jen konfigurace.')) return;
    try {
      await api(`/instances/${encodeURIComponent(editing)}`, { method: 'DELETE' });
      showView('instances');
    } catch (err) {
      el('form-error').textContent = err.message;
    }
    return;
  }
  if (e.target.closest('#rekey')) {
    const btn = e.target.closest('#rekey');
    btn.disabled = true;
    btn.textContent = 'přešifrovávám…';
    try {
      const res = await api('/secrets/rekey', { method: 'POST' });
      alert(`Přešifrováno hodnot: ${res.secrets}\nJiž aktuálních: ${res.skipped}`
        + (res.remaining ? `\nNečitelných: ${res.remaining} — chybí starý klíč v previous_keys?` : ''));
      await loadRotation();
    } catch (err) {
      showError(err);
      btn.disabled = false;
      btn.textContent = 'přešifrovat vším aktuálním klíčem';
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

checkIdentity();
refresh();
listTimer = setInterval(refresh, LIST_REFRESH_MS);

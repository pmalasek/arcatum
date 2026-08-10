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
let currentView = 'dashboard';
let currentRun = null;
let tailOffset = 0;
let me = null; // identity from /whoami: who I am and what I may do

// Which task's history is open, how much of it is shown, and where ‹ back leads from a
// run detail — a run is reached from three places now and has to return to the one it
// came from.
let historyInstance = null;
let historyLimit = 50;
let detailReturn = 'dashboard';
// Which instance the Schedules tab is filtered to; empty means all of them.
let scheduleFilter = '';

// The dashboard's period view. How wide a window is on screen lives here rather than in
// the DOM: the lists above it are redrawn every five seconds, and a switch whose truth
// was a CSS class would flip back under the operator's hands twelve times a minute.
const STATS_REFRESH_MS = 60000; // a bucket only changes when a run finishes
let statsDays = readStatsDays(); // 7 or 30, remembered across reloads
let statsData = null;            // the last /stats payload, so a redraw costs no request
let statsFetchedAt = 0;
let statsLoading = false;
let topSort = 'bytes';           // 'bytes' | 'duration' — how the instance table is ordered

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

// fmtSpan formats a duration already measured in milliseconds — what the period view
// carries, where fmtDuration's pair of timestamps does not exist.
function fmtSpan(ms) {
  if (!ms || ms < 0) return '—';
  if (ms < 1000) return ms + ' ms';
  const s = Math.round(ms / 1000);
  if (s < 60) return s + ' s';
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min ${s % 60} s`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h} h ${m % 60} min`;
  return `${Math.floor(h / 24)} d ${h % 24} h`;
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

// --- dashboard ---------------------------------------------------------------

// The whole page comes from one request. Assembling it here out of four would cost four
// requests per open browser every five seconds, on a server whose runners are already
// checking in — so the server aggregates and this only draws.
async function loadDashboard() {
  const d = await api('/dashboard');
  const c = d.counts || {};
  el('dash-counts').innerHTML = [
    countTile('instances', c.instances),
    countTile('schedules', c.schedules, c.schedules_disabled ? `${c.schedules_disabled} paused` : ''),
    countTile('running', c.running),
    countTile('failed 24h', c.failed_24h, '', c.failed_24h ? 'fail' : ''),
    countTile('runners offline', c.runners_offline, '', c.runners_offline ? 'warn' : ''),
  ].join('');

  renderDashList('dash-running', d.running, 'nothing is running', (r) => `
    <span class="mono">${esc(r.instance_id)}</span>
    ${badge(r.status)}
    <span class="hint">${fmtDuration(r.started_at, '')}</span>`,
    (r) => ({ run: r.id }));

  renderDashList('dash-failures', d.failures_24h, 'nothing failed in the last 24 hours', (r) => `
    <span class="mono">${esc(r.instance_id)}</span>
    ${badge(r.status)}
    <span class="hint">${fmtTime(r.ended_at)}</span>`,
    (r) => ({ run: r.id }));

  renderDashList('dash-upcoming', d.upcoming, 'nothing is scheduled', (u) => `
    <span class="mono">${esc(u.instance_id)}</span>
    ${u.name ? `<span class="hint">${esc(u.name)}</span>` : ''}
    <span>${fmtTime(u.next_run)}</span>`,
    (u) => ({ history: u.instance_id }));

  // The replica card draws from the state the poll keeps up to date anyway; the first
  // paint after a reload is the only moment there is nothing to draw from yet.
  if (!replicaState) await refreshReplica();
  renderDashReplica();
  // The controls carry a remembered choice, so they are put right before the first
  // answer arrives rather than after it.
  syncStatsControls();
  // Not awaited: the period view is an aggregate over weeks and the lists above must
  // not wait for it.
  loadStats();
}

function countTile(label, value, note, tone) {
  return `<span class="count${tone ? ' ' + tone : ''}">
    <b>${value ?? 0}</b> ${esc(label)}${note ? ` <span class="hint">(${esc(note)})</span>` : ''}
  </span>`;
}

// renderDashList draws one card. Every row leads somewhere — to the run that is failing,
// or to the history of the task that is about to run — because a dashboard nobody can
// click through from is just a wall of numbers.
function renderDashList(id, items, emptyText, htmlOf, targetOf) {
  const box = el(id);
  if (!items || items.length === 0) {
    box.innerHTML = `<p class="empty">${esc(emptyText)}</p>`;
    return;
  }
  box.innerHTML = items.map((item) => {
    const target = targetOf(item);
    const attr = target.run
      ? `data-run="${esc(target.run)}"`
      : `data-history="${esc(target.history)}"`;
    return `<div class="dash-row clickable" ${attr}>${htmlOf(item)}</div>`;
  }).join('');
}

// --- dashboard: the period view ------------------------------------------------

// How the last N days went. A second endpoint and a second clock: the lists above are
// "what is happening", polled every few seconds, while this is an aggregate over weeks
// where a figure only moves when a run finishes. Fetching it with the fast poll would
// make every open browser pay for a thirty-day scan twelve times a minute.

function readStatsDays() {
  try {
    return localStorage.getItem('arcatum.statsDays') === '30' ? 30 : 7;
  } catch (_) {
    return 7; // private mode refuses storage; the default is not worth an error
  }
}

function writeStatsDays(days) {
  try { localStorage.setItem('arcatum.statsDays', String(days)); } catch (_) { /* see above */ }
}

// loadStats fetches the period unless a recent answer is already in hand. force is for
// the moment the operator changes the window, where waiting out the throttle would look
// like a broken button.
async function loadStats(force) {
  if (!force && statsData && Date.now() - statsFetchedAt < STATS_REFRESH_MS) return;
  if (statsLoading) return;
  statsLoading = true;
  // Dim the existing chart rather than clearing it: a skeleton in its place would move
  // everything below it up and back down again on every refresh.
  if (force) setChartLoading(true);
  const asked = statsDays;
  let ok = false;
  try {
    const data = await api(`/stats?days=${asked}`);
    statsData = data;
    statsFetchedAt = Date.now();
    ok = true;
    renderStats();
  } catch (err) {
    // The previous drawing stays on screen. It is old, not wrong, and the connection
    // banner already says the server is unreachable.
    if (err.status !== 401) showError(err);
  } finally {
    statsLoading = false;
    setChartLoading(false);
  }
  // The window was switched while the answer was on its way, so what came back describes
  // a period the operator has already left. Without this the click would be swallowed and
  // the right figures would not appear until the throttle expired a minute later.
  if (ok && asked !== statsDays) loadStats(true);
}

function setChartLoading(on) {
  for (const card of document.querySelectorAll('.chart-card')) {
    card.classList.toggle('chart--loading', on);
  }
}

// syncStatsControls makes the highlighted period and sort order match the state that is
// actually in force. Re-applied on every draw rather than only on a click, so however the
// page is redrawn the buttons resynchronise instead of drifting. It only toggles classes,
// which is why it is safe to call from the five-second poll: no markup is rewritten, so
// nothing flickers.
function syncStatsControls() {
  for (const btn of document.querySelectorAll('[data-period]')) {
    const on = Number(btn.dataset.period) === statsDays;
    btn.classList.toggle('active', on);
    btn.setAttribute('aria-pressed', String(on));
  }
  for (const btn of document.querySelectorAll('[data-sort]')) {
    btn.classList.toggle('active', btn.dataset.sort === topSort);
  }
}

// renderStats draws from what was last fetched and never fetches itself, so any redraw
// is free and cannot change what is on screen behind the operator's back.
function renderStats() {
  syncStatsControls();
  if (!statsData) return;

  const d = statsData;
  el('dash-period-note').textContent =
    `${fmtDay(d.daily[0] && d.daily[0].day)} – ${fmtDay(d.daily[d.daily.length - 1] && d.daily[d.daily.length - 1].day)}`
    + (d.timezone ? ` · ${d.timezone}` : '');

  renderStatTiles(d.period);
  renderRunsChart(d.daily, d.days);
  renderVolumeChart(d.daily, d.days);
  renderChartValues(d.daily);
  renderTopInstances(d.instances || []);
  renderStorageCard(d.storage || {});
}

// The figures above the chart. Every one carries what it counts underneath it: a number
// on a dashboard without its denominator is where wrong conclusions come from.
function renderStatTiles(p) {
  const rate = p.completed > 0 ? Math.round(p.success_rate * 1000) / 10 : null;
  const tone = rate === null ? '' : (rate >= 99 ? ' ok' : (rate < 90 ? ' fail' : ''));
  el('dash-tiles').innerHTML = [
    tile('Backups completed', p.completed,
      `${p.success} succeeded · ${p.failed} failed`
      + (p.cancelled ? ` · ${p.cancelled} stopped` : '')),
    tile('Success rate', rate === null ? '—' : `${rate} %`,
      'of the runs that reached a verdict', tone),
    tile('Data backed up', fmtBytes(p.data_bytes), 'produced by the successful runs'),
    tile('Total run time', fmtSpan(p.duration_ms),
      `measured over ${p.duration_runs} run${p.duration_runs === 1 ? '' : 's'}`),
    tile('Average run', fmtSpan(p.avg_duration_ms),
      p.never_started
        ? `${p.never_started} run${p.never_started === 1 ? '' : 's'} never started`
        : 'per measured run'),
  ].join('');
}

function tile(label, value, note, tone) {
  return `<div class="tile">
    <span class="label">${esc(label)}</span>
    <span class="value${tone || ''}">${esc(String(value))}</span>
    <span class="note">${esc(note)}</span>
  </div>`;
}

// fmtDay turns "2026-08-10" into something readable. Parsed field by field rather than
// handed to Date(): the ISO date form is read as UTC midnight, which lands on the day
// before in every timezone west of Greenwich.
function fmtDay(day, withWeekday) {
  if (!day) return '—';
  const [y, m, d] = day.split('-').map(Number);
  const date = new Date(y, m - 1, d);
  return date.toLocaleDateString('en-GB', withWeekday
    ? { weekday: 'short', day: 'numeric', month: 'short' }
    : { day: 'numeric', month: 'short' });
}

// --- the charts ---------------------------------------------------------------

// Two panels side by side — never one plot with two y axes. A count of runs and a volume
// in bytes are different scales, and drawing them on one frame invents a relationship
// between them that is not in the data.
//
// The viewBox is measured from the container instead of being a fixed 720, so one SVG
// unit is exactly one CSS pixel. With a fixed viewBox and width:100% the browser scales
// the whole drawing to fit — which scales the type with it, and on a wide monitor turns
// a 12px tick label into a 40px one. Sizing the box to the element is what keeps the
// text the size it says it is.
const PAD = { t: 12, b: 24, r: 12 };
const PAD_COUNT = 34; // room for a two-digit count
const PAD_BYTES = 62; // room for "21.2 GiB"
const CHART_H = 168;
const CHART_MIN_W = 300; // below this the panel scrolls rather than compresses

// chartWidth measures the box the panel will be drawn into. A hidden view reports zero,
// in which case a sensible default is drawn and the resize handler redraws it correctly
// as soon as it is on screen.
function chartWidth(box) {
  const style = getComputedStyle(box);
  const inner = box.clientWidth
    - parseFloat(style.paddingLeft || 0) - parseFloat(style.paddingRight || 0);
  return Math.max(CHART_MIN_W, Math.round(inner) || 640);
}

// niceMax rounds a maximum up to a value whose half is still a whole number, so the
// middle gridline can be labelled without a decimal appearing in a count of runs.
function niceMax(v) {
  if (v <= 2) return 2;
  const mag = Math.pow(10, Math.floor(Math.log10(v)));
  for (const step of [2, 4, 6, 8, 10]) {
    if (v <= step * mag) return step * mag;
  }
  return 10 * mag;
}

// barPath draws a column with rounded corners at the data end only. The baseline end
// stays square: a rounded foot would lift the mark off the axis it is measured from.
function barPath(x, y, w, h, r) {
  if (h <= 0) return '';
  if (h <= r || w <= 2 * r) return `M${x},${y}h${w}v${h}h${-w}z`;
  return `M${x},${y + r}a${r},${r} 0 0 1 ${r},${-r}h${w - 2 * r}a${r},${r} 0 0 1 ${r},${r}v${h - r}h${-w}z`;
}

// bandGeometry lays out one panel for a measured width. The bar is capped at 24px and the
// rest of its band is left as air, rather than the column growing to fill the slot: a
// wide screen should give a chart more room to breathe, not fatter marks.
function bandGeometry(n, width, height, padL) {
  const plotW = width - padL - PAD.r;
  const plotH = height - PAD.t - PAD.b;
  const band = plotW / n;
  return { padL, plotW, plotH, band, width: Math.min(24, Math.max(3, band - 6)) };
}

// Which days get a label underneath. Derived from the band the panel actually has rather
// than from the number of days: the same thirty days are comfortable on a wide screen and
// a grey smear on half of one. A date needs roughly 52px, so that is the spacing asked
// for; the days that miss out are still in the tooltip and in the values table.
function labelEvery(band) {
  return Math.max(1, Math.ceil(52 / band));
}

function renderRunsChart(daily, days) {
  const box = el('chart-runs');
  const legend = el('chart-legend');
  // Counted over what is actually drawn. A stopped run has no column — it is neither a
  // backup nor a fault — so a period holding nothing else still has an empty chart, and
  // the axes of an empty chart are furniture pretending to be data.
  const total = daily.reduce((sum, d) => sum + d.success + d.failed, 0);
  if (total === 0) {
    box.innerHTML = '<p class="empty">no runs finished in this period</p>';
    legend.innerHTML = '';
    return;
  }
  legend.innerHTML = '<span><i class="key ok"></i>successful</span>'
    + '<span><i class="key fail"></i>failed</span>';

  const width = chartWidth(box);
  const height = CHART_H;
  const n = daily.length;
  const g = bandGeometry(n, width, height, PAD_COUNT);
  const max = niceMax(Math.max(1, ...daily.map((d) => d.success + d.failed)));
  const y = (v) => PAD.t + g.plotH * (1 - v / max);
  const every = labelEvery(g.band);

  let svg = '';
  for (const v of [0, max / 2, max]) {
    svg += `<line class="grid-line" x1="${g.padL}" y1="${y(v)}" x2="${width - PAD.r}" y2="${y(v)}"/>`
      + `<text class="axis-text" x="${g.padL - 6}" y="${y(v) + 4}" text-anchor="end">${v}</text>`;
  }

  daily.forEach((d, i) => {
    const centre = g.padL + g.band * i + g.band / 2;
    const x = centre - g.width / 2;
    const base = y(0);
    // A day on which nothing ran draws nothing. The gap in the row is the honest
    // rendering of "no backups", where a zero-height rect would be a mark for no data.
    if (d.success + d.failed > 0) {
      const okH = g.plotH * (d.success / max);
      const failH = g.plotH * (d.failed / max);
      // The 2px separation between the two segments is a gap in the surface colour, not
      // a stroke around the marks: a border would add ink that is not data.
      const gap = d.success > 0 && d.failed > 0 ? 2 : 0;
      if (d.failed > 0) {
        svg += `<path class="bar-fail" d="${barPath(x, base - okH - failH, g.width, failH, 4)}"/>`;
      }
      if (d.success > 0) {
        const h = Math.max(1, okH - gap);
        svg += `<path class="bar-ok" d="${barPath(x, base - h, g.width, h, d.failed > 0 ? 0 : 4)}"/>`;
      }
    }
    if (i % every === 0) {
      svg += `<text class="axis-text" x="${centre}" y="${height - 7}" text-anchor="middle">${esc(fmtDay(d.day))}</text>`;
    }
    // A transparent hit area across the whole band, wider than the bar, so the reading
    // is available to a mouse and — through tabindex — to a keyboard as well.
    svg += `<rect class="hit" x="${g.padL + g.band * i}" y="${PAD.t}" width="${g.band}" height="${g.plotH}"
      tabindex="0" role="img"><title>${esc(dayTitle(d))}</title></rect>`;
  });

  box.innerHTML = chartSvg(width, height, svg,
    `Runs per day over the last ${days} days`,
    `${total} runs in total, of which ${daily.reduce((s, d) => s + d.failed, 0)} failed.`);
}

function renderVolumeChart(daily, days) {
  const box = el('chart-volume');
  const max = Math.max(...daily.map((d) => d.data_bytes), 0);
  if (max === 0) {
    box.innerHTML = '<p class="empty">nothing was backed up in this period</p>';
    return;
  }
  const width = chartWidth(box);
  const height = CHART_H;
  const n = daily.length;
  const g = bandGeometry(n, width, height, PAD_BYTES);
  const y = (v) => PAD.t + g.plotH * (1 - v / max);
  const every = labelEvery(g.band);

  // Bars rather than a line: a line between Monday and Tuesday would imply a value at
  // half past Monday, and there is no such thing as half of a night's backup.
  let svg = '';
  for (const v of [0, max]) {
    svg += `<line class="grid-line" x1="${g.padL}" y1="${y(v)}" x2="${width - PAD.r}" y2="${y(v)}"/>`
      + `<text class="axis-text" x="${g.padL - 6}" y="${y(v) + 4}" text-anchor="end">${esc(v === 0 ? '0' : fmtBytes(v))}</text>`;
  }
  daily.forEach((d, i) => {
    const centre = g.padL + g.band * i + g.band / 2;
    if (d.data_bytes > 0) {
      const h = Math.max(1, g.plotH * (d.data_bytes / max));
      svg += `<path class="bar-vol" d="${barPath(centre - g.width / 2, y(0) - h, g.width, h, 4)}"/>`;
    }
    if (i % every === 0) {
      svg += `<text class="axis-text" x="${centre}" y="${height - 7}" text-anchor="middle">${esc(fmtDay(d.day))}</text>`;
    }
    svg += `<rect class="hit" x="${g.padL + g.band * i}" y="${PAD.t}" width="${g.band}" height="${g.plotH}"
      tabindex="0" role="img"><title>${esc(fmtDay(d.day, true))} — ${esc(fmtBytes(d.data_bytes))}</title></rect>`;
  });

  box.innerHTML = chartSvg(width, height, svg,
    `Data backed up per day over the last ${days} days`,
    `The busiest day produced ${fmtBytes(max)}.`);
}

function chartSvg(width, height, body, title, desc) {
  return `<svg viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img">
    <title>${esc(title)}</title><desc>${esc(desc)}</desc>${body}</svg>`;
}

function dayTitle(d) {
  const parts = [`${d.success} successful`, `${d.failed} failed`];
  if (d.cancelled) parts.push(`${d.cancelled} stopped`);
  if (d.data_bytes) parts.push(fmtBytes(d.data_bytes));
  return `${fmtDay(d.day, true)} — ${parts.join(' · ')}`;
}

// The same numbers as a table, collapsed under the chart. A tooltip may enhance a chart
// but must never be the only way to a value — not least because a phone has no hover.
function renderChartValues(daily) {
  el('chart-values').innerHTML = `<table>
    <thead><tr><th>Day</th><th>Successful</th><th>Failed</th><th>Data</th><th>Time</th></tr></thead>
    <tbody>${daily.map((d) => `<tr>
      <td>${esc(fmtDay(d.day, true))}</td>
      <td class="num">${d.success}</td>
      <td class="num">${d.failed}</td>
      <td class="num">${esc(d.data_bytes ? fmtBytes(d.data_bytes) : '—')}</td>
      <td class="num">${esc(d.duration_ms ? fmtSpan(d.duration_ms) : '—')}</td>
    </tr>`).join('')}</tbody></table>`;
}

// --- the instance table and the two cards under the chart ----------------------

function renderTopInstances(list) {
  const body = el('top-body');
  if (list.length === 0) {
    body.innerHTML = '<tr><td colspan="6" class="empty">no task finished a run in this period</td></tr>';
    return;
  }
  // Sorted here rather than on the server: the whole window is already in hand, so
  // changing the order is a redraw instead of a request.
  const rows = [...list].sort((a, b) => (topSort === 'duration'
    ? b.duration_ms - a.duration_ms
    : b.data_bytes - a.data_bytes));

  syncRows(body, rows, (r) => r.instance_id, (r) => `
    <td class="mono" data-label="Instance">${esc(r.instance_id)}</td>
    <td class="num" data-label="Data">${esc(fmtBytes(r.data_bytes))}</td>
    <td class="num" data-label="Runs">${r.success}/${r.runs}</td>
    <td class="num" data-label="Total time">${esc(fmtSpan(r.duration_ms))}</td>
    <td class="num" data-label="Average">${esc(fmtSpan(r.avg_duration_ms))}</td>
    <td data-label="Last result">${badge(r.last_status)} <span class="hint">${esc(fmtTime(r.last_ended_at))}</span></td>`,
    (tr, r) => {
      tr.className = 'clickable';
      tr.dataset.history = r.instance_id; // a row leads to the task's own history
    });
}

// What the server is holding. Three figures rather than one total, because they answer
// different questions and adding them up would answer none of them.
function renderStorageCard(st) {
  // An unmeasured repository says so. Reporting it as zero bytes would be a claim the
  // server has not yet had the chance to make.
  const repo = hasTime(st.measured_at)
    ? `${fmtBytes(st.repo_bytes)} <span class="hint">in ${st.repositories} repositor${st.repositories === 1 ? 'y' : 'ies'}`
      + `${st.stale ? ', refreshing' : ''}</span>`
    : '<span class="hint">measuring…</span>';
  el('dash-storage').innerHTML = `<dl class="meta">
    <dt>dumps</dt><dd>${esc(fmtBytes(st.dump_bytes))}</dd>
    <dt>logs</dt><dd>${esc(fmtBytes(st.log_bytes))}</dd>
    <dt>repositories</dt><dd>${repo}</dd>
  </dl>
  <p class="card-note">Dumps are the files the Restore tab hands back; repositories are
    measured on disk, after restic has deduplicated them.</p>`;
}

// The replica card reads the state the fast poll already keeps up to date (see
// refreshReplica), so the dashboard costs no request of its own for it.
function renderDashReplica() {
  const box = el('dash-replica');
  const st = replicaState;
  if (!st) { box.innerHTML = '<p class="empty">loading…</p>'; return; }
  if (!st.enabled) {
    box.innerHTML = '<p class="empty">Not configured — everything the server holds sits '
      + 'in one place only. See <code>[replica]</code> in <code>server.toml</code>.</p>';
    return;
  }
  const health = st.healthy
    ? '<span class="badge success">reachable</span>'
    : '<span class="badge failed">unreachable</span>';
  box.innerHTML = `<dl class="meta">
    <dt>target</dt><dd class="mono">${esc(st.target)}</dd>
    <dt>link</dt><dd>${health}</dd>
    <dt>queue</dt><dd>${st.counts.pending} waiting · ${st.counts.syncing} sending
      · ${st.counts.failed} failing</dd>
    <dt>last success</dt><dd>${esc(fmtTime(st.link.last_ok_at))}</dd>
  </dl>`;
}

// --- schedules ----------------------------------------------------------------

// whenText renders a schedule the way an operator reads it aloud.
function whenText(sc) {
  let out;
  if (sc.frequency === 'weekly') {
    out = `${(sc.weekdays && sc.weekdays.length) ? sc.weekdays.join(',') : 'every day'} ${sc.time}`;
  } else if (sc.frequency === 'monthly') {
    out = `day ${sc.day} · ${sc.time}`;
  } else {
    out = `daily ${sc.time}`;
  }
  return out + (sc.timezone ? ` (${sc.timezone})` : '');
}

async function loadSchedules() {
  const path = scheduleFilter
    ? `/instances/${encodeURIComponent(scheduleFilter)}/schedules`
    : '/schedules';
  const list = (await api(path)) || [];
  el('schedules-head').classList.toggle('hidden', !scheduleFilter);
  el('schedules-filter-name').textContent = scheduleFilter || '';

  const body = el('schedules-body');
  if (list.length === 0) {
    body.innerHTML = `<tr><td colspan="7" class="empty">${
      scheduleFilter ? 'this task has no schedule — it runs only when you start it'
        : 'no schedules yet'}</td></tr>`;
    return;
  }
  syncRows(body, list, (sc) => sc.id, (sc) => {
    const last = sc.last_run;
    return `
      <td class="mono" data-label="Instance">${esc(sc.instance_id)}</td>
      <td data-label="Schedule">${esc(sc.name || '—')}</td>
      <td data-label="When">${esc(whenText(sc))}</td>
      <td data-label="State">${badge(sc.state)}</td>
      <td data-label="Last run">${last
        ? `<a class="rowlink" data-open="${esc(last.id)}">${fmtTime(last.ended_at || last.started_at)}</a>`
        : '<span class="muted">—</span>'}</td>
      <td data-label="Next run">${sc.enabled ? fmtTime(sc.next_run) : '<span class="muted">paused</span>'}</td>
      <td class="actions" data-label="">
        <button class="action admin-only" data-sched-toggle="${esc(sc.id)}" data-enabled="${sc.enabled}">
          ${sc.enabled ? 'pause' : 'resume'}</button>
        <button class="action" data-history="${esc(sc.instance_id)}">history</button>
      </td>`;
  }, (tr, sc) => {
    tr.className = 'clickable';
    tr.dataset.schedule = sc.id;
  });
}

// openScheduleForm opens the form over schedule id, or an empty one for instanceID.
async function openScheduleForm(id, instanceID) {
  editingSchedule = id || null;
  el('sform-error').textContent = '';
  el('sform-title').textContent = id ? `Schedule ${id}` : 'New schedule';
  el('sform-delete').classList.toggle('hidden', !id);
  showView('schedule-form');

  let sc = null;
  if (id) {
    sc = await api('/schedules/' + encodeURIComponent(id));
  }
  const instances = (await api('/instances')) || [];
  const owner = sc ? sc.instance_id : (instanceID || (instances[0] && instances[0].id) || '');
  const freq = sc ? sc.frequency : 'daily';

  el('sform-body').innerHTML =
    fieldRow('instance', sc
      ? `<input id="s-instance" value="${esc(owner)}" disabled>`
      : `<select id="s-instance">${instances.map((i) =>
          `<option value="${esc(i.id)}"${i.id === owner ? ' selected' : ''}>${esc(i.id)}</option>`).join('')}</select>`,
      sc ? 'a schedule stays with the task it was created for — delete it and make a new one to move it'
        : 'which task this schedule runs')
    + fieldRow('name', `<input id="s-name" value="${esc(sc ? sc.name : '')}" placeholder="nightly" size="20">`,
      'a label, so several schedules of one task can be told apart')
    + fieldRow('frequency', `<select id="s-freq">${['daily', 'weekly', 'monthly'].map((f) =>
        `<option value="${f}"${f === freq ? ' selected' : ''}>${f}</option>`).join('')}</select>`)
    + fieldRow('time', `<input id="s-time" value="${esc(sc ? sc.time : '02:00')}" placeholder="02:00" size="6">`,
      'HH:MM')
    + fieldRow('weekdays', `<input id="s-weekdays" value="${esc(sc && sc.weekdays ? sc.weekdays.join(',') : '')}" placeholder="mon,thu" size="16">`,
      'weekly only; empty means every day')
    + fieldRow('day of month', `<input id="s-day" type="number" min="1" max="28" value="${sc && sc.day ? sc.day : ''}" size="4">`,
      'monthly only; 1–28, so no month is ever skipped')
    + fieldRow('timezone', `<input id="s-tz" value="${esc(sc ? sc.timezone || '' : '')}" placeholder="Europe/Prague" size="20">`,
      'empty uses the server default')
    + fieldRow('enabled', `<label class="follow"><input type="checkbox" id="s-enabled"${
        !sc || sc.enabled ? ' checked' : ''}> run on this schedule</label>`,
      'unchecked pauses it without losing the settings');
}

function collectSchedule() {
  const weekdays = el('s-weekdays').value.split(',').map((s) => s.trim()).filter(Boolean);
  const day = parseInt(el('s-day').value, 10);
  return {
    instance_id: el('s-instance').value.trim(),
    name: el('s-name').value.trim(),
    frequency: el('s-freq').value,
    time: el('s-time').value.trim(),
    weekdays: weekdays.length ? weekdays : null,
    day: Number.isFinite(day) ? day : 0,
    timezone: el('s-tz').value.trim(),
    enabled: el('s-enabled').checked,
  };
}

// --- run history of one task ---------------------------------------------------

// openHistory shows one task's runs. This is where a run is found now: beside the task it
// belongs to, rather than in one flat list of everything the server has ever done.
function openHistory(instanceID) {
  historyInstance = instanceID;
  historyLimit = 50;
  el('history-title').textContent = instanceID;
  el('history-body').innerHTML = '<tr><td colspan="8" class="empty">loading…</td></tr>';
  showView('history');
}

async function loadHistory() {
  if (!historyInstance) return;
  const res = await api(`/instances/${encodeURIComponent(historyInstance)}/runs?limit=${historyLimit}`);
  const runs = res.runs || [];
  const body = el('history-body');
  if (runs.length === 0) {
    body.innerHTML = '<tr><td colspan="8" class="empty">this task has not run yet</td></tr>';
    el('history-more').classList.add('hidden');
    el('history-note').textContent = '';
    return;
  }
  syncRows(body, runs, (r) => r.id, (r) => `
    <td class="mono" data-label="Run">${esc(r.id)}</td>
    <td data-label="Status">${badge(r.status)}</td>
    <td data-label="Offsite">${replicaBadge(r.replica_status)}</td>
    <td class="num" data-label="Code">${r.exit_code}</td>
    <td class="num" data-label="Data">${fmtBytes(r.data_bytes || r.bytes)}</td>
    <td data-label="Started">${fmtTime(r.started_at)}</td>
    <td class="num" data-label="Duration">${fmtDuration(r.started_at, r.ended_at)}</td>
    <td data-label="">
      <button class="action" data-open="${esc(r.id)}">output</button>
      ${stopButton(r)}
    </td>`, (tr, r) => {
    tr.className = 'clickable';
    tr.dataset.run = r.id;
  });
  el('history-more').classList.toggle('hidden', !res.has_more);
  el('history-note').textContent = res.has_more
    ? `showing the latest ${runs.length} runs` : `${runs.length} run${runs.length === 1 ? '' : 's'} in total`;
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
    body.innerHTML = '<tr><td colspan="7" class="empty">no instances</td></tr>';
    return;
  }
  syncRows(body, instanceList, (i) => i.id, (i) => {
    const repo = repoCache.get(i.id);
    return `
      <td class="mono" data-label="Instance">${esc(i.id)}</td>
      <td data-label="Script">${esc(i.script)}</td>
      <td data-label="Runner">${esc(i.runner_id)}</td>
      <td data-label="Schedules">${scheduleCount(i)}</td>
      <td data-label="Next run">${fmtTime(i.next_run)}</td>
      <td class="mono repo" data-label="Repository">${esc(repo ? repo.text : '…')}</td>
      <td class="actions" data-label="">
        <button class="action" data-trigger="${esc(i.id)}">run now</button>
        <button class="action admin-only" data-copy="${esc(i.id)}">copy</button>
      </td>`;
  }, (tr, i) => {
    tr.className = 'clickable';
    tr.dataset.instance = i.id;
  });
}

// scheduleCount says how many schedules a task has. "none" rather than "0" because it is
// a legitimate state — such a task simply runs when somebody starts it — and a zero in a
// column of dates reads like something is broken.
function scheduleCount(inst) {
  const n = inst.schedules || 0;
  if (n === 0) return '<span class="muted">on demand</span>';
  const paused = n - (inst.schedules_enabled || 0);
  return `${n}${paused ? ` <span class="muted">(${paused} paused)</span>` : ''}`;
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
    body.innerHTML = '<tr><td colspan="7" class="empty">no runners</td></tr>';
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
      <td class="mono" data-label="Runner" title="${esc(r.cert_fingerprint || '')}">${esc(r.id)}</td>
      <td data-label="Status">${badge(r.status)}</td>
      <td data-label="Platform">${esc(r.os)}/${esc(r.arch)}</td>
      <td class="mono" data-label="Version">${ident}</td>
      <td data-label="Certificate valid until">${fmtExpiry(r.cert_not_after)}</td>
      <td data-label="Last seen">${fmtTime(r.last_seen)}</td>
      <td data-label="">${actions}</td>
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
  // The dashboard is where a session starts: what is running, what broke overnight and what
  // is coming are the three questions somebody opens Arcatum with.
  showView('dashboard');
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
      <td class="mono" data-label="User">${esc(u.username)}${self ? ' <span class="muted">(that is you)</span>' : ''}</td>
      <td data-label="Role">${badge(u.role)}</td>
      <td data-label="Status">${u.disabled ? '<span class="expiry alert">disabled</span>' : '<span class="expiry">active</span>'}</td>
      <td data-label="Created">${fmtTime(u.created_at)}</td>
      <td data-label="Last login">${fmtTime(u.last_login)}</td>
      <td data-label="">
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
      <td class="mono" data-label="Name">${esc(d.run_id)}</td>
      <td data-label="Type">dump</td>
      <td class="num" data-label="Size">${fmtBytes(d.bytes)}</td>
      <td data-label="Modified">${fmtTime(d.ended_at)}</td>
      <td data-label=""><a class="action" href="${API}/runs/${encodeURIComponent(d.run_id)}/data" download>download</a></td>
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
      <td class="mono" data-label="Name">.</td><td data-label="Type">directory</td><td></td><td></td>
      <td data-label=""><a class="action" href="${esc(dl(restore.path, true))}">download as .tar</a></td>
    </tr>`);
  }
  for (const e of res.entries) {
    const isDir = e.type === 'dir';
    rows.push(`<tr${isDir ? ' class="clickable" data-dir="' + esc(e.path) + '"' : ''}>
      <td class="mono" data-label="Name">${isDir ? '📁 ' : ''}${esc(e.name)}</td>
      <td data-label="Type">${isDir ? 'directory' : esc(e.type)}</td>
      <td class="num" data-label="Size">${isDir ? '' : fmtBytes(e.size)}</td>
      <td data-label="Modified">${isDir ? '' : fmtTime(e.mtime)}</td>
      <td data-label=""><a class="action" href="${esc(dl(e.path, isDir))}">${isDir ? 'download .tar' : 'download'}</a></td>
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
let editing = null;         // instance id being edited, null when creating
let copyingFrom = null;     // instance the form was seeded from, null when not copying
let editingSchedule = null; // schedule id being edited, null when creating one

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
    + scheduleRow(editing, inst)
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

// scheduleRow points at the Schedules tab instead of editing timing here. An instance
// says what to back up; when it runs is a separate decision, and one task may carry
// several answers to it.
function scheduleRow(id, inst) {
  if (!id) {
    return fieldRow('schedules', '<span class="hint">added under Schedules once the instance is saved</span>',
      'until then the task can still be run by hand');
  }
  const n = inst ? (inst.schedules || 0) : 0;
  const paused = inst ? n - (inst.schedules_enabled || 0) : 0;
  const summary = n === 0
    ? 'none — this task runs only when you start it'
    : `${n} schedule${n === 1 ? '' : 's'}${paused ? `, ${paused} paused` : ''}`;
  return fieldRow('schedules',
    `<span>${esc(summary)}</span> <button class="action" data-schedules="${esc(id)}">manage</button>`,
    'when this task runs is set under Schedules');
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

// The forms, the account page and the run detail are deliberately absent: refresh() would
// overwrite a half-filled form under the operator's hands.
const loaders = {
  dashboard: loadDashboard,
  instances: loadInstances,
  schedules: loadSchedules,
  history: loadHistory,
  restore: loadRestore,
  runners: loadRunners,
  rotation: loadRotation,
  users: loadUsers,
  admin: loadAdmin,
};

const VIEWS = ['dashboard', 'instances', 'instance-form', 'schedules', 'schedule-form',
  'history', 'restore', 'runners', 'rotation', 'users', 'admin', 'account', 'detail'];

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

// openRun shows one run's metadata and live output. `from` is the view ‹ back returns to,
// so a run opened from the dashboard does not dump the operator into a task's history.
async function openRun(runID, from) {
  stopTail();
  detailReturn = from || currentView;
  if (detailReturn === 'detail') detailReturn = 'dashboard';
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

// triggerInstance asks for a run now and then opens that task's history, which is where
// the run will appear. btn is the button that was pressed, when there was one.
async function triggerInstance(id, btn) {
  if (!id) return;
  if (btn) {
    btn.disabled = true;
    btn.textContent = 'queued…';
    markRowStale(btn); // so the row is rebuilt from scratch on the next refresh
  }
  try {
    await api(`/instances/${encodeURIComponent(id)}/run`, { method: 'POST' });
    // The job starts at the runner's next check-in, and the task's history is where it
    // shows up.
    setTimeout(() => openHistory(id), 400);
  } catch (err) {
    showError(err);
    if (btn) {
      btn.disabled = false;
      btn.textContent = 'run now';
    }
  }
}

// --- events -----------------------------------------------------------------

document.querySelector('nav').addEventListener('click', (e) => {
  const tab = e.target.closest('.tab');
  if (!tab) return;
  // Reaching a tab from the navigation means "show me all of it": a filter left over from
  // a drill-down into one task would otherwise look like an empty system.
  if (tab.dataset.view === 'schedules') scheduleFilter = '';
  setNav(false); // on a narrow screen the drawer has served its purpose
  showView(tab.dataset.view);
});

// The sidebar becomes a drawer once the window is too narrow for a column beside the
// content. CSS carries the layout; this only says whether it is open, and keeps the
// button's aria-expanded telling the truth about it.
function setNav(open) {
  document.body.classList.toggle('nav-open', open);
  el('nav-toggle').setAttribute('aria-expanded', String(open));
}

el('nav-toggle').addEventListener('click', () =>
  setNav(!document.body.classList.contains('nav-open')));
el('nav-scrim').addEventListener('click', () => setNav(false));
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') setNav(false);
});

// The charts are drawn to the pixel width they were measured at, so a resized window
// means they have to be drawn again. Debounced, because a drag fires this continuously,
// and free of any request: it redraws from the data already in hand.
let resizeTimer = null;
window.addEventListener('resize', () => {
  if (resizeTimer) clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => {
    resizeTimer = null;
    if (currentView === 'dashboard' && statsData) renderStats();
  }, 150);
});

el('login-form').addEventListener('submit', submitLogin);
el('logout').addEventListener('click', logout);
el('open-account').addEventListener('click', () => {
  el('account-error').textContent = '';
  showView('account');
});
el('account-back').addEventListener('click', () => showView('dashboard'));
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

el('back').addEventListener('click', () => showView(detailReturn));
el('detail-stop').addEventListener('click', (e) => cancelRun(e.currentTarget.dataset.run));

el('history-back').addEventListener('click', () => showView('instances'));
el('history-schedules').addEventListener('click', () => {
  scheduleFilter = historyInstance;
  showView('schedules');
});
el('history-run-now').addEventListener('click', () => triggerInstance(historyInstance));
el('history-more').addEventListener('click', () => {
  historyLimit += 50;
  refresh();
});
el('schedules-all').addEventListener('click', () => {
  scheduleFilter = '';
  refresh();
});

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
  // The dashboard's own controls come first, and each returns: placed after the
  // row-navigation fallbacks further down they would either never fire or take the
  // operator to a run they did not ask for.
  const period = e.target.closest('[data-period]');
  if (period) {
    const days = Number(period.dataset.period);
    if (days && days !== statsDays) {
      statsDays = days;
      writeStatsDays(days);
      syncStatsControls(); // the button answers the click at once, not when the data lands
      loadStats(true);
    }
    return;
  }
  const sort = e.target.closest('[data-sort]');
  if (sort) {
    topSort = sort.dataset.sort;
    renderStats();
    return;
  }

  const open = e.target.closest('[data-open]');
  if (open) {
    e.stopPropagation();
    openRun(open.dataset.open);
    return;
  }
  const history = e.target.closest('[data-history]');
  if (history) {
    e.stopPropagation();
    openHistory(history.dataset.history);
    return;
  }
  const manage = e.target.closest('[data-schedules]');
  if (manage) {
    e.stopPropagation();
    scheduleFilter = manage.dataset.schedules;
    showView('schedules');
    return;
  }
  // Pausing a schedule is a one-field update: the timing stays exactly as it is, which is
  // the difference between pausing and deleting-and-retyping.
  const toggle = e.target.closest('[data-sched-toggle]');
  if (toggle) {
    e.stopPropagation();
    const id = toggle.dataset.schedToggle;
    const enabled = toggle.dataset.enabled !== 'true';
    toggle.disabled = true;
    markRowStale(toggle);
    try {
      await apiSend('/schedules/' + encodeURIComponent(id), 'PUT', { enabled });
      await loadSchedules();
    } catch (err) {
      showError(err);
      toggle.disabled = false;
    }
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
    e.stopPropagation(); // so the click does not also open the instance for editing
    await triggerInstance(trigger.dataset.trigger, trigger);
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
  if (e.target.closest('#new-schedule')) {
    await openScheduleForm(null, scheduleFilter);
    return;
  }
  if (e.target.closest('#sform-back')) {
    showView('schedules');
    return;
  }
  if (e.target.closest('#sform-save')) {
    const btn = e.target.closest('#sform-save');
    const payload = collectSchedule();
    btn.disabled = true;
    try {
      if (editingSchedule) {
        await apiSend('/schedules/' + encodeURIComponent(editingSchedule), 'PUT', payload);
      } else {
        await apiSend('/schedules', 'POST', payload);
      }
      showView('schedules');
    } catch (err) {
      el('sform-error').textContent = err.message;
    }
    btn.disabled = false;
    return;
  }
  if (e.target.closest('#sform-delete')) {
    if (!editingSchedule || !confirm('Delete this schedule?\n\n'
      + 'The task and its backups stay — it just stops running on its own.')) return;
    try {
      await api('/schedules/' + encodeURIComponent(editingSchedule), { method: 'DELETE' });
      showView('schedules');
    } catch (err) {
      el('sform-error').textContent = err.message;
    }
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

  // The dashboard's cards are divs, not table rows, but they navigate the same way.
  const dashRow = e.target.closest('.dash-row');
  if (dashRow) {
    if (dashRow.dataset.run) openRun(dashRow.dataset.run, 'dashboard');
    else if (dashRow.dataset.history) openHistory(dashRow.dataset.history);
    return;
  }

  const row = e.target.closest('tr.clickable');
  if (!row) return;
  if (row.dataset.instance) {
    await openInstanceForm(row.dataset.instance);
    return;
  }
  if (row.dataset.schedule) {
    await openScheduleForm(row.dataset.schedule);
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

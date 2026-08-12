package vitals

// De pagina: één zelfstandig HTML-document, zelfde donkere huisstijl als
// welcome. Alles Engels, geen externe assets, en de hele toestand komt uit
// /api/state — de pagina is alleen een bril op die JSON, zodat een script of
// CI met dezelfde API hetzelfde rapport kan trekken.

const page = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HopOS vitals</title>
<meta name="robots" content="noindex">
<link rel="icon" href="data:image/svg+xml,<svg xmlns=%22http://www.w3.org/2000/svg%22 viewBox=%220 0 100 100%22><text y=%22.9em%22 font-size=%2290%22>&#x1FA7A;</text></svg>">
<style>
:root{--bg:#0b0f14;--panel:#10161d;--line:#1d2733;--text:#cdd6e0;--muted:#7f8b9b;--copper:#e09a63;--leaf:#56c88b;--phosphor:#46d979;--fault:#e0654f;--mono:ui-monospace,"SF Mono",Menlo,Consolas,monospace;--sans:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
*{box-sizing:border-box}
body{margin:0;padding-bottom:60px;background:var(--bg);color:var(--text);font:15.5px/1.55 var(--sans)}
.wrap{max-width:1100px;margin:0 auto;padding:0 20px}
.bar{position:sticky;top:0;z-index:5;background:rgba(11,15,20,.9);backdrop-filter:blur(8px);border-bottom:1px solid var(--line)}
.bar-in{display:flex;align-items:center;gap:14px;height:52px;font-family:var(--mono);font-size:13px}
.brand{font-weight:700;letter-spacing:.04em}.brand span{color:var(--copper)}
#chips{color:var(--muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;flex:1}
.live{margin-left:auto;color:var(--phosphor);font-size:11px;letter-spacing:.12em;text-transform:uppercase;white-space:nowrap}
h2{font-size:13px;font-family:var(--mono);letter-spacing:.14em;text-transform:uppercase;color:var(--muted);margin:28px 0 10px}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}
.tile{background:var(--panel);border:1px solid var(--line);border-radius:6px;padding:10px 14px}
.tile b{display:block;font:600 20px/1.3 var(--mono);color:var(--text)}
.tile small{color:var(--muted);font-family:var(--mono);font-size:11px}
.tests{display:grid;grid-template-columns:repeat(auto-fill,minmax(240px,1fr));gap:10px}
.test{background:var(--panel);border:1px solid var(--line);border-radius:6px;padding:10px 14px;display:flex;align-items:center;gap:10px}
.test .n{font-family:var(--mono);font-weight:600}
.test .d{color:var(--muted);font-size:12.5px;flex:1}
button{background:transparent;border:1px solid var(--leaf);color:var(--leaf);border-radius:4px;font:600 12px var(--mono);padding:5px 12px;cursor:pointer}
button:hover{background:rgba(86,200,139,.12)}
button:disabled{border-color:var(--line);color:var(--muted);cursor:default;background:transparent}
select{background:var(--panel);border:1px solid var(--line);color:var(--text);border-radius:4px;font:12px var(--mono);padding:4px}
#note{font-family:var(--mono);font-size:12.5px;color:var(--copper);min-height:18px;margin:10px 2px}
.res{background:var(--panel);border:1px solid var(--line);border-radius:6px;padding:12px 16px;margin-bottom:10px}
.res h3{margin:0 0 6px;font:600 14px var(--mono)}
.res h3 small{color:var(--muted);font-weight:400;margin-left:8px}
.res .err{color:var(--fault);font-family:var(--mono);font-size:13px}
.mrow{display:flex;flex-wrap:wrap;gap:18px;font-family:var(--mono);font-size:13.5px}
.mrow span{color:var(--muted)}
details{margin-top:8px}summary{cursor:pointer;color:var(--muted);font:12px var(--mono)}
pre{background:var(--bg);border:1px solid var(--line);border-radius:4px;padding:10px;font:12px/1.5 var(--mono);overflow-x:auto;margin:6px 0 0}
.hint{color:var(--muted);font-size:12.5px}
.hint code{font-family:var(--mono);background:var(--panel);border:1px solid var(--line);border-radius:3px;padding:1px 5px}
</style>
</head>
<body>
<header class="bar"><div class="wrap bar-in">
  <div class="brand">&#x1FA7A; HopOS <span>vitals</span></div>
  <div id="chips">connecting&hellip;</div>
  <div class="live" id="livedot">live</div>
</div></header>
<div class="wrap">
  <h2>Node</h2>
  <div class="tiles" id="tiles"></div>

  <h2>Tests</h2>
  <div style="display:flex;gap:10px;align-items:center;margin-bottom:10px">
    <button id="runall" onclick="run('all')">Run all</button>
    <span class="hint">one test at a time; results stay until the next run</span>
  </div>
  <div class="tests" id="tests"></div>
  <p class="hint" style="margin-top:10px">tx (upload) is measured from the other side:
    <code id="txhint">curl -o /dev/null http://&lt;node&gt;:&lt;port&gt;/blob?mb=64</code></p>
  <div id="note"></div>

  <h2>Results <button style="margin-left:8px" onclick="copyReport()">Copy report</button></h2>
  <div id="results"><p class="hint">no results yet — run a test above</p></div>
</div>
<script>
'use strict';
let state = null;
const fmt = (v) => {
  if (!isFinite(v)) return '?';
  const a = Math.abs(v);
  if (a >= 1000) return Math.round(v).toLocaleString('en');
  if (a >= 100) return v.toFixed(0);
  if (a >= 1) return v.toFixed(1);
  return v.toFixed(2);
};
const esc = (t) => String(t).replace(/[&<>]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]));

async function run(name) {
  let p = '';
  if (name === 'burn') p = '&secs=' + document.getElementById('burnsecs').value;
  if (name === 'rx') p = '&mb=' + document.getElementById('rxmb').value;
  const r = await fetch('/api/run?test=' + name + p);
  if (!r.ok) alert(await r.text());
  poll();
}

function tile(label, value, unit) {
  return '<div class="tile"><b>' + value + ' <small>' + unit + '</small></b><small>' + label + '</small></div>';
}

function render() {
  const s = state, n = s.node;
  document.getElementById('chips').textContent =
    n.arch + ' | ' + n.cores + ' core' + (n.cores > 1 ? 's' : '') + (n.shared ? ' (shared)' : '') +
    ' | slot ' + n.slot + ' | ' + n.ram_mb + ' MB | ' + (n.host || n.ip) + ':' + n.port +
    ' | ' + n.version;
  document.getElementById('txhint').textContent =
    'curl -o /dev/null http://' + (n.host || n.ip) + ':' + n.port + '/blob?mb=64';

  const idle = s.idle || {};
  let t = '';
  t += tile('idle (60s)', idle.ok && idle.idle_pct >= 0 ? fmt(idle.idle_pct) : 'n/a', '%');
  t += tile('wakes/s (60s)', idle.ok ? fmt(idle.wakes_per_s) : 'n/a', '');
  t += tile('cost per wake', idle.ok && idle.wake_cost_us > 0 ? fmt(idle.wake_cost_us) : 'n/a', '&micro;s');
  t += tile('temperature', s.temp_milli_c > 0 ? fmt(s.temp_milli_c / 1000) : 'n/a', '&deg;C');
  t += tile('heap', fmt(n.heap_kb / 1024), 'MB');
  t += tile('uptime', fmt(n.uptime_s / 60), 'min');
  document.getElementById('tiles').innerHTML = t;

  const busy = s.running !== '';
  // De grid wordt elke poll opnieuw gezet; bewaar de select-keuzes.
  const keep = {};
  for (const id of ['burnsecs', 'rxmb']) {
    const el = document.getElementById(id);
    if (el) keep[id] = el.value;
  }
  let h = '';
  for (const test of s.tests) {
    let extra = '';
    if (test.name === 'burn') extra = '<select id="burnsecs"><option>60</option><option selected>120</option><option>300</option><option>600</option></select>';
    if (test.name === 'rx') extra = '<select id="rxmb"><option>8</option><option selected>32</option><option>64</option></select>';
    h += '<div class="test"><span class="n">' + test.name + '</span><span class="d">' + test.desc + '</span>' + extra +
      '<button onclick="run(\'' + test.name + '\')"' + (busy ? ' disabled' : '') + '>Run</button></div>';
  }
  document.getElementById('tests').innerHTML = h;
  for (const id in keep) {
    const el = document.getElementById(id);
    if (el) el.value = keep[id];
  }
  document.getElementById('runall').disabled = busy;
  document.getElementById('note').textContent = busy ? ('running ' + s.running + ' - ' + s.note) : '';
  document.getElementById('livedot').textContent = busy ? 'testing' : 'live';

  const order = s.tests.map(t => t.name).concat(['tx']);
  let r = '';
  for (const name of order) {
    const res = s.results[name];
    if (!res) continue;
    r += '<div class="res"><h3>' + name + '<small>' + new Date(res.started).toLocaleTimeString() +
      ' &middot; ' + fmt(res.duration_s) + 's</small></h3>';
    if (res.error) r += '<div class="err">' + esc(res.error) + '</div>';
    if (res.metrics) r += '<div class="mrow">' + res.metrics.map(m =>
      '<div><span>' + m.name + '</span> <b>' + fmt(m.value) + '</b> ' + (m.unit || '') + '</div>').join('') + '</div>';
    if (res.lines && res.lines.length)
      r += '<details' + (res.lines.length < 5 ? ' open' : '') + '><summary>' + res.lines.length +
        ' detail lines</summary><pre>' + esc(res.lines.join('\n')) + '</pre></details>';
    r += '</div>';
  }
  document.getElementById('results').innerHTML = r || '<p class="hint">no results yet - run a test above</p>';
}

function copyReport() {
  const s = state, n = s.node;
  let out = '# HopOS vitals - ' + (n.host || n.ip) + '\n\n' +
    n.arch + ', ' + n.cores + ' core(s)' + (n.shared ? ' (shared)' : '') + ', ' + n.ram_mb +
    ' MB partition, ' + n.runtime + ', app ' + n.version + '\n\n';
  const idle = s.idle || {};
  if (idle.ok) out += 'idle(60s): ' + fmt(idle.idle_pct) + '% | wakes/s: ' + fmt(idle.wakes_per_s) +
    ' | cost/wake: ' + fmt(idle.wake_cost_us) + 'us\n';
  if (s.temp_milli_c > 0) out += 'temperature: ' + fmt(s.temp_milli_c / 1000) + 'C\n';
  out += '\n| test | metrics |\n|---|---|\n';
  for (const name of s.tests.map(t => t.name).concat(['tx'])) {
    const res = s.results[name];
    if (!res) continue;
    out += '| ' + name + ' | ' + (res.error ? 'ERROR: ' + res.error :
      res.metrics.map(m => m.name + ' ' + fmt(m.value) + (m.unit || '')).join(', ')) + ' |\n';
  }
  navigator.clipboard.writeText(out);
}

async function poll() {
  try {
    state = await (await fetch('/api/state')).json();
    render();
  } catch (e) {
    document.getElementById('livedot').textContent = 'offline';
  }
  setTimeout(poll, state && state.running ? 1000 : 2500);
}
poll();
</script>
</body>
</html>
`

//! De pagina op `/`: één statisch document dat `/api/state` elke twee
//! seconden leest en de cijfers tekent, met **Run all**, een knop per test
//! en **Copy report** (het rapport als platte tekst, om naast dat van een
//! gezond board te leggen).
//!
//! Alles zit in de binary: geen CDN, geen webfont. Zichtbare tekst is
//! Engels, zoals elke pagina van een node; dit commentaar is Nederlands.

/// Het document.
pub(crate) const HTML: &str = r##"<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>vitals</title>
<style>
:root { --bg:#14110f; --fg:#e8e1d9; --dim:#9a8f84; --cu:#c8773a; --bad:#e0605a; --ok:#7fb069; --line:#2b2520; }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--fg); font:15px/1.45 ui-monospace, Menlo, Consolas, monospace; }
main { max-width:980px; margin:0 auto; padding:24px 16px 48px; }
h1 { margin:0 0 4px; color:var(--cu); font-size:22px; }
.sub { color:var(--dim); margin-bottom:18px; }
.tiles { display:grid; grid-template-columns:repeat(auto-fit, minmax(150px, 1fr)); gap:10px; margin-bottom:18px; }
.tile { border:1px solid var(--line); border-radius:8px; padding:10px 12px; }
.tile b { display:block; font-size:18px; }
.tile span { color:var(--dim); font-size:12px; }
button { background:transparent; color:var(--fg); border:1px solid var(--cu); border-radius:6px; padding:4px 10px; font:inherit; cursor:pointer; }
button:hover { background:var(--cu); color:var(--bg); }
button:disabled { opacity:.4; cursor:default; }
.bar { display:flex; gap:10px; align-items:center; flex-wrap:wrap; margin-bottom:12px; }
.note { color:var(--cu); }
table { width:100%; border-collapse:collapse; }
td, th { text-align:left; vertical-align:top; padding:8px 6px; border-top:1px solid var(--line); }
th { color:var(--dim); font-weight:normal; font-size:12px; }
.m { display:inline-block; margin:0 12px 2px 0; }
.m i { color:var(--dim); font-style:normal; }
.err { color:var(--bad); }
.skip { color:var(--dim); }
details { color:var(--dim); font-size:12px; margin-top:4px; }
pre { white-space:pre-wrap; margin:4px 0 0; }
.name { color:var(--cu); }
</style>
</head>
<body>
<main>
<h1>vitals</h1>
<div class="sub" id="sub">reading the node...</div>
<div class="tiles" id="tiles"></div>
<div class="bar">
  <button id="all" onclick="run('all')">Run all</button>
  <button onclick="copyReport()">Copy report</button>
  <span class="note" id="note"></span>
</div>
<table>
  <thead><tr><th>test</th><th>result</th><th></th></tr></thead>
  <tbody id="rows"></tbody>
</table>
</main>
<script>
let state = null;
const esc = s => String(s).replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
const fmt = v => v === null || v === undefined ? 'n/a' : (Math.abs(v) >= 100 ? v.toFixed(0) : Math.abs(v) >= 10 ? v.toFixed(1) : v.toFixed(2));
function tile(label, value) { return '<div class="tile"><b>' + esc(value) + '</b><span>' + esc(label) + '</span></div>'; }
function render() {
  const s = state, n = s.node, i = s.idle;
  document.getElementById('sub').textContent = n.name + ' / slot ' + n.slot + ' / ' + n.ip + ':' + n.port + ' / ' + n.arch + ' / vitals ' + n.version;
  const temp = s.temp_milli_c > 0 ? (s.temp_milli_c / 1000).toFixed(1) + ' C' : 'n/a';
  document.getElementById('tiles').innerHTML =
    tile('cores (online)', n.cores + ' (' + n.online + ')' + (n.shared ? ' shared' : '')) +
    tile('idle, last ' + fmt(i.span_s) + ' s', i.ok ? (i.idle_pct === null ? 'n/a' : fmt(i.idle_pct) + ' %') : 'warming up') +
    tile('wakes per second', i.ok ? fmt(i.wakes_per_s) : '-') +
    tile('cost per wake', i.ok && i.wake_cost_us !== null ? fmt(i.wake_cost_us) + ' us' : 'n/a') +
    tile('temperature', temp) +
    tile('heap used / peak', n.heap_used_kb + ' / ' + n.heap_peak_kb + ' KB') +
    tile('uptime', n.uptime_s + ' s');
  document.getElementById('note').textContent = s.running ? 'running ' + s.running + (s.note ? ': ' + s.note : '') : (i.idle_note || '');
  document.getElementById('all').disabled = !!s.running;
  let rows = '';
  for (const t of s.tests) {
    const r = s.results[t.name];
    let res = '<span class="skip">' + esc(t.desc) + '</span>';
    if (r) {
      if (r.error) res = '<span class="err">' + esc(r.error) + '</span>';
      else if (r.skipped) res = '<span class="skip">skipped: ' + esc(r.skipped) + '</span>';
      else res = r.metrics.map(m => '<span class="m">' + esc(m.name) + ' <b>' + fmt(m.value) + '</b> <i>' + esc(m.unit) + '</i></span>').join('');
      if (r.lines && r.lines.length) res += '<details><summary>' + r.lines.length + ' line(s), ' + r.duration_s.toFixed(1) + ' s</summary><pre>' + esc(r.lines.join('\n')) + '</pre></details>';
    }
    const btn = t.runnable ? '<button onclick="run(\'' + t.name + '\')"' + (s.running ? ' disabled' : '') + '>Run</button>' : '';
    rows += '<tr><td class="name">' + esc(t.name) + '</td><td>' + res + '</td><td>' + btn + '</td></tr>';
  }
  document.getElementById('rows').innerHTML = rows;
}
async function refresh() {
  try { const r = await fetch('/api/state', {cache: 'no-store'}); state = await r.json(); render(); }
  catch (e) { document.getElementById('note').textContent = 'no answer from the node'; }
}
async function run(name) {
  const r = await fetch('/api/run?test=' + encodeURIComponent(name));
  if (!r.ok) document.getElementById('note').textContent = await r.text();
  refresh();
}
function copyReport() {
  if (!state) return;
  const n = state.node, i = state.idle;
  let out = 'vitals ' + n.version + ' on ' + n.name + ' (' + n.arch + ', slot ' + n.slot + ', ' + n.cores + ' core(s), ' + n.ram_mb + ' MB)\n';
  out += 'idle: ' + (i.ok ? (i.idle_pct === null ? 'n/a' : fmt(i.idle_pct) + ' %') + ', ' + fmt(i.wakes_per_s) + ' wakes/s, ' + (i.wake_cost_us === null ? 'n/a' : fmt(i.wake_cost_us) + ' us') + ' per wake' : 'no window yet') + (i.idle_note ? ' (' + i.idle_note + ')' : '') + '\n';
  out += 'temperature: ' + (state.temp_milli_c > 0 ? (state.temp_milli_c / 1000).toFixed(1) + ' C' : 'n/a') + '\n';
  for (const t of state.tests) {
    const r = state.results[t.name];
    if (!r) continue;
    out += '\n' + t.name + ': ';
    if (r.error) out += 'ERROR ' + r.error;
    else if (r.skipped) out += 'skipped (' + r.skipped + ')';
    else out += r.metrics.map(m => m.name + ' ' + fmt(m.value) + ' ' + m.unit).join(', ');
    out += '\n';
    for (const l of (r.lines || [])) out += '  ' + l + '\n';
  }
  navigator.clipboard.writeText(out).then(() => { document.getElementById('note').textContent = 'report copied'; }, () => { window.prompt('Copy the report:', out); });
}
refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
"##;

#[cfg(test)]
mod tests {
    use super::HTML;

    #[test]
    fn the_page_asks_the_api_and_has_the_buttons() {
        assert!(HTML.starts_with("<!doctype html>"));
        assert!(HTML.contains("/api/state"));
        assert!(HTML.contains("/api/run?test="));
        assert!(HTML.contains("Run all"));
        assert!(HTML.contains("Copy report"));
        // Geen bron van buiten: een node zonder internet ziet hetzelfde.
        assert!(!HTML.contains("http://"));
        assert!(!HTML.contains("https://"));
    }
}

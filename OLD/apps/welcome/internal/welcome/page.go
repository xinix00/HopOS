package welcome

import (
	"html"
	"strings"
)

// Page rendert de pagina met de node-gegevens én de cijfers van dit moment
// erin. Per opvraging opnieuw: een pagina-hit is mensen-tempo, en zo staan de
// levende getallen ook goed in de HTML zelf — met JS uit blijft de pagina waar
// in plaats van een gestolde momentopname van de boot te tonen. De inline JS
// houdt ze daarna bij via /api/status, dat wél elke seconde langskomt en
// daarom niets meer is dan negen getallen.
//
// Alles zit in de binary: geen CDN, geen webfont-URL, geen plaatje van
// gethop.org. Een node kan zonder internet in een rek staan en moet er dan nóg
// zo uitzien. De https-links in de tekst zijn voor de laptop van de lezer, niet
// voor de node zelf (apphttp linkt geen TLS en zou ze niet kunnen ophalen).
func Page(n Node, st Status) []byte {
	// De artifact-URL van dít image: per architectuur een eigen asset, dus de
	// voorbeelden op de pagina kloppen op arm64 én riscv64.
	artifact := "https://github.com/xinix00/hop/releases/download/rolling-release/welcome-" +
		n.Arch + "-tamago.elf"

	dnsRow := ""
	if n.DNS != "" {
		dnsRow = `<tr><td class="f">cluster dns</td><td class="v">` + esc(n.DNS) +
			`</td><td class="n">HOP_DNS — where this app would look up its neighbours by name (it does not need to)</td></tr>`
	}

	// Drie plekken praten over de core, en alle drie zeggen wat HOP NÚ zegt.
	// "Een hele core" is geen eigenschap maar een meting: het is de default, maar
	// met een sharegroup zetten er meerdere kooien op één core, en HOP zet dat om
	// terwijl de app leeft. Dus geen belofte, alleen dit moment.
	//
	// En let op het verschil dat de pagina eerder verkeerd had: app.Slot is het
	// KOOInummer (HOP patcht slotHint in het image bij Start; MPIDR is alleen de
	// terugval, en op servers is dat geen slotnummer). Op welke fysieke core die
	// kooi landt hoort een app niet te weten — dus staat dat er ook niet.
	phrase, tile, core := "a core HOP assigned it", "the cage HOP started this app in", "—"
	switch st.Core {
	case CoreDedicated:
		phrase = "a core it has to itself at this moment"
		tile = "nobody else on this core right now"
		core = "dedicated"
	case CoreShared:
		phrase = "a core it shares with at least one other slot, because someone asked for that density with a sharegroup"
		tile = "sharing this core with another slot"
		core = "shared"
	}

	return []byte(strings.NewReplacer(
		"__CORE_PHRASE__", phrase,
		"__CORE_TILE__", tile,
		"__CORE_VALUE__", core,
		"__HOST__", esc(n.Host),
		"__ADDR__", esc(URL(n)),
		"__BANNER__", esc(Banner(n)),
		"__IP__", esc(n.IP),
		"__PORT__", esc(n.Port),
		"__SLOT__", itoa(n.Slot),
		"__CORES__", itoa(n.Cores),
		"__RAM__", fmtBytes(n.RAMSize),
		"__ARCH__", esc(n.Arch),
		"__RUNTIME__", esc(n.Runtime),
		"__VERSION__", esc(n.Version),
		"__ARTIFACT__", esc(artifact),
		"__DNS_ROW__", dnsRow,
		"__UPTIME__", fmtDuration(st.UptimeSeconds),
		"__HEAP__", fmtBytes(st.HeapBytes),
		"__OBJECTS__", fmtCount(st.HeapObjects),
		"__GOROUTINES__", itoa(st.Goroutines),
		"__VIEWS__", fmtCount(st.Views),
		"__REQS__", fmtCount(st.Requests),
	).Replace(pageHTML))
}

// esc houdt env-waarden (HOPOS_HOST, HOP_DNS) uit de HTML-grammatica: het is
// tekst van buiten, ook al komt het van dichtbij.
func esc(s string) string { return html.EscapeString(s) }

func itoa(n int) string {
	if n < 0 {
		return "?"
	}
	return fmtCount(uint64(n))
}

// pageHTML is de pagina met __TOKENS__ waar de node-gegevens in gaan. Geen
// backticks in de CSS/JS: dit is een raw string.
const pageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HopOS — node __HOST__</title>
<meta name="description" content="A HopOS node. This page is served by an app that is the operating system in its own slot.">
<meta name="robots" content="noindex">
<link rel="icon" href="data:image/svg+xml,<svg xmlns=%22http://www.w3.org/2000/svg%22 viewBox=%220 0 100 100%22><text y=%22.9em%22 font-size=%2290%22>🐇</text></svg>">
<style>
:root{--bg:#0b0f14;--panel:#10161d;--panel-2:#0d1219;--line:#1d2733;--text:#cdd6e0;--muted:#7f8b9b;--copper:#e09a63;--leaf:#56c88b;--leaf-dim:#3a8a60;--phosphor:#46d979;--fault:#e0654f;--acc:var(--leaf);--acc-dim:var(--leaf-dim);--mono:ui-monospace,"SF Mono","Cascadia Code","JetBrains Mono",Menlo,Consolas,monospace;--sans:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:16.5px/1.65 var(--sans);-webkit-font-smoothing:antialiased}
a{color:var(--acc);text-decoration:none}
a:hover{text-decoration:underline;text-underline-offset:3px}
a:focus-visible{outline:2px solid var(--acc);outline-offset:3px;border-radius:2px}
.wrap{max-width:1060px;margin:0 auto;padding:0 24px}
code{font-family:var(--mono);font-size:.88em;background:var(--panel);border:1px solid var(--line);border-radius:3px;padding:1px 5px;color:var(--text)}

.bar{position:sticky;top:0;z-index:10;background:color-mix(in srgb,var(--bg) 88%,transparent);backdrop-filter:blur(8px);border-bottom:1px solid var(--line)}
.bar-in{display:flex;align-items:center;gap:18px;height:56px;font-family:var(--mono);font-size:13.5px}
.brand{font-weight:700;letter-spacing:.04em;white-space:nowrap}
.brand .ears{color:var(--copper);font-weight:400;margin-right:9px}
.crumb{color:var(--muted);min-width:0;flex:1 1 auto;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
@media (max-width:520px){.crumb{display:none}}
.live{margin-left:auto;display:flex;align-items:center;gap:8px;color:var(--phosphor);font-size:12px;letter-spacing:.14em;text-transform:uppercase;white-space:nowrap}
.live i{width:8px;height:8px;border-radius:50%;background:var(--phosphor);box-shadow:0 0 9px var(--phosphor);animation:pulse 2.4s ease-in-out infinite}
.live.off{color:var(--fault)}
.live.off i{background:var(--fault);box-shadow:none;animation:none}
@keyframes pulse{50%{opacity:.3}}
@media (prefers-reduced-motion:reduce){.live i{animation:none}}

.hero{padding:60px 0 44px}
.rabbit{margin:0 0 24px;font-family:var(--mono);font-size:14px;line-height:1.5;color:var(--copper)}
.mark{font-family:var(--mono);font-size:12.5px;letter-spacing:.18em;text-transform:uppercase;color:var(--acc);margin:0 0 16px}
h1{font-family:var(--mono);font-size:clamp(27px,4.2vw,42px);line-height:1.16;letter-spacing:-.015em;margin:0 0 20px;font-weight:700;text-wrap:balance}
.lede{font-size:17.5px;color:var(--muted);max-width:35em;margin:0}
.lede strong{color:var(--text);font-weight:600}

.tiles{display:grid;grid-template-columns:repeat(4,1fr);gap:16px;padding-bottom:52px}
@media (max-width:880px){.tiles{grid-template-columns:repeat(2,1fr)}}
@media (max-width:520px){.tiles{grid-template-columns:1fr}}
.tile{border:1px solid var(--line);border-radius:6px;background:var(--panel-2);padding:18px 20px 16px;min-width:0}
.tile .k{display:block;font-family:var(--mono);font-size:11px;letter-spacing:.15em;text-transform:uppercase;color:var(--muted)}
.tile .v{display:block;font-family:var(--mono);font-size:21px;font-weight:700;color:var(--acc);margin:7px 0 5px;overflow-wrap:anywhere;line-height:1.25}
.tile .n{display:block;font-size:12.5px;color:var(--muted);line-height:1.45}

section{padding:52px 0;border-top:1px solid var(--line)}
h2{font-family:var(--mono);font-size:clamp(19px,2.6vw,25px);margin:0 0 14px;letter-spacing:-.01em}
.section-lede{color:var(--muted);max-width:44em;margin:0 0 30px}
.section-lede strong{color:var(--text);font-weight:600}

.tbl-wrap{overflow-x:auto;border:1px solid var(--line);border-radius:5px}
table{border-collapse:collapse;width:100%;min-width:620px;font-size:14.5px}
th{font-family:var(--mono);font-size:11px;text-transform:uppercase;letter-spacing:.1em;color:var(--muted);font-weight:600;text-align:left;background:var(--panel-2)}
th,td{padding:12px 18px;border-bottom:1px solid var(--line);vertical-align:top}
tr:last-child td{border-bottom:0}
td.f{color:var(--muted);font-family:var(--mono);font-size:13px;white-space:nowrap}
td.v{font-family:var(--mono);color:var(--text);white-space:nowrap;font-variant-numeric:tabular-nums}
td.n{color:var(--muted);font-size:13.5px}
td.n em{color:var(--text);font-style:normal}
.l{color:var(--phosphor)}
.legend{font-family:var(--mono);font-size:12.5px;color:var(--muted);margin:16px 0 0}
/* Smal: drie kolommen naast elkaar wordt één woord per regel, dus de tabel
   klapt om in een blok per feit — label, waarde en herkomst onder elkaar. Deze
   regels staan ná de kolom-regels hierboven, anders winnen die. */
@media (max-width:660px){
table{min-width:0}
thead{display:none}
tbody,tr,td{display:block}
tr{border-bottom:1px solid var(--line);padding:13px 16px}
tr:last-child{border-bottom:0}
th,td{border:0;padding:1px 0;white-space:normal}
td.f{font-size:11px;letter-spacing:.13em;text-transform:uppercase}
td.v{font-size:15.5px;white-space:normal;overflow-wrap:anywhere}
td.n{font-size:13px;margin-top:3px}
}

.honest{border:1px solid var(--line);border-left:3px solid var(--acc);border-radius:4px;padding:20px 24px;background:var(--panel-2);max-width:46em;margin-top:32px}
.honest h3{font-family:var(--mono);font-size:12.5px;letter-spacing:.11em;text-transform:uppercase;margin:0 0 10px;color:var(--acc)}
.honest p{font-size:14.5px;color:var(--muted);margin:0}
.honest p b{color:var(--text);font-weight:600}

.term{background:#070b10;border:1px solid var(--line);border-radius:6px;overflow:hidden;box-shadow:0 24px 60px -32px rgba(0,0,0,.9)}
.term-bar{display:flex;align-items:center;gap:8px;padding:9px 14px;border-bottom:1px solid var(--line);font-family:var(--mono);font-size:11.5px;color:var(--muted);letter-spacing:.05em;background:var(--panel-2)}
.term-bar .dot{width:9px;height:9px;border-radius:50%;background:var(--line);flex-shrink:0}
.term-bar .dot:first-child{background:#2c3a4a}
.term pre{margin:0;padding:18px;overflow-x:auto;font-family:var(--mono);font-size:12.8px;line-height:1.62;color:var(--phosphor)}
.term .cmt{color:var(--muted)}
.term .ps{color:var(--copper)}
.term .hi{color:#7dffa8}

.cards{display:grid;grid-template-columns:repeat(3,1fr);gap:18px;margin-top:30px}
@media (max-width:880px){.cards{grid-template-columns:1fr}}
.card{border:1px solid var(--line);border-radius:5px;padding:22px;background:var(--panel-2)}
.card h3{font-family:var(--mono);font-size:15px;margin:0 0 9px;color:var(--text)}
.card p{font-size:14px;color:var(--muted);margin:0}

footer{padding:46px 0 56px;border-top:1px solid var(--line)}
.foot{display:flex;justify-content:space-between;align-items:flex-start;gap:44px;flex-wrap:wrap}
footer pre{margin:0;font-family:var(--mono);font-size:13px;line-height:1.5;color:var(--copper);flex-shrink:0}
.foot-links{font-family:var(--mono);font-size:13.5px;display:grid;gap:10px;justify-items:start}
.foot-copy{margin:34px 0 0;padding-top:18px;border-top:1px solid var(--line);font-family:var(--mono);font-size:12px;color:var(--muted)}
</style>
</head>
<body>

<header class="bar"><div class="wrap bar-in">
  <span class="brand"><span class="ears">(\(\</span>HopOS</span>
  <span class="crumb">node __HOST__ · slot __SLOT__</span>
  <span class="live" id="live"><i></i><span id="livetxt">live</span></span>
</div></header>

<main>
<div class="wrap">
  <div class="hero">
    <pre class="rabbit">   (\(\
   ( -.-)
   o_(")(")</pre>
    <p class="mark">this node is up</p>
    <h1>You have reached a HopOS node.</h1>
    <p class="lede">The page you are reading is served by a Go program that <strong>is</strong>
    the operating system in slot __SLOT__ of this machine. No Linux underneath, no container
    around it, no kernel in between — <strong>__CORE_PHRASE__</strong>, and a memory partition
    drawn in hardware. It is also the first job this node ran, which is why you got
    something instead of a closed port.</p>
  </div>

  <div class="tiles">
    <div class="tile"><span class="k">node</span><span class="v">__HOST__</span><span class="n">this address works from outside and from the node itself</span></div>
    <div class="tile"><span class="k">slot</span><span class="v">__SLOT__</span><span class="n">__CORE_TILE__</span></div>
    <div class="tile"><span class="k">memory</span><span class="v">__RAM__</span><span class="n">a partition, not a quota — the limit is hardware</span></div>
    <div class="tile"><span class="k">runtime</span><span class="v">__ARCH__</span><span class="n">__RUNTIME__ — the Go runtime is the OS here</span></div>
  </div>
</div>

<section><div class="wrap">
  <h2>What this app knows about itself</h2>
  <p class="section-lede">Everything below is first-hand: its own runtime, its own network
  stack, and the handful of variables HOP handed it at start. <strong>Nothing was asked of
  the cluster.</strong></p>

  <div class="tbl-wrap"><table>
    <thead><tr><th>fact</th><th>value</th><th>where it comes from</th></tr></thead>
    <tbody>
      <tr><td class="f">node address</td><td class="v">__ADDR__</td><td class="n">the port HOP published on the node IP; since v1.5.4 the same address also works from inside</td></tr>
      <tr><td class="f">app address</td><td class="v">__IP__</td><td class="n">its <em>own</em> network stack and IP on the internal net — no shared socket layer</td></tr>
      <tr><td class="f">slot</td><td class="v">__SLOT__</td><td class="n">the cage HOP started this app in — it patches the number into the image at start. <em>Which</em> physical core that cage landed on is HOP's business; an app is not told, and with a sharegroup several cages share one</td></tr>
      <tr><td class="f">this core is</td><td class="v"><span class="l" data-k="core">__CORE_VALUE__</span></td><td class="n">whole cores are the default, and sharing is <em>allowed but never imposed</em>: HOP stacks cages on one core only when a job asks for it with a <em>sharegroup</em>, so nothing lands on your core behind your back. Which is why this is measured rather than promised — and it flips while the app runs as neighbours arrive and leave</td></tr>
      <tr><td class="f">cores</td><td class="v">__CORES__</td><td class="n">how many this app was given; a job asking for more CPU shares gets more, sharing one heap. Whether they are exclusive is the row above</td></tr>
      <tr><td class="f">memory partition</td><td class="v">__RAM__</td><td class="n">handed out by HOP and enforced by the hardware — there is nothing to exceed</td></tr>
      <tr><td class="f">heap in use</td><td class="v"><span class="l" data-k="heap">__HEAP__</span></td><td class="n">from the Go runtime, which on this core <em>is</em> the operating system</td></tr>
      <tr><td class="f">live objects</td><td class="v"><span class="l" data-k="objects">__OBJECTS__</span></td><td class="n">everything this page needs is already in memory; there is no filesystem to read</td></tr>
      <tr><td class="f">goroutines</td><td class="v"><span class="l" data-k="goroutines">__GOROUTINES__</span></td><td class="n">including the heartbeat that tells HOP this app has not hung</td></tr>
      <tr><td class="f">uptime</td><td class="v"><span class="l" data-k="uptime">__UPTIME__</span></td><td class="n">since this app reported READY — the machine underneath may well be older</td></tr>
      <tr><td class="f">page views</td><td class="v"><span class="l" data-k="views">__VIEWS__</span></td><td class="n">counted by the app itself; nothing else is keeping score</td></tr>
      <tr><td class="f">requests served</td><td class="v"><span class="l" data-k="requests">__REQS__</span></td><td class="n">including the once-a-second poll that keeps these numbers moving</td></tr>
      __DNS_ROW__
      <tr><td class="f">image</td><td class="v">welcome __VERSION__</td><td class="n">one artifact, canonically linked — the same image runs in any slot</td></tr>
    </tbody>
  </table></div>
  <p class="legend"><span class="l">●</span> live values, polled from <code>/api/status</code> once a second. <code>/healthz</code> answers plain <code>ok</code>.</p>

  <div class="honest">
    <h3>what this page does not know</h3>
    <p>No node name, no cluster name, no uptime for the machine. A slot is not told any of
    that, and <b>this page asks nothing of the cluster API</b> — that call would need the
    cluster key, and a welcome page you cannot read without a key is not a welcome. So it
    shows what it can see for itself, and makes nothing up.</p>
  </div>
</div></section>

<section><div class="wrap">
  <h2>Make it yours</h2>
  <p class="section-lede">This page is just a job — the first one a headless node runs.
  Point that job at your own image and the face of this node changes with it.</p>

  <div class="term">
    <div class="term-bar"><span class="dot"></span><span class="dot"></span><span class="dot"></span><span>this node</span></div>
    <pre><span class="cmt"># the boot-config line that started this app (one line in hopos.cfg)</span>
hopos.init[]={"name":"welcome","driver":"hop",
  "artifacts":[{"url":"__ARTIFACT__"}],
  "memory_limit":67108864,"ports":{"http":__PORT__}}

<span class="cmt"># or hand it to a running cluster — ports are published from the job spec</span>
<span class="ps">$</span> hop apply --name welcome --driver hop \
      --artifact __ARTIFACT__ --memory 64M

<span class="cmt"># what this app has been saying all along</span>
<span class="ps">$</span> hop logs welcome
<span class="hi">__BANNER__</span></pre>
  </div>

  <div class="cards">
    <div class="card"><h3>your app, whole cores</h3><p>A <code>hop</code>-driver job is a raw Go image: no Dockerfile, no base image, no kernel to boot. Linked once, it runs in any slot on any node.</p></div>
    <div class="card"><h3>logs and health</h3><p><code>hop logs welcome</code> tails what this app writes over the log ring. <code>/healthz</code> answers <code>ok</code> for whatever is watching.</p></div>
    <div class="card"><h3>read the story</h3><p><a href="https://gethop.org/hopos/">gethop.org/hopos</a> explains what an OS without a kernel buys you, and <a href="https://gethop.org/hopos/docs/">the docs</a> show how to write one of these.</p></div>
  </div>
</div></section>
</main>

<footer><div class="wrap">
  <div class="foot">
    <pre>   (\(\
   ( -.-)
   o_(")(")</pre>
    <div class="foot-links">
      <a href="https://gethop.org/hopos/">HopOS — the Go-only OS</a>
      <a href="https://gethop.org/hop/">HOP — the orchestrator</a>
      <a href="https://gethop.org/hopos/docs/">docs</a>
      <a href="https://github.com/xinix00/hop">github/hop</a>
    </div>
  </div>
  <p class="foot-copy">welcome __VERSION__ · __RUNTIME__ · __ARCH__ · served straight from RAM by the app itself — no filesystem, no TLS and no kernel in this image.</p>
</div></footer>

<script>
var live = document.getElementById("live"), livetxt = document.getElementById("livetxt");
function set(k, v) {
  var e = document.querySelector('[data-k="' + k + '"]');
  if (e) e.textContent = v;
}
function bytes(n) {
  if (n >= 1073741824) return (n / 1073741824).toFixed(2) + " GB";
  if (n >= 1048576) return (n / 1048576).toFixed(1) + " MB";
  if (n >= 1024) return Math.round(n / 1024) + " kB";
  return n + " B";
}
function dur(s) {
  var p = function (n) { return (n < 10 ? "0" : "") + n; };
  var d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600),
      m = Math.floor(s % 3600 / 60), x = Math.floor(s % 60);
  if (d > 0) return d + "d " + p(h) + "h " + p(m) + "m";
  if (h > 0) return h + "h " + p(m) + "m " + p(x) + "s";
  if (m > 0) return m + "m " + p(x) + "s";
  return x + "s";
}
function num(n) { return n.toLocaleString("en-US"); }
function tick() {
  fetch("/api/status", { cache: "no-store" }).then(function (r) {
    if (!r.ok) throw new Error(r.status);
    return r.json();
  }).then(function (s) {
    set("uptime", dur(s.uptime_seconds));
    set("heap", bytes(s.heap_bytes));
    set("objects", num(s.heap_objects));
    set("goroutines", num(s.goroutines));
    set("views", num(s.views));
    set("requests", num(s.requests));
    set("core", s.core === "unknown" ? "—" : s.core);
    live.className = "live";
    livetxt.textContent = "live";
  }).catch(function () {
    live.className = "live off";
    livetxt.textContent = "no signal";
  });
}
tick();
setInterval(tick, 1000);
</script>

</body>
</html>
`

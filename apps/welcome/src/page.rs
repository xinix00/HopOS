//! De pagina: één HTML-document per verzoek, met de getallen van dit moment.
//!
//! Per verzoek opnieuw opgebouwd: een paginabezoek is mensentempo, en zo
//! staan de levende getallen (uptime, verzoeken, heap) goed in de HTML zelf,
//! zonder script dat ze bijhoudt. Alles zit in de binary: geen CDN, geen
//! webfont, geen plaatje van buiten. Een node kan zonder internet in een rek
//! staan en moet er dan nog zo uitzien (de Go-welcome, `OLD/apps/welcome`).
//!
//! De tekst op de pagina is Engels: het is een publieke pagina. Waarden van
//! buiten (de naam van de node uit de env, de `Host`-kop van de browser)
//! gaan altijd door [`Esc`].
//!
//! Dit module bezit niets en doet geen I/O: [`render`] schrijft in een
//! buffer die de aanroeper vooraf reserveerde, en weigert (in plaats van te
//! groeien) als hij vol is. Zo is een pagina nooit een verborgen allocatie
//! die bij een volle heap het programma afbreekt (handboek §6).

use alloc::vec::Vec;
use core::fmt::{self, Write};

/// De ruimte die [`render`] hooguit gebruikt. De pagina is 7760 bytes
/// (gemeten 29-09 op QEMU, `tools/qemu-test-welcome.sh`); de rest is marge
/// voor een lange nodenaam of `Host`-kop, die leanhttp elk op 8 KiB per
/// kopregel begrenst.
pub(crate) const PAGE_CAP: usize = 32 << 10;

/// De bunny van HopOS, in tekst: dezelfde als op de Go-pagina.
pub(crate) const BUNNY: &str = "   (\\(\\\n   ( -.-)\n   o_(\")(\")";

/// Wat de app bij de start over zichzelf weet.
pub(crate) struct Facts<'a> {
    /// De naam van de node (`ER_ATTR_NODE_ID` van Hop).
    pub(crate) node: &'a str,
    /// Het slot (de kooi) waarin de app draait.
    pub(crate) slot: u64,
    /// Het eigen adres op het slot-LAN.
    pub(crate) ip: [u8; 4],
    /// De poort waarop hij luistert (`ER_PORT_HTTP`).
    pub(crate) port: u16,
    /// De RAM-declaratie: de partitie min de ABI-staart.
    pub(crate) ram: u64,
    /// De versie van dit image.
    pub(crate) version: &'a str,
}

/// De getallen van dit moment.
pub(crate) struct Live<'a> {
    /// Hoe de browser de node noemde (de `Host`-kop), of leeg.
    pub(crate) host: &'a str,
    /// Sinds READY, in nanoseconden.
    pub(crate) uptime_ns: u64,
    /// Verzoeken, dit meegeteld.
    pub(crate) requests: u64,
    /// Bytes in gebruik op de heap.
    pub(crate) heap_used: u64,
    /// De hoogste stand sinds de start.
    pub(crate) heap_peak: u64,
    /// De maat van de heap.
    pub(crate) heap_capacity: u64,
    /// Geslaagde allocaties sinds de start.
    pub(crate) allocs: u64,
}

/// Een schrijver in een vooraf gereserveerde buffer die nooit groeit: past
/// een stuk niet meer, dan is het een `fmt::Error`.
struct Bounded<'v>(&'v mut Vec<u8>);

impl Write for Bounded<'_> {
    fn write_str(&mut self, s: &str) -> fmt::Result {
        if self.0.len().saturating_add(s.len()) > self.0.capacity() {
            return Err(fmt::Error);
        }
        // Binnen de capaciteit: geen hertoewijzing.
        self.0.extend_from_slice(s.as_bytes());
        Ok(())
    }
}

/// Tekst van buiten, ontsnapt voor HTML.
pub(crate) struct Esc<'a>(pub(crate) &'a str);

impl fmt::Display for Esc<'_> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let mut rest = self.0;
        while let Some(i) = rest.find(['&', '<', '>', '"', '\'']) {
            let (plain, tail) = rest.split_at(i);
            f.write_str(plain)?;
            let mut chars = tail.chars();
            f.write_str(match chars.next() {
                Some('&') => "&amp;",
                Some('<') => "&lt;",
                Some('>') => "&gt;",
                Some('"') => "&quot;",
                _ => "&#39;",
            })?;
            rest = chars.as_str();
        }
        f.write_str(rest)
    }
}

/// Bytes, leesbaar: `512 B`, `12.5 KiB`, `62.0 MiB`.
pub(crate) struct Bytes(pub(crate) u64);

impl fmt::Display for Bytes {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        const UNITS: [&str; 4] = ["KiB", "MiB", "GiB", "TiB"];
        if self.0 < 1024 {
            return write!(f, "{} B", self.0);
        }
        // In tienden, met gehele getallen: geen float in een no_std-app.
        let mut tenths = self.0.saturating_mul(10) / 1024;
        let mut unit = 0;
        while tenths >= 10 * 1024 && unit + 1 < UNITS.len() {
            tenths /= 1024;
            unit += 1;
        }
        let name = UNITS.get(unit).copied().unwrap_or("?");
        write!(f, "{}.{} {name}", tenths / 10, tenths % 10)
    }
}

/// Een duur, leesbaar: `42s`, `3m 07s`, `2h 05m`, `1d 03h`.
pub(crate) struct Uptime(pub(crate) u64);

impl fmt::Display for Uptime {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = self.0 / 1_000_000_000;
        let (d, h, m) = (s / 86_400, s / 3600 % 24, s / 60 % 60);
        match (d, h, m) {
            (0, 0, 0) => write!(f, "{s}s"),
            (0, 0, _) => write!(f, "{m}m {:02}s", s % 60),
            (0, _, _) => write!(f, "{h}h {m:02}m"),
            _ => write!(f, "{d}d {h:02}h"),
        }
    }
}

/// Een telling met duizendtallen: `1,234,567`.
pub(crate) struct Count(pub(crate) u64);

impl fmt::Display for Count {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let n = self.0;
        if n < 1000 {
            return write!(f, "{n}");
        }
        write!(f, "{},{:03}", Count(n / 1000), n % 1000)
    }
}

/// Schrijft de pagina in `out`, binnen zijn capaciteit (reserveer
/// [`PAGE_CAP`]). Een `fmt::Error` is een pagina die niet paste; `out` is
/// dan half gevuld en hoort weggegooid.
pub(crate) fn render(facts: &Facts<'_>, live: &Live<'_>, out: &mut Vec<u8>) -> fmt::Result {
    let mut w = Bounded(out);
    let node = Esc(facts.node);
    let slot = facts.slot;
    let [a, b, c, d] = facts.ip;
    let up = Uptime(live.uptime_ns);
    let reqs = Count(live.requests);
    write!(
        w,
        "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n\
         <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n\
         <title>HopOS - node {node}</title>\n<meta name=\"robots\" content=\"noindex\">\n"
    )?;
    w.write_str(STYLE)?;
    write!(
        w,
        "</head>\n<body>\n<header class=\"bar\"><div class=\"wrap bar-in\">\
         <span class=\"brand\"><span class=\"ears\">(\\(\\</span>HopOS</span>\
         <span class=\"crumb\">node {node} &middot; slot {slot}</span>\
         <span class=\"live\"><i></i>live</span></div></header>\n<main>\n<div class=\"wrap\">\n\
         <div class=\"hero\">\n<pre class=\"rabbit\">{BUNNY}</pre>\n\
         <p class=\"mark\">this node is up</p>\n<h1>You have reached a HopOS node.</h1>\n\
         <p class=\"lede\">The page you are reading is served by a Rust program that \
         <strong>is</strong> the operating system in slot {slot} of this machine. No Linux \
         underneath, no container around it, no kernel in between: a core HOP assigned it, \
         and a memory partition drawn in hardware.</p>\n</div>\n"
    )?;
    write!(
        w,
        "<div class=\"tiles\">\n\
         <div class=\"tile\"><span class=\"k\">node</span><span class=\"v\">{node}</span>\
         <span class=\"n\">the name HOP knows this machine by</span></div>\n\
         <div class=\"tile\"><span class=\"k\">slot</span><span class=\"v\">{slot}</span>\
         <span class=\"n\">the cage HOP started this app in</span></div>\n\
         <div class=\"tile\"><span class=\"k\">uptime</span><span class=\"v\">{up}</span>\
         <span class=\"n\">since this app started serving</span></div>\n\
         <div class=\"tile\"><span class=\"k\">requests</span><span class=\"v\">{reqs}</span>\
         <span class=\"n\">this one included</span></div>\n</div>\n</div>\n"
    )?;
    w.write_str(
        "<section><div class=\"wrap\">\n<h2>What this app knows about itself</h2>\n\
         <p class=\"section-lede\">Everything below is first-hand: its own heap, its own \
         network stack, and the handful of variables HOP handed it at start. \
         <strong>Nothing was asked of the cluster.</strong></p>\n\
         <div class=\"tbl-wrap\"><table>\n\
         <thead><tr><th>fact</th><th>value</th><th>where it comes from</th></tr></thead>\n<tbody>\n",
    )?;
    let host = if live.host.is_empty() { "-" } else { live.host };
    row(
        &mut w,
        "node",
        &node,
        "ER_ATTR_NODE_ID, handed over by HOP at start",
    )?;
    row(
        &mut w,
        "reached as",
        &Esc(host),
        "the Host your browser asked for; the kern forwards that port on the node address to this slot",
    )?;
    row(
        &mut w,
        "app address",
        &format_args!("{a}.{b}.{c}.{d}:{}", facts.port),
        "its <em>own</em> network stack on the slot LAN, listening on ER_PORT_HTTP",
    )?;
    row(
        &mut w,
        "slot",
        &slot,
        "the cage number; which physical core it landed on is HOP's business",
    )?;
    row(
        &mut w,
        "memory partition",
        &Bytes(facts.ram),
        "handed out by HOP and enforced by stage-2 translation: there is nothing to exceed",
    )?;
    row(
        &mut w,
        "heap in use",
        &Bytes(live.heap_used),
        "from the allocator of applib, counted per block",
    )?;
    row(
        &mut w,
        "heap peak",
        &Bytes(live.heap_peak),
        "the high-water mark since start",
    )?;
    row(
        &mut w,
        "heap size",
        &Bytes(live.heap_capacity),
        "what lies between the image and the stack",
    )?;
    row(
        &mut w,
        "allocations",
        &Count(live.allocs),
        "successful allocations since start, counted by the allocator itself",
    )?;
    row(
        &mut w,
        "uptime",
        &up,
        "since the server started; the machine underneath may well be older",
    )?;
    row(
        &mut w,
        "requests served",
        &reqs,
        "counted by the app itself; nothing else is keeping score",
    )?;
    row(
        &mut w,
        "image",
        &format_args!("welcome {}", facts.version),
        "one artifact, canonically linked: the same image runs in any slot",
    )?;
    write!(
        w,
        "</tbody></table></div>\n</div></section>\n</main>\n\
         <footer><div class=\"wrap foot\"><pre>{BUNNY}</pre>\
         <div class=\"foot-links\"><span>HopOS v3</span><span>Rust, no_std, one slot</span>\
         <a href=\"/health\">/health</a></div></div></footer>\n</body>\n</html>\n"
    )
}

/// Eén rij van de tabel: feit, waarde, herkomst. `note` is vaste tekst van
/// deze crate en mag markup dragen; `value` komt al ontsnapt of is een getal.
fn row(w: &mut Bounded<'_>, fact: &str, value: &dyn fmt::Display, note: &str) -> fmt::Result {
    writeln!(
        w,
        "<tr><td class=\"f\">{fact}</td><td class=\"v\">{value}</td><td class=\"n\">{note}</td></tr>"
    )
}

/// De stijl: die van de Go-welcome, ingekort tot wat deze pagina gebruikt.
/// Een eigen stuk, zodat de accolades van CSS niet door `format!` hoeven.
const STYLE: &str = "<style>
:root{--bg:#0b0f14;--panel:#10161d;--panel-2:#0d1219;--line:#1d2733;--text:#cdd6e0;--muted:#7f8b9b;--copper:#e09a63;--leaf:#56c88b;--phosphor:#46d979;--mono:ui-monospace,\"SF Mono\",Menlo,Consolas,monospace;--sans:system-ui,-apple-system,\"Segoe UI\",Roboto,sans-serif}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:16.5px/1.65 var(--sans);-webkit-font-smoothing:antialiased}
a{color:var(--leaf);text-decoration:none}
a:hover{text-decoration:underline}
.wrap{max-width:1060px;margin:0 auto;padding:0 24px}
.bar{position:sticky;top:0;background:var(--bg);border-bottom:1px solid var(--line)}
.bar-in{display:flex;align-items:center;gap:18px;height:56px;font-family:var(--mono);font-size:13.5px}
.brand{font-weight:700;letter-spacing:.04em;white-space:nowrap}
.brand .ears{color:var(--copper);font-weight:400;margin-right:9px}
.crumb{color:var(--muted);flex:1 1 auto;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.live{margin-left:auto;display:flex;align-items:center;gap:8px;color:var(--phosphor);font-size:12px;letter-spacing:.14em;text-transform:uppercase}
.live i{width:8px;height:8px;border-radius:50%;background:var(--phosphor);box-shadow:0 0 9px var(--phosphor);animation:pulse 2.4s ease-in-out infinite}
@keyframes pulse{50%{opacity:.3}}
@media (prefers-reduced-motion:reduce){.live i{animation:none}}
.hero{padding:60px 0 44px}
.rabbit{margin:0 0 24px;font-family:var(--mono);font-size:14px;line-height:1.5;color:var(--copper)}
.mark{font-family:var(--mono);font-size:12.5px;letter-spacing:.18em;text-transform:uppercase;color:var(--leaf);margin:0 0 16px}
h1{font-family:var(--mono);font-size:clamp(27px,4.2vw,42px);line-height:1.16;margin:0 0 20px}
.lede{font-size:17.5px;color:var(--muted);max-width:35em;margin:0}
.lede strong{color:var(--text)}
.tiles{display:grid;grid-template-columns:repeat(4,1fr);gap:16px;padding-bottom:52px}
@media (max-width:880px){.tiles{grid-template-columns:repeat(2,1fr)}}
@media (max-width:520px){.tiles{grid-template-columns:1fr}}
.tile{border:1px solid var(--line);border-radius:6px;background:var(--panel-2);padding:18px 20px 16px;min-width:0}
.tile .k{display:block;font-family:var(--mono);font-size:11px;letter-spacing:.15em;text-transform:uppercase;color:var(--muted)}
.tile .v{display:block;font-family:var(--mono);font-size:21px;font-weight:700;color:var(--leaf);margin:7px 0 5px;overflow-wrap:anywhere}
.tile .n{display:block;font-size:12.5px;color:var(--muted)}
section{padding:52px 0;border-top:1px solid var(--line)}
h2{font-family:var(--mono);font-size:clamp(19px,2.6vw,25px);margin:0 0 14px}
.section-lede{color:var(--muted);max-width:44em;margin:0 0 30px}
.section-lede strong{color:var(--text)}
.tbl-wrap{overflow-x:auto;border:1px solid var(--line);border-radius:5px}
table{border-collapse:collapse;width:100%;font-size:14.5px}
th{font-family:var(--mono);font-size:11px;text-transform:uppercase;letter-spacing:.1em;color:var(--muted);text-align:left;background:var(--panel-2)}
th,td{padding:12px 18px;border-bottom:1px solid var(--line);vertical-align:top}
tr:last-child td{border-bottom:0}
td.f{color:var(--muted);font-family:var(--mono);font-size:13px;white-space:nowrap}
td.v{font-family:var(--mono);white-space:nowrap;font-variant-numeric:tabular-nums}
td.n{color:var(--muted);font-size:13.5px}
td.n em{color:var(--text);font-style:normal}
@media (max-width:660px){thead{display:none}tbody,tr,td{display:block}tr{border-bottom:1px solid var(--line);padding:13px 16px}th,td{border:0;padding:1px 0;white-space:normal}td.v{overflow-wrap:anywhere}}
footer{padding:46px 0 56px;border-top:1px solid var(--line)}
.foot{display:flex;justify-content:space-between;align-items:flex-start;gap:44px;flex-wrap:wrap}
footer pre{margin:0;font-family:var(--mono);font-size:13px;line-height:1.5;color:var(--copper)}
.foot-links{font-family:var(--mono);font-size:13.5px;display:grid;gap:10px;justify-items:start;color:var(--muted)}
</style>
";

#[cfg(test)]
mod tests {
    use super::*;
    use alloc::string::String;

    fn facts() -> Facts<'static> {
        Facts {
            node: "hopos-qemu",
            slot: 2,
            ip: [10, 100, 0, 3],
            port: 80,
            ram: (64 << 20) - (2 << 20),
            version: "3.0.0",
        }
    }

    fn live(host: &'static str) -> Live<'static> {
        Live {
            host,
            uptime_ns: 3_723_000_000_000,
            requests: 1234,
            heap_used: 150 * 1024,
            heap_peak: 200 * 1024,
            heap_capacity: 60 << 20,
            allocs: 42,
        }
    }

    fn page(host: &'static str) -> String {
        let mut out = Vec::new();
        out.try_reserve_exact(PAGE_CAP).unwrap();
        render(&facts(), &live(host), &mut out).unwrap();
        assert!(out.len() <= PAGE_CAP);
        String::from_utf8(out).unwrap()
    }

    #[test]
    fn the_page_carries_the_bunny_node_slot_and_numbers() {
        let p = page("192.168.1.20");
        assert!(p.contains("( -.-)"), "de bunny");
        assert!(p.contains("(\\(\\"));
        assert!(p.contains("node hopos-qemu &middot; slot 2"));
        assert!(p.contains("1h 02m"), "uptime");
        assert!(p.contains("1,234"), "verzoeken");
        assert!(p.contains("62.0 MiB"), "partitie");
        assert!(p.contains("150.0 KiB"), "heap");
        assert!(p.contains("10.100.0.3:80"));
        assert!(p.contains("192.168.1.20"));
        assert!(p.ends_with("</html>\n"));
    }

    #[test]
    fn outside_text_is_escaped() {
        let p = page("<script>\"x\"&'y'");
        assert!(!p.contains("<script>"));
        assert!(p.contains("&lt;script&gt;&quot;x&quot;&amp;&#39;y&#39;"));
    }

    #[test]
    fn a_page_that_does_not_fit_is_refused_not_grown() {
        let mut out = Vec::new();
        out.try_reserve_exact(1024).unwrap();
        let cap = out.capacity();
        assert!(render(&facts(), &live(""), &mut out).is_err());
        assert_eq!(out.capacity(), cap, "geen hertoewijzing");
    }

    #[test]
    fn numbers_read_like_a_person_writes_them() {
        assert_eq!(Bytes(512).to_string(), "512 B");
        assert_eq!(Bytes(1536).to_string(), "1.5 KiB");
        assert_eq!(Bytes(3 << 30).to_string(), "3.0 GiB");
        assert_eq!(Uptime(42_000_000_000).to_string(), "42s");
        assert_eq!(Uptime(187_000_000_000).to_string(), "3m 07s");
        assert_eq!(Uptime(90_000_000_000_000).to_string(), "1d 01h");
        assert_eq!(Count(999).to_string(), "999");
        assert_eq!(Count(1_000_001).to_string(), "1,000,001");
    }
}

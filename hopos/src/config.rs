//! Van `hopos.cfg` naar de env van Hop: de node-keuzes die de operator op
//! het bootmedium zet, als `KEY=waarde`-regels op de control-page van
//! Hop's slot.
//!
//! In de Go-kern las de kern `hopos.cfg` en gaf de sleutels in-proces aan
//! `agentboot`. Nu is Hop een app; wat hij van de node moet weten, gaat
//! als env-blob mee bij de plaatsing (`CTRL_ENV_DATA`, hoogstens
//! [`CTRL_ENV_MAX`] bytes). De lezer van de tekst is `fw::bootcfg` (één
//! parser voor elk kanaal); dit module kent alleen de sleutels en hun
//! vertaling:
//!
//! | `hopos.cfg` | env van Hop |
//! | --- | --- |
//! | `hopos.node` | `HOPOS_NODE` (anders de board-naam uit [`Facts`]) |
//! | `hopos.cluster` | `HOPOS_CLUSTER` (anders `hopos`) |
//! | `hopos.apikey` | `HOPOS_APIKEY` |
//! | `hopos.insecure=1` | `HOPOS_INSECURE=1` |
//! | `hopos.s3.endpoint`, `.bucket`, `.region`, `.key`, `.secret`, `.pathstyle` | `HOPOS_S3_ENDPOINT` enzovoort |
//! | `hopos.ntp` (`host` of `host:poort`) | `HOPOS_NTP` (anders `pool.ntp.org`) |
//! | `hopos.lock.type` (`hoplockserver` of `s3`), `.url`, `.key`, `.apikey` | `HOPOS_LOCK_TYPE` enzovoort: de lease van het cluster; zonder blijft de node standalone leader |
//! | `hopos.lease_ttl` (seconden, minstens 15) | `HOPOS_LEASE_TTL` |
//! | `hopos.advertise` (`host:poort`) | `HOPOS_ADVERTISE`: het adres dat de andere nodes zien, achter een NAT |
//! | `hopos.init[]` (herhaald) | `HOPOS_INIT_JOBS`: één JSON-array |
//!
//! en uit de kern zelf ([`Facts`]): `HOPOS_NODE_IP`, `HOPOS_PORT`,
//! `HOPOS_CORES`, `HOPOS_MEMORY` en `DNS` (de server uit de lease, voor de
//! resolver van applib).
//!
//! # De init-jobs: env, niet een bestand
//!
//! De keuze (29-09): de init-jobs gaan als `HOPOS_INIT_JOBS` in de env,
//! niet als bestand op hopfs. Een bestand vraagt een hopfs-schrijf vóór de
//! plaatsing, met een pad in het volume van een slot dat nog niet bestaat;
//! de env is er al en gaat atomair mee met de plaatsing. De ruimte is er:
//! de env heeft 3800 bytes, de headless-template één job van ~350, de
//! GUI-template twee van samen ~700. Past het geheel niet, dan valt
//! `HOPOS_INIT_JOBS` weg, luid (`HOPOS_CFG_INIT_TOO_BIG`), en start Hop
//! zonder init-jobs in plaats van helemaal niet: een node zonder basis is
//! bereikbaar en te repareren, een node zonder Hop niet. Hop leest dan
//! `/hop/init-jobs.json` in zijn volume als dat er staat; daar kan een
//! operator (of later de kern) een grotere set neerleggen.
//!
//! # Geheimen
//!
//! `hopos.apikey` en `hopos.s3.secret` staan in de env (Hop heeft ze
//! nodig), maar nooit op de console: [`hop_env`] logt de env met die
//! waarden vervangen door hun lengte.

use abi::hopabi::CTRL_ENV_MAX;
use alloc::string::String;
use core::fmt::{self, Write as _};
use core::net::Ipv4Addr;
use cpu::println;
use fw::bootcfg;

/// De config van QEMU, zolang dat board nog geen `hopos.cfg` leest: wat de
/// kern tot nu toe vast meegaf (een open API, luid, en de naam van de node).
pub(crate) const QEMU_CFG: &str = "hopos.node=hopos-qemu\nhopos.cluster=hopos\nhopos.insecure=1\n";

/// De clusternaam als `hopos.cluster` ontbreekt (Go: `hopos`).
const DEFAULT_CLUSTER: &str = "hopos";

/// De env-sleutels waarvan de waarde nooit op de console komt.
const SECRETS: [&str; 3] = ["HOPOS_APIKEY", "HOPOS_S3_SECRET", "HOPOS_LOCK_APIKEY"];

/// De S3-sleutels: `hopos.cfg` links, de env rechts.
const S3_KEYS: [(&str, &str); 6] = [
    ("hopos.s3.endpoint", "HOPOS_S3_ENDPOINT"),
    ("hopos.s3.bucket", "HOPOS_S3_BUCKET"),
    ("hopos.s3.region", "HOPOS_S3_REGION"),
    ("hopos.s3.key", "HOPOS_S3_KEY"),
    ("hopos.s3.secret", "HOPOS_S3_SECRET"),
    ("hopos.s3.pathstyle", "HOPOS_S3_PATHSTYLE"),
];

/// De cluster van Hop (`agentd_hopos::lock`): de lock, de lease, het adres
/// dat de andere nodes zien als de uplink achter een NAT zit (QEMU slirp), en
/// de tijdserver (een lease is een tijd: een node doet pas mee met een gezette
/// klok, en de kern pint de wandklok op een vaste datum tot SNTP hem zet).
/// `hopos.cfg` links, de env rechts. Zonder `hopos.lock.type` blijft de node
/// standalone leader; de S3-sleutels alleen maken geen cluster, want die zijn
/// ook de object-store van de apps.
const CLUSTER_KEYS: [(&str, &str); 7] = [
    ("hopos.lock.type", "HOPOS_LOCK_TYPE"),
    ("hopos.lock.url", "HOPOS_LOCK_URL"),
    ("hopos.lock.key", "HOPOS_LOCK_KEY"),
    ("hopos.lock.apikey", "HOPOS_LOCK_APIKEY"),
    ("hopos.lease_ttl", "HOPOS_LEASE_TTL"),
    ("hopos.advertise", "HOPOS_ADVERTISE"),
    ("hopos.ntp", "HOPOS_NTP"),
];

/// Wat QEMU uit de bootargs vóór [`QEMU_CFG`] zet, zodat het wint: een
/// tweede node van een cluster heeft een eigen naam, de clusternaam en de
/// sleutel van de rest.
const QEMU_FIRST: [&str; 3] = ["hopos.node", "hopos.cluster", "hopos.apikey"];

/// De config van Hop op QEMU: [`QEMU_FIRST`] (node, cluster, sleutel: die
/// winnen van [`QEMU_CFG`]), dan [`QEMU_CFG`], dan de S3- en cluster-sleutels
/// uit de bootargs (`-append "hopos.s3.endpoint=... hopos.s3.bucket=..."`), want
/// dat board heeft nog geen `hopos.cfg` en de store-ops van de apps lopen
/// via de S3 van Hop (tools/qemu-test-store.sh). Een waarde met een spatie
/// kan niet in een bootarg; daarvoor is er het bestand.
pub(crate) fn qemu_hop_cfg(param: impl Fn(&'static str) -> String) -> String {
    let mut text = String::new();
    for key in QEMU_FIRST {
        let v = param(key);
        if !v.is_empty() {
            let _ = writeln!(text, "{key}={v}");
        }
    }
    text.push_str(QEMU_CFG);
    for (key, _) in S3_KEYS.iter().chain(CLUSTER_KEYS.iter()) {
        let v = param(key);
        if !v.is_empty() {
            let _ = writeln!(text, "{key}={v}");
        }
    }
    text
}

/// De tekst van een `hopos.cfg`, gelezen met `fw::bootcfg`.
#[derive(Copy, Clone, Debug)]
pub(crate) struct NodeCfg<'a> {
    text: &'a str,
}

impl<'a> NodeCfg<'a> {
    /// De config uit de tekst van een configbestand.
    pub(crate) const fn parse(text: &'a str) -> Self {
        Self { text }
    }

    /// De eerste waarde van `key`, of "" (de enkelvoudige sleutel).
    fn one(&self, key: &'a str) -> &'a str {
        bootcfg::first(bootcfg::all(self.text, key))
    }

    /// Alle waarden van `key` (de herhaalde sleutel, `hopos.init[]`).
    fn all(&self, key: &'a str) -> impl Iterator<Item = &'a str> + 'a {
        bootcfg::all(self.text, key)
    }
}

/// Wat de kern zelf over de node weet en Hop meegeeft.
#[derive(Copy, Clone, Debug)]
pub(crate) struct Facts<'a> {
    /// De naam als `hopos.node` ontbreekt (het board, of zijn serienummer).
    pub(crate) default_node: &'a str,
    /// Het uplink-adres uit de lease; zonder lease meldt Hop zijn slot-adres.
    pub(crate) node_ip: Option<Ipv4Addr>,
    /// De DNS-server uit de lease.
    pub(crate) dns: Option<Ipv4Addr>,
    /// De agent-poort; de leader luistert op poort + 1000.
    pub(crate) port: u16,
    /// Alle app-cores van het plan (Hop plant tegen deze min de zijne).
    pub(crate) app_cores: usize,
    /// Het geheugen van de pool (Hop plant tegen dit min het zijne).
    pub(crate) pool_bytes: u64,
    /// De partitie van Hop zelf.
    pub(crate) hop_mem: u64,
}

/// De env-blob van Hop: `KEY=waarde\n`-regels.
#[derive(Clone, Debug, Default)]
pub(crate) struct EnvBlob {
    text: String,
    /// Hoeveel init-jobs in `HOPOS_INIT_JOBS` staan.
    pub(crate) init_jobs: usize,
    /// Hoeveel init-jobs niet pasten en wegvielen.
    pub(crate) init_dropped: usize,
}

impl EnvBlob {
    /// De bytes voor de control-page.
    pub(crate) fn as_bytes(&self) -> &[u8] {
        self.text.as_bytes()
    }

    /// De lengte in bytes.
    pub(crate) fn len(&self) -> usize {
        self.text.len()
    }

    /// De waarde van `key`, als hij erin staat.
    #[cfg_attr(not(test), expect(dead_code, reason = "alleen de tests lezen terug"))]
    pub(crate) fn get(&self, key: &str) -> Option<&str> {
        self.text
            .lines()
            .find_map(|l| l.strip_prefix(key)?.strip_prefix('='))
    }

    /// De env zoals hij op de console mag: geheimen als hun lengte.
    fn redacted(&self) -> String {
        let mut out = String::new();
        for line in self.text.lines() {
            let (k, v) = line.split_once('=').unwrap_or((line, ""));
            let sep = if out.is_empty() { "" } else { " " };
            // Een schrijffout in een String bestaat niet (alleen OOM, en die
            // breekt af); de uitkomst negeren is hier dus geen verlies.
            let _ = if SECRETS.contains(&k) {
                write!(out, "{sep}{k}=<{} bytes>", v.len())
            } else if k == "HOPOS_INIT_JOBS" {
                write!(out, "{sep}{k}=<{} jobs, {} bytes>", self.init_jobs, v.len())
            } else {
                write!(out, "{sep}{line}")
            };
        }
        out
    }
}

/// Waarom er geen env voor Hop is.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub(crate) enum EnvError {
    /// Zonder de init-jobs is de env nog te groot (een absurde waarde in
    /// een andere sleutel).
    TooLarge {
        /// De lengte zonder init-jobs.
        len: usize,
        /// Wat de control-page draagt.
        max: usize,
    },
    /// `hopos.init[]` is geen JSON-object (de regel begint niet met `{` of
    /// eindigt niet met `}`); Hop leest hem daarna streng.
    BadInit {
        /// De plek (vanaf 0).
        index: usize,
    },
}

impl fmt::Display for EnvError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::TooLarge { len, max } => write!(f, "env is {len} bytes, the page holds {max}"),
            Self::BadInit { index } => {
                write!(
                    f,
                    "hopos.init[] entry {index} is not a one-line JSON object"
                )
            }
        }
    }
}

/// De env van Hop uit `cfg` en de feiten van de kern. Logt de env (zonder
/// geheimen) en de keuzes rond de API-sleutel, elk één regel met marker.
pub(crate) fn hop_env(cfg: &NodeCfg<'_>, facts: &Facts<'_>) -> Result<EnvBlob, EnvError> {
    let blob = build(cfg, facts)?;
    let key = !cfg.one("hopos.apikey").is_empty();
    let insecure = cfg.one("hopos.insecure") == "1";
    println!("slots: Hop env: {} HOPOS_HOP_ENV", blob.redacted());
    match (key, insecure) {
        (true, true) => println!(
            "slots: hopos.apikey and hopos.insecure=1 both set; the key wins, remove the insecure line HOPOS_HOP_KEY"
        ),
        (true, false) => println!("slots: Hop API authenticates with hopos.apikey HOPOS_HOP_KEY"),
        (false, true) => println!(
            "slots: Hop runs with HOPOS_INSECURE=1: API authentication is OFF HOPOS_HOP_INSECURE"
        ),
        (false, false) => println!(
            "slots: no hopos.apikey and no hopos.insecure=1: Hop will refuse its API HOPOS_HOP_NO_AUTH"
        ),
    }
    if blob.init_dropped > 0 {
        println!(
            "slots: {} init job(s) do not fit the {CTRL_ENV_MAX}-byte env; Hop starts without them (put them in /hop/init-jobs.json) HOPOS_CFG_INIT_TOO_BIG",
            blob.init_dropped
        );
    }
    Ok(blob)
}

/// De console over TCP (conport.rs, net.rs): `hopos.console=1` zet hem
/// aan, `=0` uit; zonder die sleutel volgt hij `hopos.insecure=1` (de
/// testbank op het eigen LAN). Go: geen sleutel is geen poort, maar elke
/// bankconfig had hem; hier is de bank de insecure-vlag. De poort geeft
/// elke lezer de hele console, dus nooit stil aan op een node met een
/// sleutel. Uit de config van Hop, anders uit de bootargs (`param`); main
/// beslist vóór het net, want de listener start zodra de lease er is, ook
/// na een flip waarin Hop niet opnieuw wordt geplaatst.
pub(crate) fn console_enabled(cfg: &NodeCfg<'_>, param: impl Fn(&'static str) -> String) -> bool {
    let key = |k: &'static str| -> String {
        let v = cfg.one(k);
        if v.is_empty() {
            param(k)
        } else {
            String::from(v)
        }
    };
    match key("hopos.console").as_str() {
        "1" | "on" => true,
        "0" | "off" => false,
        _ => key("hopos.insecure") == "1",
    }
}

/// Bouwt de env zonder te loggen: eerst alles behalve de init-jobs, dan
/// de init-jobs als het geheel nog past.
fn build(cfg: &NodeCfg<'_>, facts: &Facts<'_>) -> Result<EnvBlob, EnvError> {
    let max = usize::try_from(CTRL_ENV_MAX).unwrap_or(usize::MAX);
    let mut text = String::new();
    base(&mut text, cfg, facts).map_err(|_| EnvError::TooLarge { len: 0, max })?;
    if text.len() > max {
        return Err(EnvError::TooLarge {
            len: text.len(),
            max,
        });
    }
    let mut jobs = String::new();
    let mut count = 0;
    for (index, spec) in cfg.all("hopos.init[]").enumerate() {
        if !(spec.starts_with('{') && spec.ends_with('}')) {
            return Err(EnvError::BadInit { index });
        }
        jobs.push(if count == 0 { '[' } else { ',' });
        jobs.push_str(spec);
        count += 1;
    }
    let mut blob = EnvBlob {
        text,
        init_jobs: 0,
        init_dropped: 0,
    };
    if count == 0 {
        return Ok(blob);
    }
    jobs.push(']');
    // "HOPOS_INIT_JOBS=" plus de array plus de regeleinde.
    let line = "HOPOS_INIT_JOBS=".len() + jobs.len() + 1;
    if blob.text.len() + line <= max {
        blob.text.push_str("HOPOS_INIT_JOBS=");
        blob.text.push_str(&jobs);
        blob.text.push('\n');
        blob.init_jobs = count;
    } else {
        blob.init_dropped = count;
    }
    Ok(blob)
}

/// De sleutels behalve de init-jobs.
fn base(out: &mut String, cfg: &NodeCfg<'_>, f: &Facts<'_>) -> fmt::Result {
    let node = match cfg.one("hopos.node") {
        "" => f.default_node,
        n => n,
    };
    let cluster = match cfg.one("hopos.cluster") {
        "" => DEFAULT_CLUSTER,
        c => c,
    };
    writeln!(out, "HOPOS_NODE={node}")?;
    writeln!(out, "HOPOS_CLUSTER={cluster}")?;
    let key = cfg.one("hopos.apikey");
    if !key.is_empty() {
        writeln!(out, "HOPOS_APIKEY={key}")?;
    }
    // Het vlag gaat mee zoals hij er staat, ook naast een sleutel: Hop
    // beslist (de sleutel wint) en zegt het zelf.
    if cfg.one("hopos.insecure") == "1" {
        writeln!(out, "HOPOS_INSECURE=1")?;
    }
    for (from, to) in S3_KEYS.iter().chain(CLUSTER_KEYS.iter()) {
        let v = cfg.one(from);
        if !v.is_empty() {
            writeln!(out, "{to}={v}")?;
        }
    }
    if let Some(ip) = f.node_ip {
        writeln!(out, "HOPOS_NODE_IP={ip}")?;
    }
    if let Some(dns) = f.dns {
        writeln!(out, "DNS={dns}")?;
    }
    writeln!(out, "HOPOS_PORT={}", f.port)?;
    // Hop plant tegen de cores die hij kan uitdelen: alle app-cores min de
    // zijne (hij deelt zijn core niet met jobs, alleen met zijn groep).
    writeln!(out, "HOPOS_CORES={}", f.app_cores.saturating_sub(1).max(1))?;
    writeln!(
        out,
        "HOPOS_MEMORY={}",
        f.pool_bytes.saturating_sub(f.hop_mem)
    )?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    const FACTS: Facts<'static> = Facts {
        default_node: "hopos-qemu",
        node_ip: Some(Ipv4Addr::new(10, 0, 2, 15)),
        dns: Some(Ipv4Addr::new(10, 0, 2, 3)),
        port: 8080,
        app_cores: 3,
        pool_bytes: 512 << 20,
        hop_mem: 64 << 20,
    };

    #[test]
    fn the_console_port_follows_the_key_and_then_the_insecure_flag() {
        let none = |_: &'static str| String::new();
        assert!(console_enabled(&NodeCfg::parse("hopos.insecure=1\n"), none));
        assert!(!console_enabled(
            &NodeCfg::parse("hopos.console=0\nhopos.insecure=1\n"),
            none
        ));
        assert!(console_enabled(&NodeCfg::parse("hopos.console=on\n"), none));
        assert!(!console_enabled(&NodeCfg::parse(""), none));
        // De bootargs vullen aan wat de config niet zegt.
        assert!(console_enabled(&NodeCfg::parse(""), |k| String::from(
            if k == "hopos.console" { "1" } else { "" }
        )));
        assert!(!console_enabled(
            &NodeCfg::parse("hopos.console=off\n"),
            |_| String::from("1")
        ));
    }

    #[test]
    fn qemu_bootargs_add_the_s3_keys_and_nothing_else() {
        let text = qemu_hop_cfg(|k| match k {
            "hopos.s3.endpoint" => String::from("http://10.0.2.2:9000"),
            "hopos.s3.bucket" => String::from("hop"),
            "hopos.s3.secret" => String::from("geheim"),
            "hopos.ntp" => String::from("10.0.2.2:10123"),
            _ => String::new(),
        });
        let b = build(&NodeCfg::parse(&text), &FACTS).unwrap();
        assert!(b.text.contains("HOPOS_S3_ENDPOINT=http://10.0.2.2:9000\n"));
        assert!(b.text.contains("HOPOS_S3_BUCKET=hop\n"));
        assert!(!b.text.contains("HOPOS_S3_REGION"));
        assert!(b.text.contains("HOPOS_NTP=10.0.2.2:10123\n"));
        assert!(b.text.contains("HOPOS_INSECURE=1\n"), "QEMU_CFG stays");
        assert!(!b.redacted().contains("geheim"));
        assert_eq!(qemu_hop_cfg(|_| String::new()), QEMU_CFG);
    }

    #[test]
    fn qemu_default_is_what_the_kernel_gave_before() {
        let b = build(&NodeCfg::parse(QEMU_CFG), &FACTS).unwrap();
        assert_eq!(
            core::str::from_utf8(b.as_bytes()).unwrap(),
            "HOPOS_NODE=hopos-qemu\nHOPOS_CLUSTER=hopos\nHOPOS_INSECURE=1\n\
             HOPOS_NODE_IP=10.0.2.15\nDNS=10.0.2.3\nHOPOS_PORT=8080\nHOPOS_CORES=2\n\
             HOPOS_MEMORY=469762048\n"
        );
    }

    #[test]
    fn the_template_keys_arrive_and_secrets_stay_off_the_console() {
        let cfg = "# node\nhopos.node=hop-1\nhopos.cluster=hoplab\nhopos.apikey=s3cr3tkey\n\
            # hopos.insecure=1\nhopos.s3.endpoint=https://s3.example.com\nhopos.s3.bucket=hop-prod\n\
            hopos.s3.key=AKIA\nhopos.s3.secret=very-secret\nhopos.s3.pathstyle=1\n\
            hopos.init[]={\"name\":\"welcome\",\"artifacts\":[{\"url\":\"https://x/y.elf\"}]}\n\
            hopos.init[]={\"name\":\"two\", \"cmd\":\"met spaties\"}\n";
        let b = build(&NodeCfg::parse(cfg), &FACTS).unwrap();
        assert_eq!(b.get("HOPOS_NODE"), Some("hop-1"));
        assert_eq!(b.get("HOPOS_CLUSTER"), Some("hoplab"));
        assert_eq!(b.get("HOPOS_APIKEY"), Some("s3cr3tkey"));
        assert_eq!(b.get("HOPOS_INSECURE"), None, "commentaar is geen config");
        assert_eq!(b.get("HOPOS_S3_ENDPOINT"), Some("https://s3.example.com"));
        assert_eq!(b.get("HOPOS_S3_SECRET"), Some("very-secret"));
        assert_eq!(b.get("HOPOS_S3_PATHSTYLE"), Some("1"));
        assert_eq!(b.get("HOPOS_S3_REGION"), None);
        assert_eq!(
            b.get("HOPOS_INIT_JOBS"),
            Some(
                r#"[{"name":"welcome","artifacts":[{"url":"https://x/y.elf"}]},{"name":"two", "cmd":"met spaties"}]"#
            )
        );
        assert_eq!((b.init_jobs, b.init_dropped), (2, 0));
        let shown = b.redacted();
        assert!(
            !shown.contains("s3cr3tkey") && !shown.contains("very-secret"),
            "{shown}"
        );
        assert!(shown.contains("HOPOS_APIKEY=<9 bytes>"), "{shown}");
        assert!(shown.contains("HOPOS_INIT_JOBS=<2 jobs,"), "{shown}");
        assert!(shown.contains("HOPOS_S3_KEY=AKIA"), "{shown}");
    }

    #[test]
    fn init_jobs_that_do_not_fit_are_dropped_not_hop() {
        let job = alloc::format!(
            "{{\"name\":\"big\",\"env\":{{\"X\":\"{}\"}}}}",
            "x".repeat(2000)
        );
        let cfg = alloc::format!("hopos.insecure=1\nhopos.init[]={job}\nhopos.init[]={job}\n");
        let b = build(&NodeCfg::parse(&cfg), &FACTS).unwrap();
        assert_eq!((b.init_jobs, b.init_dropped), (0, 2));
        assert_eq!(b.get("HOPOS_INIT_JOBS"), None);
        assert!(b.len() as u64 <= CTRL_ENV_MAX);
        assert_eq!(b.get("HOPOS_INSECURE"), Some("1"));
    }

    #[test]
    fn a_broken_init_line_and_an_absurd_value_are_refused() {
        let b = build(&NodeCfg::parse("hopos.init[]=name=welcome\n"), &FACTS);
        assert_eq!(b.unwrap_err(), EnvError::BadInit { index: 0 });
        let huge = alloc::format!("hopos.node={}\n", "n".repeat(4000));
        assert!(matches!(
            build(&NodeCfg::parse(&huge), &FACTS),
            Err(EnvError::TooLarge { .. })
        ));
    }

    #[test]
    fn no_node_name_takes_the_board_name() {
        let b = build(&NodeCfg::parse(""), &FACTS).unwrap();
        assert_eq!(b.get("HOPOS_NODE"), Some("hopos-qemu"));
        assert_eq!(b.get("HOPOS_CLUSTER"), Some("hopos"));
        assert_eq!(b.get("HOPOS_INSECURE"), None);
    }
}

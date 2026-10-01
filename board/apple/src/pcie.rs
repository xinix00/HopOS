//! De PCIe-kant van de Mac mini: de controller zelf opbrengen, de link van
//! de ethernet-poort, de DART in bypass, en de brug en het endpoint
//! configureren. Wat op elk ander HopOS-board de firmware al gedaan had.
//!
//! De keten (Go `apcie.go` en `pcie.go`, BEWEZEN 29-08 in een chain-boot:
//! 263 tunables, 2 van 3 poorten, endpoint 02:00.0 14e4:1682, tg3 LINK UP):
//!
//! 1. power-domein aan ([`crate::pmgr`]), tunables op AXI, RC en PHY;
//! 2. de PHY-klokken aanvragen, de PHY uit reset, de gedeelde referentieklok
//!    en het root complex starten;
//! 3. per poort de vaste schrijfacties van m1n1's T8122-tak, de poort-PHY,
//!    de poort uit reset, en de DesignWare-kern (snelheid, breedte);
//! 4. dan de link van poort 2 ([`link_up`]): PERST# loopt op dit silicium
//!    over een AP-GPIO (ADT `function-perst` = GPIO 165) en de training
//!    moet expliciet gestart (PORT_LTSSMCTL); dat is van Linux'
//!    `pcie-apple.c`, niet van m1n1, en met die twee is de link binnen 50 ms
//!    up;
//! 5. de DART van de poort in bypass (DART én DAPF: half bypassen is niet
//!    bypassen) en de RID aan stream 0, zodat een DMA-adres een fysiek adres
//!    is. Dezelfde vertrouwensafspraak als op elk HopOS-board: de NIC is van
//!    de kern, apps praten via de interne switch;
//! 6. busnummers en het prefetch-venster van de brug, en pas dán de BAR's.
//!    Zonder dat venster staat de link, is het endpoint zichtbaar, en leest
//!    élk BAR-adres all-ones (29-08).
//!
//! Meevaller: t8132 heeft geen fuse-programmering (`fuse_bits = NULL` vanaf
//! t8122); dat was de enige tabel die niet uit de boom te lezen was. Alle
//! vaste offsets komen letterlijk uit m1n1 `src/pcie.c`, tak
//! `APCIE_T8122` met type `APCIE_T8132`: ongedocumenteerd, dus ze staan er
//! zoals ze daar staan, in dezelfde volgorde.
//!
//! Op deze bus is een externe abort UITGESTELD: hij landt bij de
//! eerstvolgende toegang, niet bij de schrijf die hem veroorzaakte. Daarom
//! meldt elke stap zich vóór hij hem doet ([`step`]), met een barrière
//! (29-08: één verkeerd masker, `PCIE_LNKCAP2_SLS` van 7 in plaats van 6
//! bits, nam het hele blok onderuit, en pas de tik na elke schrijf wees het
//! aan).

use crate::fwinfo;
use crate::pmgr::{self, Pmgr};
use cpu::idle::now;
use cpu::println;
use dev::Pa;
use driver_pcie::{Bdf, Config, Ecam, Function, MmioWindow};
use fw::adt::{Adt, Node};

/// De ECAM van `/arm-io/apcie` (reg[0]; 32 KB per poort op bus 0).
pub const ECAM: u64 = 0x1c_b000_0000;
/// Het 64-bit prefetch-venster, 512 MB, PCI-adres == CPU-adres (de
/// ADT-ranges: het 32-bit venster vertaalt, dit niet).
pub const MMIO: u64 = 0xb_c000_0000;
/// De maat van dat venster.
pub const MMIO_SIZE: u64 = 0x2000_0000;
/// De ethernet-poort: device 2 op bus 0 (`pci-bridge2`, `lan-1gb`).
pub const ETH_PORT: u8 = 2;
/// Het busnummer achter de brug.
pub const ETH_BUS: u8 = 2;
/// Het poortregisterblok (reg[7 + 8 * 2] van de apcie-node, GEMETEN).
const ETH_PORT_BASE: u64 = 0x4_9202_8000;
/// De PERST#-GPIO van de poort (ADT `function-perst`).
const ETH_PERST_PIN: u64 = 165;
/// De DART van de poort (`dart,t8110`).
const ETH_DART: u64 = 0x4_9200_0000;
/// `/arm-io/gpio0` (`gpio,t8101`): één register per pin; DATA bit 0, MODE
/// 3:1. Alleen de onderste vier bits aanraken: de rest is de pinconfig van
/// iBoot, en die wissen maakt de pin dood (GEMETEN: 0x74a02 → 0x2).
const GPIO_BASE: u64 = 0x3_9a00_0000;

// De poortregisters (Linux `pcie-apple.c`; m1n1 dezelfde).
const PORT_LTSSMCTL: u64 = 0x080;
const PORT_LINKSTS: u64 = 0x208;
const PORT_APPCLK: u64 = 0x800;
const PORT_STATUS: u64 = 0x804;
const PORT_PERST: u64 = 0x814;
/// Op deze generatie is DIT de poort-reset, niet 0x814.
const PORT_RESET_T602X: u64 = 0x82c;
/// RID → DART-stream op t602x/t8132 (niet de 0x828 van de oudere chips);
/// m1n1 nult hem en zet er nooit iets in.
const PORT_RID2SID: u64 = 0x3000;
const PORT_MSIMAP: u64 = 0x3800;
const RID2SID_VALID: u32 = 1 << 31;

// DART (Linux `apple-dart.c`): per stream een TCR. Bit 2 (DAPF-bypass) is
// op deze DART niet schrijfbaar (GEMETEN: 0x6 leest 0x2), maar m1n1 zet
// beide, en DMA op 1 TiB+ loopt.
const DART_TCR: u64 = 0x1000;
const DART_BYPASS: u32 = (1 << 1) | (1 << 2);
const DART_STREAMS: u64 = 16;

// De apcie-node van t8132 (m1n1 `regs_t8132`).
const IDX_CONFIG: usize = 0;
const IDX_RC: usize = 1;
const IDX_PHY: usize = 2;
const IDX_PHY_IP: usize = 3;
const IDX_AXI: usize = 4;
const SHARED_REGS: usize = 7;
const PHY_OFF: u64 = 0x8000;
const PHY_CMN_OFF: u64 = 0x4000;

const PHY_CTRL: u64 = 0x000;
const PHY_CLK0REQ: u32 = 1 << 0;
const PHY_CLK1REQ: u32 = 1 << 1;
const PHY_CLK0ACK: u32 = 1 << 2;
const PHY_CLK1ACK: u32 = 1 << 3;
const PHY_RESET_T8132: u32 = 1 << 4;

// De DesignWare-kern in de config-space van de poort.
const DBI_RO_WR: u64 = 0x8bc;
const PORT_LINK_CTL: u64 = 0x710;
const PORT_LINK_MODE: u32 = 0x3f << 16;
const LINK_WIDTH_CTL: u64 = 0x80c;
const LINK_WIDTH: u32 = 0x1f << 8;
const SPEED_CHANGE: u32 = 1 << 17;
const CAP: u64 = 0x70;
const LNKCAP: u64 = 0x0c;
const LNKCAP_SLS: u32 = 0xf;
const LNKCAP_MLW: u32 = 0x3f << 4;
const LNKCAP2: u64 = 0x2c;
/// GENMASK(6,1): ZES bits. Zeven nam bit 7 (Crosslink) mee en daarmee het
/// hele registerblok (29-08).
const LNKCAP2_SLS: u32 = 0x3f << 1;
const LNKCTL2: u64 = 0x30;
const LNKCTL2_TLS: u16 = 0xf;

const _: () = {
    assert!(LNKCAP2_SLS == 0b0111_1110);
    assert!(PORT_RID2SID < PORT_MSIMAP);
    assert!(MMIO.is_multiple_of(1 << 20) && MMIO_SIZE.is_power_of_two());
};

const MS: u64 = 1_000_000;

fn rmw(a: u64, clear: u32, set: u32) {
    let a = Pa(a);
    dev::write32(a, (dev::read32(a) & !clear) | set);
    dev::mb();
}

fn poll(a: u64, mask: u32, want: u32, ns: u64) -> bool {
    let t0 = now();
    loop {
        if dev::read32(Pa(a)) & mask == want {
            return true;
        }
        if now().saturating_sub(t0) > ns {
            return false;
        }
    }
}

fn sleep_ns(ns: u64) {
    let t0 = now();
    while now().saturating_sub(t0) < ns {
        core::hint::spin_loop();
    }
}

/// Meldt de volgende stap en zet een barrière, zodat een uitgestelde abort
/// bij de stap landt die hem veroorzaakte.
fn step(args: core::fmt::Arguments<'_>) {
    dev::mb();
    println!("{args}");
}

/// Waarom de PCIe-kant niet opkwam.
pub type Why = &'static str;

/// Brengt de controller en zijn poorten op. Draait hij al (m1n1 deed het
/// onder de loader), dan blijft alles staan: de bring-up twee keer doen zou
/// een werkende link neerhalen. Geeft de bootregel.
pub fn init() -> Result<&'static str, Why> {
    let root = dev::read32(Pa(ECAM));
    if root != u32::MAX && root != 0 {
        return Ok("apcie: already up, left as the boot loader set it");
    }
    let t = fwinfo::adt().ok_or("no device tree")?;
    let chain = t.trace("/arm-io/apcie").ok_or("no /arm-io/apcie")?;
    let node = chain.node();
    let reg = |i: usize| t.reg_at(&chain, i).map_or(0, |r| r.0);
    let (config, rc, phy_ip, axi) = (reg(IDX_CONFIG), reg(IDX_RC), reg(IDX_PHY_IP), reg(IDX_AXI));
    let phy_blk = reg(IDX_PHY);
    if config == 0 || rc == 0 || phy_blk == 0 || phy_ip == 0 || axi == 0 {
        return Err("incomplete reg set on /arm-io/apcie");
    }
    let (phy, phy_cmn) = (phy_blk + PHY_OFF, phy_blk + PHY_CMN_OFF);
    let ports = t.u32(node, "#ports").unwrap_or(0) as usize;
    let nregs = t.prop(node, "reg").map_or(0, |r| r.len() / 16);
    if ports == 0 || nregs <= SHARED_REGS {
        return Err("apcie: ports and reg windows do not divide");
    }
    let per_port = (nregs - SHARED_REGS) / ports;
    step(format_args!(
        "apcie: config {config:#x} rc {rc:#x} phy {phy:#x} phy-ip {phy_ip:#x} axi {axi:#x}, {ports} ports, {per_port} reg/port"
    ));

    // 1. Stroom. Zonder dit antwoordt geen register hieronder.
    let gates = Pmgr::open(t).map_or(0, |p| p.power_on(node));
    step(format_args!("apcie: {gates} power gate(s) enabled"));
    let mut tuned = 0;
    let mut apply = |n: Node, prop: &str, base: u64| {
        let c = pmgr::tunables(&t, n, prop, base);
        tuned += c.unwrap_or(0);
        step(format_args!("apcie:   {prop} at {base:#x}: {c:?} line(s)"));
    };

    // 2. De gedeelde blokken.
    apply(node, "apcie-axi2af-tunables", axi);
    dev::write32(Pa(rc + 0x4), 0); // m1n1 zet dit zonder toelichting; wij ook.
    dev::mb();
    apply(node, "apcie-common-tunables", rc);
    apply(node, "apcie-phy-tunables", phy_blk);

    // 3. De PHY: twee klokken, dan uit reset.
    phy_clocks(phy)?;
    rmw(phy + PHY_CTRL, PHY_RESET_T8132, 0);
    sleep_ns(MS);
    rmw(phy + 4, 0, 0x01);
    apply(node, "apcie-phy-ip-pll-tunables", phy_ip);
    apply(node, "apcie-phy-ip-auspma-tunables", phy_ip);
    rmw(phy + 4, 0, 0x10);

    // 4. De referentieklok en het root complex.
    rmw(phy_cmn, 0b11, 1);
    if !poll(phy + 0x8, 1, 1, 250 * MS) {
        return Err("apcie: PHY clock did not start");
    }
    rmw(phy + PHY_CTRL, 0, 0x200);
    dev::write32(Pa(rc + 0x54), 0x140);
    dev::write32(Pa(rc + 0x50), 0x1);
    dev::mb();
    if !poll(rc + 0x58, 1, 1, 250 * MS) {
        return Err("apcie: root complex did not start");
    }

    // 5. De poorten. Het config-venster van poort N is dat van device N op
    // bus 0, RECHTSTREEKS uit het poortnummer (m1n1 schuift een teller mee
    // die op deze machine, zonder pci-bridge1, poort 2 in de config-space
    // van device 1 schreef; onschadelijk gebleken, niet bedoeld).
    let mut up = 0;
    for port in 0..ports {
        let Some(bridge) = t.child(node, bridge_name(port)) else {
            continue;
        };
        let i = port * per_port + SHARED_REGS;
        let (pb, pphy) = (reg(i), reg(i + 2));
        if pb == 0 || pphy == 0 {
            return Err("apcie: a port has no register windows");
        }
        let cfg = config + ((port as u64) << 15);
        step(format_args!(
            "apcie: port {port} base {pb:#x} phy {pphy:#x} config {cfg:#x}"
        ));
        init_port(&t, bridge, pb, pphy, cfg, &mut apply)?;
        up += 1;
    }
    println!(
        "apcie: brought up by HopOS, {gates} power gate(s), {tuned} tunable(s), {up} of {ports} port(s)"
    );
    Ok("apcie: brought up by HopOS")
}

/// De naam van de brug van poort `p` ("pci-bridge0" tot en met 7).
fn bridge_name(p: usize) -> &'static str {
    const NAMES: [&str; 8] = [
        "pci-bridge0",
        "pci-bridge1",
        "pci-bridge2",
        "pci-bridge3",
        "pci-bridge4",
        "pci-bridge5",
        "pci-bridge6",
        "pci-bridge7",
    ];
    NAMES.get(p).copied().unwrap_or("")
}

/// Vraagt de twee PHY-klokken aan en wacht op elke bevestiging.
fn phy_clocks(phy: u64) -> Result<(), Why> {
    rmw(phy + PHY_CTRL, 0, PHY_CLK0REQ);
    if !poll(phy + PHY_CTRL, PHY_CLK0ACK, PHY_CLK0ACK, 50 * MS) {
        return Err("apcie: PHY CLK0 not acknowledged");
    }
    rmw(phy + PHY_CTRL, 0, PHY_CLK1REQ);
    if !poll(phy + PHY_CTRL, PHY_CLK1ACK, PHY_CLK1ACK, 50 * MS) {
        return Err("apcie: PHY CLK1 not acknowledged");
    }
    Ok(())
}

/// De bring-up van één poort, in de volgorde van m1n1's T8122-tak.
fn init_port(
    t: &Adt<'_>,
    bridge: Node,
    pb: u64,
    pphy: u64,
    cfg: u64,
    apply: &mut impl FnMut(Node, &str, u64),
) -> Result<(), Why> {
    const FIRST: [(u64, u32); 13] = [
        (0x088, 0x110),
        (0x100, 0xffff_ffff),
        (0x148, 0xffff_ffff),
        (0x210, 0xffff_ffff),
        (0x080, 0),
        (0x084, 0),
        (0x104, 0xffff_fff0),
        (0x124, 0x100),
        (0x16c, 0),
        (0x13c, 0x10),
        (0x800, 0x10_0100),
        (0x808, 0x10_00ff),
        (0x82c, 0),
    ];
    const THEN: [(u64, u32); 6] = [
        (0x130, 0x300_0000),
        (0x140, 0x10),
        (0x144, 0x25_3770),
        (0x21c, 0),
        (0x834, 0),
        (0x83c, 0),
    ];
    for (off, v) in FIRST {
        dev::write32(Pa(pb + off), v);
    }
    for i in 0..16 {
        dev::write32(Pa(pb + PORT_RID2SID + 4 * i), 0);
    }
    for i in 0..512 {
        dev::write32(Pa(pb + PORT_MSIMAP + 4 * i), 0);
    }
    for (off, v) in THEN {
        dev::write32(Pa(pb + off), v);
    }
    dev::mb();
    apply(bridge, "apcie-config-tunables", pb);
    rmw(pb + PORT_APPCLK, 0, 1);

    rmw(pphy + PHY_CTRL, PHY_CLK0REQ | PHY_CLK1REQ, 0);
    phy_clocks(pphy)?;
    rmw(pphy + PHY_CTRL, 0x10, 0);
    rmw(pphy + PHY_CTRL, 0, 0x200);
    rmw(pphy + PHY_CTRL, 0, 0x400);

    rmw(pb + PORT_RESET_T602X, 0, 1);
    if !poll(pb + PORT_STATUS, 1, 1, 250 * MS) {
        return Err("apcie: port did not come up");
    }
    if !poll(pb + PORT_LINKSTS, 1 << 2, 0, 250 * MS) {
        return Err("apcie: port stayed busy");
    }
    let touch = |what: &str| {
        dev::mb();
        println!(
            "apcie:   {what:<22} LINKSTS {:#x}",
            dev::read32(Pa(pb + PORT_LINKSTS))
        );
    };
    touch("port ready");

    // De DesignWare-kern: alleen-lezen-registers even open, de tunables van
    // de brug erin, snelheid en breedte vast, dicht.
    rmw(cfg + DBI_RO_WR, 0, 1);
    touch("DBI open");
    apply(bridge, "pcie-rc-tunables", cfg);
    apply(bridge, "pcie-rc-gen3-shadow-tunables", cfg);
    apply(bridge, "pcie-rc-gen4-shadow-tunables", cfg);
    touch("rc tunables");
    let speed = max_link_speed(t, bridge);
    if speed > 0 {
        rmw(cfg + CAP + LNKCAP, LNKCAP_SLS, speed);
        rmw(cfg + CAP + LNKCAP2, LNKCAP2_SLS, ((1 << speed) - 1) << 1);
        let a = Pa(cfg + CAP + LNKCTL2);
        dev::write16(
            a,
            (dev::read16(a) & !LNKCTL2_TLS) | (speed as u16 & LNKCTL2_TLS),
        );
        touch("speed");
        rmw(cfg + LINK_WIDTH_CTL, 0, SPEED_CHANGE);
        touch("speed change");
    }
    rmw(cfg + PORT_LINK_CTL, PORT_LINK_MODE, 1 << 16);
    touch("lane mode");
    rmw(cfg + LINK_WIDTH_CTL, LINK_WIDTH, 1 << 8);
    touch("link width");
    rmw(cfg + CAP + LNKCAP, LNKCAP_MLW, 1 << 4);
    touch("LNKCAP width");
    rmw(cfg + DBI_RO_WR, 1, 0);
    touch("DBI closed");
    Ok(())
}

/// De maximale linksnelheid van een brug. Staat er 1, dan mag het eerste
/// kind hem verhogen (`target-link-speed` of `expected-link-speed`, m1n1).
fn max_link_speed(t: &Adt<'_>, bridge: Node) -> u32 {
    let speed = t.u32(bridge, "maximum-link-speed").unwrap_or(0);
    if speed != 1 {
        return speed.min(4);
    }
    t.children(bridge)
        .next()
        .and_then(|c| {
            t.u32(c, "target-link-speed")
                .filter(|&v| v > 0)
                .or_else(|| t.u32(c, "expected-link-speed"))
        })
        .unwrap_or(speed)
        .min(4)
}

/// Zet een GPIO als uitgang op `v` (alleen DATA en MODE).
fn gpio_set(pin: u64, v: bool) {
    let a = GPIO_BASE + 4 * pin;
    rmw(a, 0xf, (1 << 1) | u32::from(v));
}

/// Brengt de link van de ethernet-poort op: referentieklok, PERST# over de
/// GPIO, de register-PERST, en de training. Tijden uit de ADT
/// (`t-refclk-to-perst`, `perst-to-config`: beide 100 ms).
pub fn link_up(timeout_ns: u64) -> Result<(), Why> {
    let b = ETH_PORT_BASE;
    if dev::read32(Pa(b + PORT_LINKSTS)) & 1 != 0 {
        return Ok(());
    }
    rmw(b + PORT_APPCLK, 0, 1);
    gpio_set(ETH_PERST_PIN, false);
    sleep_ns(10 * MS);
    rmw(b + PORT_PERST, 0, 1);
    gpio_set(ETH_PERST_PIN, true);
    sleep_ns(100 * MS);
    if dev::read32(Pa(b + PORT_STATUS)) & 1 == 0 {
        return Err("apcie: ethernet port not ready");
    }
    dev::write32(Pa(b + PORT_LTSSMCTL), 1);
    dev::mb();
    if !poll(b + PORT_LINKSTS, 1, 1, timeout_ns) {
        return Err("apcie: link did not come up");
    }
    Ok(())
}

/// Alle streams van de ethernet-DART op bypass: welke stream de poort aan
/// de RID van de NIC hangt, is niet gegarandeerd, en bypass op een
/// ongebruikte stream kost niets.
fn dart_bypass() {
    for sid in 0..DART_STREAMS {
        dev::write32(Pa(ETH_DART + DART_TCR + 4 * sid), DART_BYPASS);
    }
    dev::mb();
}

/// Koppelt RID `bdf` aan DART-stream 0 op plek `idx`.
fn map_rid(idx: u64, bdf: Bdf) {
    let v =
        RID2SID_VALID | (u32::from(bdf.bus) << 8) | (u32::from(bdf.dev) << 3) | u32::from(bdf.func);
    dev::write32(Pa(ETH_PORT_BASE + PORT_RID2SID + 4 * idx), v);
    dev::mb();
}

/// De config-space van de APCIe (bussen 0 tot en met [`ETH_BUS`]).
#[must_use]
pub fn ecam() -> Ecam {
    // SAFETY: de ECAM van `/arm-io/apcie` (reg[0], 256 MB), onder 512 GB en
    // dus Device-gemapt (`mmu::build`); na [`init`] antwoordt hij.
    unsafe { Ecam::new(Pa(ECAM), 0, ETH_BUS) }
}

/// Wat de enumeratie opleverde: het endpoint, zijn BAR0 en zijn
/// config-space.
pub struct Endpoint {
    /// De functie.
    pub f: Function,
    /// BAR0 (CPU-adres).
    pub bar0: u64,
    /// De ECAM-config-space van de functie (4 KB).
    pub cfg: u64,
}

/// Link op, DART in bypass, brug geconfigureerd, BAR's toegewezen: het
/// endpoint achter de ethernet-poort.
pub fn enumerate_nic() -> Result<Endpoint, Why> {
    link_up(500 * MS)?;
    dart_bypass();
    let e = ecam();
    let br = Bdf::new(0, ETH_PORT, 0).ok_or("bad bridge address")?;
    // Busnummers: primary 0, secondary en subordinate ETH_BUS.
    let bus = e.read32(br, 0x18);
    e.write32(
        br,
        0x18,
        (bus & !0x00ff_ffff) | (u32::from(ETH_BUS) << 8) | (u32::from(ETH_BUS) << 16),
    );
    // Het prefetch-venster (64-bit) en het niet-prefetchbare venster uit.
    let end = MMIO + MMIO_SIZE - 1;
    e.write32(
        br,
        0x24,
        ((((end >> 20) & 0xfff) as u32) << 20)
            | (1 << 16)
            | ((((MMIO >> 20) & 0xfff) as u32) << 4)
            | 1,
    );
    e.write32(br, 0x28, (MMIO >> 32) as u32);
    e.write32(br, 0x2c, (end >> 32) as u32);
    e.write32(br, 0x20, 0x0000_fff0);
    dev::mb();
    sleep_ns(10 * MS);
    let f = driver_pcie::find(&e, ETH_BUS, |f| !f.is_bridge()).ok_or("link up but no endpoint")?;
    let mut win = MmioWindow::new(MMIO, MMIO_SIZE);
    let bars = f
        .assign_bars(&e, &mut win)
        .map_err(|_| "BAR assignment failed")?;
    let bar0 = bars
        .first()
        .and_then(driver_pcie::Bar::mem)
        .ok_or("no BAR0")?
        .0;
    map_rid(0, f.bdf);
    f.enable(&e);
    // De brug: memory-decode en bus-master.
    e.write32(br, 0x04, e.read32(br, 0x04) | 0x6);
    dev::mb();
    let cfg = ECAM + f.bdf.ecam_offset();
    Ok(Endpoint { f, bar0, cfg })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bridge_names_and_masks() {
        assert_eq!(bridge_name(2), "pci-bridge2");
        assert_eq!(bridge_name(9), "");
        // De GENMASK(6,1)-les van 29-08.
        assert_eq!(LNKCAP2_SLS & (1 << 7), 0);
        assert_eq!(((1u32 << 3) - 1) << 1, 0b1110);
    }
}

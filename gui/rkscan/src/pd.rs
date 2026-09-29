//! Het power-domein PD_VO: het draagt de VOP2 en de HDMI-TX, en zonder dat
//! domein aan zijn hun registers dood. Niet fout, dóód: een dichte klok
//! leest nullen, een afgeschakeld domein kan de bus vasthouden.
//!
//! REFERENTIE (opgehaald 05-08): Linux v6.13
//! drivers/pmdomain/rockchip/pm-domains.c (rk3568_pmu-offsets,
//! rk3568_pm_domains-tabel, rockchip_pd_power en de twee helpers eronder)
//! plus rk356x-base.dtsi voor het PMU-basisadres.

use crate::{Chain, Error, Result, Step, put};
use core::mem::offset_of;
use dev::Reg;

/// De vijf registers die één domein aan- of uitzetten (rk3568_pmu).
#[repr(C)]
pub(crate) struct Pmu {
    _r0: [u32; 20],
    /// BUS_IDLE_REQ: de NIU-idle-request (hiword-masked).
    idle_req: Reg<u32>,
    _r1: [u32; 3],
    /// BUS_IDLE_ACK: de bevestiging van de request.
    idle_ack: Reg<u32>,
    _r2: u32,
    /// BUS_IDLE_ST: de werkelijke idle-stand.
    idle_st: Reg<u32>,
    _r3: [u32; 11],
    /// PWR_ST: lezen, 0 = aan, 1 = uit.
    pwr_st: Reg<u32>,
    _r4: u32,
    /// PWR_CON: schrijven, 0 = aan, 1 = uit (hiword-masked).
    pwr_con: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Pmu, idle_req) == 0x50);
    assert!(offset_of!(Pmu, idle_ack) == 0x60);
    assert!(offset_of!(Pmu, idle_st) == 0x68);
    assert!(offset_of!(Pmu, pwr_st) == 0x98);
    assert!(offset_of!(Pmu, pwr_con) == 0xA0);
};

// PD_VO (rk3568_pm_domains: DOMAIN_RK3568("vo", BIT(7), BIT(4), false)).
// DOMAIN_RK3568 is DOMAIN_M, dus pwr en req zijn hiword-masked: het
// maskerbit zit 16 posities hoger. PWR_ST gebruikt hetzelfde bit als
// PWR_CON, IDLE_ST en IDLE_ACK hetzelfde als IDLE_REQ.
/// Het bit van PD_VO in PWR_CON en PWR_ST.
pub(crate) const PD_VO_PWR: u32 = 1 << 7;
/// Het bit van PD_VO in de idle-registers.
pub(crate) const PD_VO_REQ: u32 = 1 << 4;
/// Hoe lang een PMU-veld mag doen over schakelen (Go: 10 ms).
const PMU_WAIT_NS: u64 = 10_000_000;

/// De drie PMU-registers die een mislukte bring-up ontleden.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct PowerInfo {
    /// PWR_ST.
    pub status: u32,
    /// BUS_IDLE_ACK.
    pub ack: u32,
    /// BUS_IDLE_ST.
    pub idle: u32,
}

impl Chain {
    pub(crate) fn pmu(&self) -> &'static Pmu {
        // SAFETY: de voorwaarde van `Chain::new`: `b.pmu` is het PMU-blok.
        unsafe { dev::regs(self.b.pmu) }
    }

    /// Zet het video-domein aan. Idempotent: staat het al aan, dan doet
    /// dit niets behalve de klokken openzetten en de idle-request loslaten.
    ///
    /// De volgorde komt uit `rockchip_pd_power(pd, true)` en is niet vrij:
    ///
    /// 1. de klokken van het domein moeten LOPEN tijdens het schakelen
    ///    (daarom staat `clk_bulk_enable` er in de driver omheen), dus eerst
    ///    `vop_clock_on`;
    /// 2. het domein aan (pwr-bit naar 0) en wachten tot PWR_ST het meldt;
    /// 3. de NIU-idle-request LOSLATEN en wachten op ack én idle. Sla je dat
    ///    over, dan staat het domein aan maar isoleert de interconnect het:
    ///    registers lezen nullen en de DMA komt nooit bij DRAM.
    pub fn power_on_vo(&self) -> Result {
        self.vop_clock_on();
        let p = self.pmu();
        if p.pwr_st.read() & PD_VO_PWR != 0 {
            // Uit, dus aanzetten: alleen het maskerbit, de waarde 0 is "aan".
            put(&p.pwr_con, PD_VO_PWR << 16);
            dev::mb();
            self.pmu_wait(Step::PdPower, &p.pwr_st, PD_VO_PWR)?;
        }
        put(&p.idle_req, PD_VO_REQ << 16);
        dev::mb();
        self.pmu_wait(Step::PdIdleAck, &p.idle_ack, PD_VO_REQ)?;
        self.pmu_wait(Step::PdIdle, &p.idle_st, PD_VO_REQ)
    }

    /// Wacht tot `mask` in `reg` nul is. De fout draagt de rauwe inhoud,
    /// zodat één boot genoeg is om te weten welke stap bleef hangen.
    fn pmu_wait(&self, step: Step, reg: &Reg<u32>, mask: u32) -> Result {
        if self.wait(PMU_WAIT_NS, || reg.read() & mask == 0) {
            return Ok(());
        }
        let off = core::ptr::from_ref(reg).addr() - self.b.pmu.as_usize();
        Err(Error::Settle {
            step,
            off: off as u32,
            got: reg.read(),
            mask,
            want: 0,
        })
    }

    /// De drie relevante PMU-registers, voor de foutregel.
    #[must_use]
    pub fn power_info(&self) -> PowerInfo {
        let p = self.pmu();
        PowerInfo {
            status: p.pwr_st.read(),
            ack: p.idle_ack.read(),
            idle: p.idle_st.read(),
        }
    }
}

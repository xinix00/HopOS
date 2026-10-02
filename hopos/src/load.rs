//! De meetlat per slot op de console (docs/apps.md): om de
//! [`LOAD_EVERY`] één regel per levend slot met de idle-tijd en de wekken
//! die de app zelf op zijn control-page publiceert (`CTRL_IDLE`,
//! `CTRL_WAKES`; applib::sleep). Zonder deze regel is "de app slaapt" niet te
//! onderscheiden van "de app dut", en kost de jacht uren (de LicheeRV met
//! drie Rust-slots in één sharegroup, 02-10-2026). Het rekenwerk staat in
//! `kern::load`, op de host getoetst; hier alleen het lezen en het ritme.
//!
//! Op QEMU-TCG is `WFE` een no-op en meet een eigen core ~0% idle; een yield
//! op een gedeelde core telt wel.
use abi::hopabi::{AppStatus, CTRL_CORES, CTRL_IDLE, CTRL_STATUS, CTRL_WAKES};
use core::time::Duration;
use cpu::println;
use executor::Executor;
use kern::SLOT_CAP;
use kern::load::{Sample, slot_load};

/// Het ritme van de regel.
const LOAD_EVERY: Duration = Duration::from_secs(30);

pub(crate) fn start(exec: &'static Executor) {
    if let Err(e) = exec.spawn(run(exec)) {
        println!("load: task not spawned ({e:?}), no HOPOS_SLOT_LOAD lines");
    }
}

fn word(page: dev::Pa, off: u64) -> u64 {
    dev::pull(page.add(off), 8);
    dev::read64(page.add(off))
}

async fn run(exec: &'static Executor) {
    let mut last: [Option<Sample>; SLOT_CAP + 1] = [None; SLOT_CAP + 1];
    loop {
        exec.after(LOAD_EVERY).await;
        let at_ns = exec.now();
        for (slot, prev) in last.iter_mut().enumerate().skip(1) {
            let Some(page) = crate::clock::ctrl_page(slot) else {
                *prev = None;
                continue;
            };
            if AppStatus::from_raw(word(page, CTRL_STATUS)) != Some(AppStatus::Ready) {
                *prev = None;
                continue;
            }
            let now = Sample {
                idle: word(page, CTRL_IDLE),
                wakes: word(page, CTRL_WAKES),
                at_ns,
            };
            if let Some(p) = *prev
                && let Some(line) =
                    slot_load(slot, p, now, word(page, CTRL_CORES), cpu::idle::freq())
            {
                println!("{line}");
            }
            *prev = Some(now);
        }
    }
}

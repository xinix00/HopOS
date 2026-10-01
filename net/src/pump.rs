//! De RX-pomp: de uplink-NIC tussen de draad en de switch (Go: `rxLoop` en
//! de `WaitNIC`-goroutine in `hopnet`).
//!
//! De pomp bezit de NIC. Ontvangen frames gaan als waarde de ingress-rij in
//! (de switch doet de NAT en bezorgt); frames voor de draad komen uit de
//! egress-rij, die alleen de switch vult. Zo heeft de NIC één eigenaar en
//! elke rij één producer: de `uplinkTxMu` van de Go-kern bestaat niet meer.
//!
//! Wachten: op de NIC-interrupt waar het board er een bedraadt, met de
//! vangrail van 10 ms, want een verloren flank mag nooit een hang worden (hij
//! kost bij stilte 100 wekken per seconde in plaats van 3.333, en onder
//! verkeer wordt hij nooit geraakt). Zonder lijn: pollen op 300 µs, zodat de
//! core bij stilte echt kan slapen; onder last wordt er nooit geslapen.

use crate::{Frame, Stats, UPLINK_QUEUE};
use core::sync::atomic::Ordering::Relaxed;
use core::time::Duration;
use executor::Executor;
use netdev::{Device, TxError};
use sync::spsc::{Receiver, Sender};
use sync::{Either, Signal, Stop, select, yield_now};

/// De vangrail op de NIC-interrupt. Op het raster van 10 ms sinds boot,
/// hetzelfde als de vangrail van de servicers (`kern::slots::SERVICER_GUARD`):
/// dan kosten ze samen één wek per 10 ms in plaats van elk één (Linux:
/// `round_jiffies`; QEMU 30-09: een stille OS-core ~250 naar ~210).
pub const IRQ_GUARD: Duration = Duration::from_millis(10);

/// De poll-periode zonder interrupt.
pub const NIC_POLL: Duration = Duration::from_micros(300);

/// Frames per RX-burst. De RX-doorbell gaat per burst, niet per frame: per
/// frame scheelt dat één tot drie PCIe-acties, en op een 1 Gbit-link zijn
/// dat er 70.000 per seconde (L83, de 100 MB/s-jacht). Om de 32 frames toch
/// een flush, zodat de NIC nooit zonder descriptors zit.
pub const RX_BATCH: usize = 32;

/// De pomp.
pub struct Pump<'a, D: Device> {
    nic: D,
    ingress: Sender<'a, Frame, UPLINK_QUEUE>,
    egress: Receiver<'a, Frame, UPLINK_QUEUE>,
    /// De bel van de pomp: de switch zet hem na een ronde met egress-werk.
    bell: &'a Signal,
    /// De deur van de switch.
    door: &'a Signal,
    stats: &'a Stats,
}

impl<'a, D: Device> Pump<'a, D> {
    /// Een pomp over `nic`.
    pub fn new(
        nic: D,
        ingress: Sender<'a, Frame, UPLINK_QUEUE>,
        egress: Receiver<'a, Frame, UPLINK_QUEUE>,
        bell: &'a Signal,
        door: &'a Signal,
        stats: &'a Stats,
    ) -> Self {
        Self {
            nic,
            ingress,
            egress,
            bell,
            door,
            stats,
        }
    }

    /// De NIC.
    pub fn nic(&mut self) -> &mut D {
        &mut self.nic
    }

    /// Zet wat de switch klaarlegde de draad op. `true` = er ging iets uit.
    fn tx_pass(&mut self) -> bool {
        let mut sent = false;
        while let Some(f) = self.egress.try_recv() {
            match self.nic.transmit(f.bytes()) {
                Ok(()) => sent = true,
                // Vol: één flush (de doorbell van wat al klaarstaat) en nog
                // één poging; daarna is het Ethernet: drop, TCP herstelt.
                Err(TxError::Full) => {
                    self.nic.flush();
                    if self.nic.transmit(f.bytes()).is_ok() {
                        sent = true;
                    } else {
                        self.stats.nic_tx_errors.fetch_add(1, Relaxed);
                    }
                }
                Err(_) => {
                    self.stats.nic_tx_errors.fetch_add(1, Relaxed);
                }
            }
        }
        sent
    }

    /// Eén RX-burst de ingress-rij in. Een volle rij laat de frames in de
    /// NIC-ring liggen: dat is de backpressure, geen drop. Geeft het aantal.
    fn rx_pass(&mut self) -> usize {
        let mut n = 0;
        while n < RX_BATCH && self.ingress.free() > 0 {
            let mut f = Frame::new();
            let Some(len) = self.nic.receive(f.buf_mut()) else {
                break;
            };
            f.set_len(len);
            if self.ingress.try_send(f).is_err() {
                self.stats.uplink_rx_drops.fetch_add(1, Relaxed);
            }
            n += 1;
        }
        n
    }

    /// Eén ronde zonder wachten: TX, dan een RX-burst, dan de doorbells.
    /// `true` = er was werk.
    pub fn pass(&mut self) -> bool {
        let sent = self.tx_pass();
        let got = self.rx_pass();
        if sent || got > 0 {
            self.nic.flush();
        }
        if got > 0 {
            // De burst ligt bij de switch: hem nú wekken, niet pas bij HOP's
            // idle of de failsafe. Onder inbound verkeer is HOP nooit idle,
            // en dan lagen de ACK's van de apps tot de 1 ms-failsafe in hun
            // TX-ringen (gemeten 20-09: 37 MB/s = venster ÷ RTT).
            self.door.set();
        }
        sent || got > 0
    }

    /// De lus van de pomp. Keert terug als `stop` luidt.
    pub async fn run<const T: usize, const M: usize>(
        &mut self,
        exec: &'static Executor<T, M>,
        stop: &Stop,
    ) {
        let irq = self.nic.irq();
        loop {
            if stop.is_set() {
                return;
            }
            if self.pass() {
                yield_now().await;
                continue;
            }
            self.stats.rx_idle.fetch_add(1, Relaxed);
            let stopped = match irq {
                Some(line) => {
                    let grid = u64::try_from(IRQ_GUARD.as_nanos()).unwrap_or(u64::MAX);
                    let guard =
                        exec.until((exec.now() / grid).saturating_add(1).saturating_mul(grid));
                    let w = select(
                        stop.wait(),
                        select(self.bell.wait(), select(line.wait(), guard)),
                    );
                    matches!(w.await, Either::Left(()))
                }
                None => {
                    let w = select(stop.wait(), select(self.bell.wait(), exec.after(NIC_POLL)));
                    matches!(w.await, Either::Left(()))
                }
            };
            if stopped {
                return;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{Egress, Ingress};
    use netdev::Mac;
    use std::cell::Cell;
    use std::collections::VecDeque;

    struct FakeNic {
        rx: VecDeque<Vec<u8>>,
        tx: Vec<Vec<u8>>,
        flushes: usize,
        full_once: bool,
    }

    impl Device for FakeNic {
        fn transmit(&mut self, f: &[u8]) -> Result<(), TxError> {
            if self.full_once {
                self.full_once = false;
                return Err(TxError::Full);
            }
            self.tx.push(f.to_vec());
            Ok(())
        }
        fn receive(&mut self, buf: &mut [u8]) -> Option<usize> {
            let f = self.rx.pop_front()?;
            buf[..f.len()].copy_from_slice(&f);
            Some(f.len())
        }
        fn flush(&mut self) {
            self.flushes += 1;
        }
        fn mac(&self) -> Mac {
            Mac([2, 0, 0, 0, 0, 9])
        }
    }

    fn leak<T>(v: T) -> &'static T {
        Box::leak(Box::new(v))
    }

    #[test]
    fn rx_burst_gaat_de_switch_in_en_wekt_hem() {
        let (ing_tx, mut ing_rx) = leak(Ingress::new()).split().unwrap();
        let (_eg_tx, eg_rx) = leak(Egress::new()).split().unwrap();
        let door = leak(Signal::new());
        let stats = leak(Stats::new());
        let nic = FakeNic {
            rx: (0..40u8).map(|i| vec![i; 60]).collect(),
            tx: Vec::new(),
            flushes: 0,
            full_once: false,
        };
        let mut p = Pump::new(nic, ing_tx, eg_rx, leak(Signal::new()), door, stats);
        assert!(p.pass());
        assert_eq!(ing_rx.len(), RX_BATCH, "één burst is RX_BATCH frames");
        assert_eq!(p.nic().flushes, 1, "één doorbell per burst");
        assert!(door.is_set());
        assert!(p.pass());
        assert_eq!(ing_rx.len(), 40);
        assert_eq!(ing_rx.try_recv().unwrap().bytes(), &[0u8; 60][..]);
        assert!(!p.pass(), "lege NIC is geen werk");
    }

    #[test]
    fn volle_ingress_laat_de_frames_in_de_nic() {
        let (ing_tx, mut ing_rx) = leak(Ingress::new()).split().unwrap();
        let (_eg_tx, eg_rx) = leak(Egress::new()).split().unwrap();
        let stats = leak(Stats::new());
        let nic = FakeNic {
            rx: (0..UPLINK_QUEUE + 5).map(|_| vec![1; 60]).collect(),
            tx: Vec::new(),
            flushes: 0,
            full_once: false,
        };
        let mut p = Pump::new(
            nic,
            ing_tx,
            eg_rx,
            leak(Signal::new()),
            leak(Signal::new()),
            stats,
        );
        while p.pass() {}
        assert_eq!(ing_rx.len(), UPLINK_QUEUE);
        assert_eq!(
            p.nic().rx.len(),
            5,
            "de rest hoort in de NIC-ring te blijven"
        );
        assert_eq!(stats.uplink_rx_drops.load(Relaxed), 0);
        let _ = ing_rx.try_recv();
        assert!(p.pass());
        assert_eq!(p.nic().rx.len(), 4);
    }

    #[test]
    fn egress_gaat_de_draad_op_met_een_flush() {
        let (ing_tx, _ing_rx) = leak(Ingress::new()).split().unwrap();
        let (mut eg_tx, eg_rx) = leak(Egress::new()).split().unwrap();
        let stats = leak(Stats::new());
        let nic = FakeNic {
            rx: VecDeque::new(),
            tx: Vec::new(),
            flushes: 0,
            full_once: true,
        };
        let mut p = Pump::new(
            nic,
            ing_tx,
            eg_rx,
            leak(Signal::new()),
            leak(Signal::new()),
            stats,
        );
        for i in 0..3u8 {
            assert!(eg_tx.try_send(Frame::from_slice(&[i; 42]).unwrap()).is_ok());
        }
        assert!(p.pass());
        assert_eq!(
            p.nic().tx.len(),
            3,
            "een volle TX-ring kreeg zijn tweede poging niet"
        );
        assert_eq!(p.nic().flushes, 2, "de flush bij vol plus die van de burst");
        assert_eq!(stats.nic_tx_errors.load(Relaxed), 0);
    }

    thread_local! {
        static NOW: Cell<u64> = const { Cell::new(0) };
    }
    fn fake_now() -> u64 {
        NOW.with(Cell::get)
    }

    /// De lus pollt zonder lijn op zijn eigen klok, en stopt op de bel.
    #[test]
    fn poll_lus_pakt_een_frame_na_de_poll_periode() {
        let exec: &'static Executor<4, 4> = leak(Executor::new());
        exec.set_clock(fake_now);
        let (ing_tx, ing_rx) = leak(Ingress::new()).split().unwrap();
        let (_eg_tx, eg_rx) = leak(Egress::new()).split().unwrap();
        let stats = leak(Stats::new());
        let stop: &'static Stop = leak(Stop::new());
        let nic = FakeNic {
            rx: VecDeque::new(),
            tx: Vec::new(),
            flushes: 0,
            full_once: false,
        };
        let mut p = Pump::new(
            nic,
            ing_tx,
            eg_rx,
            leak(Signal::new()),
            leak(Signal::new()),
            stats,
        );
        p.nic().rx.push_back(vec![7; 64]);
        // De taak bezit de pomp; de test kijkt via de rij en de tellers.
        exec.spawn(async move { p.run(exec, stop).await }).unwrap();
        while exec.step() {}
        assert_eq!(ing_rx.len(), 1, "het frame kwam niet door");
        assert!(stats.rx_idle.load(Relaxed) >= 1);
        NOW.with(|n| n.set(NIC_POLL.as_nanos() as u64 + 1));
        while exec.step() {}
        assert!(
            stats.rx_idle.load(Relaxed) >= 2,
            "de poll-timer wekte de pomp niet"
        );
        stop.set();
        while exec.step() {}
        assert_eq!(exec.live_tasks(), 0);
    }
}

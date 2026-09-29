//! De console van de Mac mini: de dockchannel en de Samsung-UART, allebei
//! al opgezet door iBoot of m1n1 (klok, baud, pinnen); wij pollen en
//! schrijven alleen.
//!
//! - De dockchannel (`aapl,dock-channels`) is een FIFO naar de
//!   Type-C-debugpoort, en m1n1's console op de M2 en later: TX-vrije
//!   plaatsen op +0x4014, een byte in op +0x4004 (m1n1
//!   `dockchannel_uart.c`). Op de laptop is het `/dev/cu.kis-100000-ch-0`
//!   na `macvdmtool reboot debugusb` (GEMETEN 28-08).
//! - uart0 (`uart-1,samsung`): UTRSTAT op +0x10 (bit 1 = TX leeg), UTXH op
//!   +0x20 (m1n1 `uart.c`). Op de M4 droeg de debug-console hier niets van
//!   (28-08), maar het kost per byte één poll en een board dat zonder de
//!   dockchannel opkomt, heeft dan nog een weg.
//!
//! Vaste adressen, en dat is een keuze (Go 29-08): de ADT weet ze ook, maar
//! de eerste bytes console-uitvoer mogen niet hangen aan een boom die zelf
//! pas werkt als het DRAM gemapt is. `discover` meldt het als de ADT het
//! anders ziet.
//!
//! Een console mag de node nooit ophouden: de poll is begrensd, en een lijn
//! die stokt krijgt daarna een klein budget tot een byte weer past. Zo kost
//! een lezerloze FIFO geen 2 ms per byte, maar komt een lezer die later
//! aanhaakt wél weer in beeld (Go `console.go`).

use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use dev::Pa;

/// De dockchannel van de t8132 (GEMETEN 28-08).
pub const DOCKCHANNEL: u64 = crate::head::DOCK_FALLBACK;
/// uart0 van de t8132 (GEMETEN 28-08).
pub const UART0: u64 = 0x3_ad20_0000;

const DOCK_TX: u64 = 0x4004;
const DOCK_TX_FREE: u64 = 0x4014;
const UTRSTAT: u64 = 0x10;
const UTXH: u64 = 0x20;
const UTRSTAT_TXBE: u32 = 1 << 1;

/// De poll per byte; en als de vorige byte al niet paste.
const POLL_MAX: u32 = 20_000;
const POLL_STALLED: u32 = 256;

static DOCK_STALLED: AtomicBool = AtomicBool::new(false);
static UART_STALLED: AtomicBool = AtomicBool::new(false);

/// Wacht begrensd tot `ready` en schrijft dan `c`; lukt dat niet binnen
/// het budget, dan valt de byte en staat de lijn als gestokt.
fn put(status: Pa, ready: impl Fn(u32) -> bool, data: Pa, c: u8, stalled: &AtomicBool) {
    let budget = if stalled.load(Relaxed) {
        POLL_STALLED
    } else {
        POLL_MAX
    };
    for _ in 0..budget {
        if ready(dev::read32(status)) {
            dev::write32(data, u32::from(c));
            stalled.store(false, Relaxed);
            return;
        }
    }
    stalled.store(true, Relaxed);
}

fn put_both(c: u8) {
    let d = Pa(DOCKCHANNEL);
    put(
        d.add(DOCK_TX_FREE),
        |v| v != 0,
        d.add(DOCK_TX),
        c,
        &DOCK_STALLED,
    );
    let u = Pa(UART0);
    put(
        u.add(UTRSTAT),
        |v| v & UTRSTAT_TXBE != 0,
        u.add(UTXH),
        c,
        &UART_STALLED,
    );
}

/// De console-haak van de kern: `\n` wordt `\r\n` (de tty aan de andere kant
/// staat raw; m1n1 doet hetzelfde).
pub fn write(b: &[u8]) {
    for &c in b {
        if c == b'\n' {
            put_both(b'\r');
        }
        put_both(c);
    }
}

/// Eén regel op de dockchannel met `v` als 16 hexcijfers, zonder stack-
/// buffers of formattering: dit draait met de MMU uit (`head::apple_early`).
pub(crate) fn raw_line(dock: Pa, text: &[u8], v: u64) {
    let byte = |c: u8| {
        put(
            dock.add(DOCK_TX_FREE),
            |s| s != 0,
            dock.add(DOCK_TX),
            c,
            &DOCK_STALLED,
        )
    };
    for &c in b"\r\n".iter().chain(text) {
        byte(c);
    }
    for i in (0..16).rev() {
        let n = ((v >> (4 * i)) & 0xf) as u8;
        byte(if n < 10 { b'0' + n } else { b'a' + n - 10 });
    }
    byte(b'\r');
    byte(b'\n');
}

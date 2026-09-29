//! Het beeld van QEMU virt: `-device ramfb`, ingesteld over fw_cfg.
//!
//! Dit bezit de framebuffer-regio van dit board en de ene instelling per
//! boot. Zonder `-device ramfb` is er geen bestand `etc/ramfb` en is het
//! board headless, zoals altijd (Go: "bewust niet gebouwd: dev-target, geen
//! edge-node"). Het bestaat nu wel, omdat de QEMU-poort de console op het
//! glas moet kunnen zien: `tools/qemu-test.sh` met `GUI=1` maakt er een
//! screendump van.
//!
//! ramfb is een RAM-buffer van de gast die QEMU uitleest: de gast kiest
//! adres en maat en schrijft die (big-endian) in het fw_cfg-bestand
//! `etc/ramfb`, via de DMA-interface van fw_cfg (`docs/specs/fw_cfg.rst`,
//! `hw/display/ramfb.c`). Onder EDK2 doet QemuRamfbDxe precies dit en
//! wordt het een GOP; zo vond Go op 19-07 dat de buffer boven 4 GB kan
//! liggen (0x1_bc7a_0000), en daarom ziet een app hem op een vast IPA
//! (`kern::grants`). Hier kiezen we het adres zelf: [`FB_REGION`].

use board::Region;
use core::sync::atomic::{AtomicU8, Ordering::Relaxed};
use dev::Pa;
use driver_fb::Desc;

/// Het fw_cfg-blok van virt (`VIRT_FW_CFG`).
const FW_CFG: Pa = Pa(0x0902_0000);
/// Het dataregister.
const DATA: u64 = 0x00;
/// Het selectorregister (16 bits, big-endian).
const SELECTOR: u64 = 0x08;
/// Het DMA-adresregister (64 bits, big-endian); de schrijf start de DMA.
const DMA_ADDR: u64 = 0x10;

/// De sleutels.
const KEY_SIGNATURE: u16 = 0x0000;
const KEY_ID: u16 = 0x0001;
const KEY_FILE_DIR: u16 = 0x0019;
/// `FW_CFG_ID` bit 1: de DMA-interface bestaat.
const ID_DMA: u32 = 1 << 1;

/// De bits van het DMA-controlwoord.
const DMA_ERROR: u32 = 0x01;
const DMA_SELECT: u32 = 0x08;
const DMA_WRITE: u32 = 0x10;

/// De framebuffer-regio: 8 MB in het gat boven de kooi-regio (0xC400_0000
/// tot 4 GB is RAM dat niemand uitdeelt; `mmu.rs` mapt het Normal WB en de
/// kern zet de buffer daarna op Normal-NC, `cpu::memattr`). 2 MB-gealigneerd,
/// zodat dat venster precies past. De laatste pagina is de kladblok van de
/// fw_cfg-DMA.
pub(crate) const FB_REGION: Region = Region {
    base: Pa(0xC400_0000),
    size: 8 << 20,
};
/// De kladblokpagina: het DMA-verzoek op +0, de ramfb-configuratie op +0x40.
const SCRATCH: Pa = Pa(FB_REGION.base.0 + FB_REGION.size - 0x1000);

/// De modus: 1280x800, zodat de console 16x16-cellen krijgt (80x50) en
/// de buffer met de kladblok in de regio past.
const WIDTH: u32 = 1280;
const HEIGHT: u32 = 800;
/// DRM_FORMAT_XRGB8888 (`'X' 'R' '2' '4'`): wat `driver-fb` op 32 bpp
/// schrijft (0xAARRGGBB als little-endian woord).
const FOURCC_XRGB8888: u32 = 0x3432_5258;

const _: () = {
    assert!(FB_REGION.base.0.is_multiple_of(2 << 20) && FB_REGION.size.is_multiple_of(2 << 20));
    assert!((WIDTH as u64) * (HEIGHT as u64) * 4 <= SCRATCH.0 - FB_REGION.base.0);
    // Boven de kooi-regio en onder 4 GB (qemu-run.sh: `-m 3G`, RAM tot
    // 0x1_0000_0000).
    assert!(
        FB_REGION.base.0 >= crate::slots::DEVICE_WINDOW.base + crate::slots::DEVICE_WINDOW.size
    );
    assert!(FB_REGION.base.0 + FB_REGION.size <= 0x1_0000_0000);
};

/// 0 = nog niet geprobeerd, 1 = het beeld staat, 2 = geen beeld.
static STATE: AtomicU8 = AtomicU8::new(0);

/// De framebuffer van deze boot: de eerste vraag stelt ramfb in, elke
/// volgende krijgt dezelfde uitkomst.
pub(crate) fn framebuffer() -> Option<Desc> {
    let state = match STATE.load(Relaxed) {
        0 => {
            let up = match setup() {
                Ok(()) => {
                    cpu::println!(
                        "fb: ramfb {WIDTH}x{HEIGHT} @ {:#x} HOPOS_FB_UP",
                        FB_REGION.base.0
                    );
                    1
                }
                Err(why) => {
                    cpu::println!("fb: no ramfb ({why}), headless");
                    2
                }
            };
            STATE.store(up, Relaxed);
            up
        }
        s => s,
    };
    (state == 1).then_some(desc())
}

/// De descriptor van onze buffer.
fn desc() -> Desc {
    Desc {
        base: FB_REGION.base,
        width: WIDTH,
        height: HEIGHT,
        stride: WIDTH * 4,
        bpp: 32,
        swap_rb: false,
    }
}

/// Zoekt `etc/ramfb` en schrijft de configuratie erin.
fn setup() -> Result<(), &'static str> {
    select(KEY_SIGNATURE);
    let mut sig = [0u8; 4];
    read(&mut sig);
    if &sig != b"QEMU" {
        return Err("no fw_cfg");
    }
    select(KEY_ID);
    if read_le32() & ID_DMA == 0 {
        return Err("fw_cfg without DMA");
    }
    let key = find_file(b"etc/ramfb").ok_or("no -device ramfb")?;
    let cfg = config(FB_REGION.base.0, WIDTH, HEIGHT);
    dev::copy_in(SCRATCH.add(0x40), &cfg);
    dma_write(key, SCRATCH.add(0x40), cfg.len())
}

/// De 28 bytes van `RAMFBCfg`, big-endian: adres, fourcc, vlaggen,
/// breedte, hoogte, stride.
fn config(addr: u64, w: u32, h: u32) -> [u8; 28] {
    let mut c = [0u8; 28];
    c[0..8].copy_from_slice(&addr.to_be_bytes());
    c[8..12].copy_from_slice(&FOURCC_XRGB8888.to_be_bytes());
    c[16..20].copy_from_slice(&w.to_be_bytes());
    c[20..24].copy_from_slice(&h.to_be_bytes());
    c[24..28].copy_from_slice(&(w * 4).to_be_bytes());
    c
}

fn select(key: u16) {
    dev::write16(FW_CFG.add(SELECTOR), key.to_be());
}

fn read(buf: &mut [u8]) {
    for b in buf {
        *b = dev::read8(FW_CFG.add(DATA));
    }
}

/// `FW_CFG_ID` is little-endian (de enige sleutel die dat is).
fn read_le32() -> u32 {
    let mut b = [0u8; 4];
    read(&mut b);
    u32::from_le_bytes(b)
}

fn read_be<const N: usize>() -> [u8; N] {
    let mut b = [0u8; N];
    read(&mut b);
    b
}

/// De sleutel van bestand `name` in de fw_cfg-directory.
fn find_file(name: &[u8]) -> Option<u16> {
    select(KEY_FILE_DIR);
    let count = u32::from_be_bytes(read_be::<4>());
    for _ in 0..count.min(1024) {
        let _size = read_be::<4>();
        let key = u16::from_be_bytes(read_be::<2>());
        let _reserved = read_be::<2>();
        let entry: [u8; 56] = read_be();
        let len = entry.iter().position(|&b| b == 0).unwrap_or(entry.len());
        if entry.get(..len) == Some(name) {
            return Some(key);
        }
    }
    None
}

/// Schrijft `len` bytes vanaf `src` naar fw_cfg-sleutel `key` via de DMA,
/// en wacht op de bevestiging (QEMU doet het synchroon; de grens is voor
/// een fw_cfg die dat niet doet).
fn dma_write(key: u16, src: Pa, len: usize) -> Result<(), &'static str> {
    let control = u32::from(key) << 16 | DMA_SELECT | DMA_WRITE;
    let len = u32::try_from(len).map_err(|_| "config too long")?;
    // FWCfgDmaAccess: control, length, address; alles big-endian.
    dev::write32(SCRATCH, control.to_be());
    dev::write32(SCRATCH.add(4), len.to_be());
    dev::write64(SCRATCH.add(8), src.0.to_be());
    dev::push(SCRATCH, 0x80);
    dev::write64(FW_CFG.add(DMA_ADDR), SCRATCH.0.to_be());
    for _ in 0..1_000_000 {
        dev::pull(SCRATCH, 16);
        let c = u32::from_be(dev::read32(SCRATCH));
        if c & DMA_ERROR != 0 {
            return Err("fw_cfg DMA error");
        }
        if c == 0 {
            return Ok(());
        }
        core::hint::spin_loop();
    }
    Err("fw_cfg DMA timeout")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_config_is_big_endian_ramfbcfg() {
        let c = config(0xC400_0000, 1280, 800);
        assert_eq!(&c[0..8], &[0, 0, 0, 0, 0xC4, 0, 0, 0]);
        // De fourcc is een getal ('X' in de laagste byte), big-endian
        // geschreven: QEMU doet `be32_to_cpu` en vergelijkt met
        // DRM_FORMAT_XRGB8888.
        assert_eq!(
            u32::from_be_bytes([c[8], c[9], c[10], c[11]]),
            FOURCC_XRGB8888
        );
        assert_eq!(FOURCC_XRGB8888.to_le_bytes(), *b"XR24");
        assert_eq!(&c[12..16], &[0; 4]);
        assert_eq!(u32::from_be_bytes([c[16], c[17], c[18], c[19]]), 1280);
        assert_eq!(u32::from_be_bytes([c[20], c[21], c[22], c[23]]), 800);
        assert_eq!(u32::from_be_bytes([c[24], c[25], c[26], c[27]]), 5120);
        assert!(desc().check().is_ok());
    }
}

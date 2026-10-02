//! Het bewijs dat tools/mkcard de Go-mkcard is: drie kaarten met de vormen
//! van de scripts (de Pi's, de Radxa met zijn donor, de LicheeRV op LBA 1)
//! uit vaste bestanden, byte voor byte. Staat `go` op de machine en de
//! Go-bron nog in de boom, dan bouwt de Go-tool dezelfde kaart ernaast; zo
//! niet, dan houdt de sha256 die op 02-10-2026 uit de Go-tool kwam de lat.
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

use std::path::{Path, PathBuf};
use std::process::Command;

/// De sha256 per kaart, zoals de Go-mkcard (tag v2.2.8) ze op 02-10-2026
/// maakte uit [`inputs`].
const PINNED: [(&str, &str); 3] = [
    (
        "pi",
        "2c48c0d2b485639549f9149cba7292f50cee263c6a55c3f2e010f5a035b37891",
    ),
    (
        "radxa",
        "9e0ba318ead20f42b874b58066efd976693f0f4bc3103bbe9d1f553f5d6994a8",
    ),
    (
        "licheerv",
        "35cc4754268baa55436b5fbdfde640f80b8574a38d1498587437e7cb03f5ef6f",
    ),
];

/// De kaarten: naam en de argumenten (vóór `-o`), met de vormen van
/// image/rpi4.sh, image/radxa-zero3.sh en image/licheerv-agent.sh plus de
/// randen van de 8.3-namen.
fn cases() -> [(&'static str, Vec<&'static str>); 3] {
    [
        (
            "pi",
            vec![
                "-size",
                "64",
                "-start",
                "8192",
                "-label",
                "bootfs",
                "-vollabel",
                "kernel8.img",
                "config.txt",
                "cmdline.txt",
                "start4.elf",
                "bcm2711-rpi-4-b.dtb",
                "bcm2712-rpi-5-b.dtb",
                "dtbo.bin=overlays/bcm2712d0.dtbo",
                "hop.elf",
            ],
        ),
        (
            "radxa",
            vec![
                "-size=40",
                "-start",
                "32768",
                "-label",
                "hopos",
                "-vollabel=true",
                "-raw",
                "donor.bin@32768",
                "-raw",
                "small.bin@0x200000",
                "hopos.img",
                "hopos.ird",
                "extlinux.conf=extlinux/extlinux.conf",
                "boot.efi=EFI/BOOT/BOOTAA64.EFI",
                "kernel8.img=averylongname",
                "config.txt=a.b.c",
            ],
        ),
        ("licheerv", vec!["-size", "16", "fip-licheerv.bin=fip.bin"]),
    ]
}

/// De invoerbestanden: naam en lengte; de inhoud komt uit [`bytes`].
fn inputs() -> [(&'static str, usize); 15] {
    [
        ("kernel8.img", 70_001),
        ("config.txt", 300),
        ("cmdline.txt", 20),
        ("start4.elf", 2048),
        ("bcm2711-rpi-4-b.dtb", 5000),
        ("bcm2712-rpi-5-b.dtb", 5001),
        ("dtbo.bin", 1363),
        ("hop.elf", 1),
        ("donor.bin", 100_000),
        ("small.bin", 4096),
        ("hopos.img", 123_457),
        ("hopos.ird", 9000),
        ("extlinux.conf", 200),
        ("boot.efi", 65_536),
        ("fip-licheerv.bin", 440_832),
    ]
}

/// Vaste, niet-triviale inhoud: een xorshift per bestand.
fn bytes(seed: usize, len: usize) -> Vec<u8> {
    let mut x = 0x9E37_79B9_u32 ^ u32::try_from(seed + 1).unwrap();
    (0..len)
        .map(|_| {
            x ^= x << 13;
            x ^= x >> 17;
            x ^= x << 5;
            x.to_le_bytes()[0]
        })
        .collect()
}

/// Een eigen map per testrun.
fn workdir() -> PathBuf {
    let d = std::env::temp_dir().join(format!("mkcard-go-{}", std::process::id()));
    std::fs::create_dir_all(&d).unwrap();
    for (i, (name, len)) in inputs().into_iter().enumerate() {
        std::fs::write(d.join(name), bytes(i, len)).unwrap();
    }
    d
}

/// De Go-bron, als die nog in de boom staat en `go` er is.
fn go_source() -> Option<PathBuf> {
    let src = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../OLD/image/mkcard/main.go");
    let go = Command::new("go").arg("version").output();
    (src.exists() && go.is_ok_and(|o| o.status.success())).then_some(src)
}

fn run(cmd: &mut Command, dir: &Path) {
    let out = cmd.current_dir(dir).output().unwrap();
    assert!(
        out.status.success(),
        "{cmd:?}: {}",
        String::from_utf8_lossy(&out.stderr)
    );
}

#[test]
fn byte_for_byte_like_go() {
    let dir = workdir();
    let go = go_source();
    let mut got = Vec::new();
    for (name, args) in cases() {
        let rust_img = dir.join(format!("{name}-rust.img"));
        run(
            Command::new(env!("CARGO_BIN_EXE_mkcard"))
                .arg("-o")
                .arg(&rust_img)
                .args(&args),
            &dir,
        );
        let ours = std::fs::read(&rust_img).unwrap();
        if let Some(src) = &go {
            let go_img = dir.join(format!("{name}-go.img"));
            run(
                Command::new("go")
                    .env("GOTOOLCHAIN", "local")
                    .env("GOWORK", "off")
                    .arg("run")
                    .arg(src)
                    .arg("-o")
                    .arg(&go_img)
                    .args(&args),
                &dir,
            );
            let theirs = std::fs::read(&go_img).unwrap();
            assert_eq!(ours.len(), theirs.len(), "{name}: length");
            let first = ours.iter().zip(&theirs).position(|(a, b)| a != b);
            assert_eq!(first, None, "{name}: first differing byte");
        }
        got.push((name, sha256(&ours)));
    }
    std::fs::remove_dir_all(&dir).unwrap();
    let want: Vec<_> = PINNED.iter().map(|&(n, s)| (n, s.to_owned())).collect();
    assert_eq!(got, want);
}

#[test]
fn sha256_is_sha256() {
    assert_eq!(
        sha256(b"abc"),
        "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
    );
}

/// SHA-256 (FIPS 180-4), want de lat is een getal dat `shasum -a 256`
/// ook geeft en de werkruimte heeft geen crates van buiten.
fn sha256(data: &[u8]) -> String {
    const K: [u32; 64] = [
        0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4,
        0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe,
        0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f,
        0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
        0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc,
        0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
        0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116,
        0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
        0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7,
        0xc67178f2,
    ];
    let mut h: [u32; 8] = [
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab,
        0x5be0cd19,
    ];
    let bits = u64::try_from(data.len()).unwrap() * 8;
    let mut tail = data[data.len() - data.len() % 64..].to_vec();
    tail.push(0x80);
    tail.resize((tail.len() + 8).next_multiple_of(64) - 8, 0);
    tail.extend_from_slice(&bits.to_be_bytes());
    let body = &data[..data.len() - data.len() % 64];
    for block in body.chunks_exact(64).chain(tail.chunks_exact(64)) {
        let mut w = [0u32; 64];
        for (wi, b) in w.iter_mut().zip(block.chunks_exact(4)) {
            *wi = u32::from_be_bytes(b.try_into().unwrap());
        }
        for i in 16..64 {
            let s0 = w[i - 15].rotate_right(7) ^ w[i - 15].rotate_right(18) ^ (w[i - 15] >> 3);
            let s1 = w[i - 2].rotate_right(17) ^ w[i - 2].rotate_right(19) ^ (w[i - 2] >> 10);
            w[i] = w[i - 16]
                .wrapping_add(s0)
                .wrapping_add(w[i - 7])
                .wrapping_add(s1);
        }
        let [mut a, mut b, mut c, mut d, mut e, mut f, mut g, mut hh] = h;
        for (k, wi) in K.iter().zip(w) {
            let s1 = e.rotate_right(6) ^ e.rotate_right(11) ^ e.rotate_right(25);
            let ch = (e & f) ^ (!e & g);
            let t1 = hh
                .wrapping_add(s1)
                .wrapping_add(ch)
                .wrapping_add(*k)
                .wrapping_add(wi);
            let s0 = a.rotate_right(2) ^ a.rotate_right(13) ^ a.rotate_right(22);
            let t2 = s0.wrapping_add((a & b) ^ (a & c) ^ (b & c));
            hh = g;
            g = f;
            f = e;
            e = d.wrapping_add(t1);
            d = c;
            c = b;
            b = a;
            a = t1.wrapping_add(t2);
        }
        for (x, y) in h.iter_mut().zip([a, b, c, d, e, f, g, hh]) {
            *x = x.wrapping_add(y);
        }
    }
    h.iter().map(|x| format!("{x:08x}")).collect()
}

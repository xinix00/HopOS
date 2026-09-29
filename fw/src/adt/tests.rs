//! Host-tests voor de ADT-lezer, geport uit `OLD/metal/fw/adt/adt_test.go`.
//! De boom wordt hier in een `Vec` gebouwd en als slice gelezen: exact de
//! offset-rekenkunde die op ijzer de firmware-input verwerkt.

use super::*;

/// Een ADT-node om te coderen: properties (naam, waarde) en kinderen.
struct N {
    name: &'static str,
    props: Vec<(&'static str, Vec<u8>)>,
    children: Vec<N>,
}

fn n(name: &'static str, props: Vec<(&'static str, Vec<u8>)>, children: Vec<N>) -> N {
    N {
        name,
        props,
        children,
    }
}

fn u32b(v: u32) -> Vec<u8> {
    v.to_le_bytes().to_vec()
}

fn u64b(v: u64) -> Vec<u8> {
    v.to_le_bytes().to_vec()
}

fn cat(parts: &[Vec<u8>]) -> Vec<u8> {
    parts.concat()
}

impl N {
    fn encode(&self) -> Vec<u8> {
        let mut name = self.name.as_bytes().to_vec();
        name.push(0);
        let mut props = vec![("name", name)];
        props.extend(self.props.iter().cloned());
        let mut out = u32b(props.len() as u32);
        out.extend(u32b(self.children.len() as u32));
        for (k, v) in &props {
            let mut nb = [0u8; PROP_NAME_LEN];
            nb[..k.len()].copy_from_slice(k.as_bytes());
            out.extend(nb);
            out.extend(u32b(v.len() as u32));
            out.extend(v);
            while !out.len().is_multiple_of(4) {
                out.push(0);
            }
        }
        for c in &self.children {
            out.extend(c.encode());
        }
        out
    }
}

fn sample() -> N {
    n(
        "device-tree",
        vec![("compatible", b"j773gap\0".to_vec())],
        vec![
            n(
                "cpus",
                vec![("#address-cells", u32b(2))],
                vec![
                    n(
                        "cpu0",
                        vec![("reg", u32b(0)), ("cpu-impl-reg", u64b(0x2_1005_0000))],
                        vec![],
                    ),
                    n(
                        "cpu6",
                        vec![("reg", u32b(0x100)), ("cpu-impl-reg", u64b(0x2_1105_0000))],
                        vec![],
                    ),
                ],
            ),
            n(
                "arm-io",
                vec![],
                vec![
                    n(
                        "uart0",
                        vec![("reg", cat(&[u64b(0x3_ad20_0000), u64b(0x4000)]))],
                        vec![],
                    ),
                    n("ans", vec![("nvme-secure-bar", vec![])], vec![]),
                ],
            ),
        ],
    )
}

#[test]
fn path_and_props() {
    let b = sample().encode();
    let t = Adt::new(&b).unwrap();
    assert_eq!(t.path("/"), Some(Node::ROOT));
    assert_eq!(t.name(Node::ROOT), Some("device-tree"));
    assert_eq!(t.str(Node::ROOT, "compatible"), Some("j773gap"));

    let u = t.path("/arm-io/uart0").unwrap();
    assert_eq!(t.reg("/arm-io/uart0", 0), Some((0x3_ad20_0000, 0x4000)));
    assert_eq!(t.reg("/arm-io/uart0", 1), None, "reg[1] bestaat niet");
    assert_eq!(t.prop(u, "nvme-secure-bar"), None);
    assert_eq!(t.path("/arm-io/does-not-exist"), None);
}

#[test]
fn children_and_name_separators() {
    let b = sample().encode();
    let t = Adt::new(&b).unwrap();
    let cpus = t.path("/cpus").unwrap();
    let seen: Vec<_> = t.children(cpus).filter_map(|c| t.name(c)).collect();
    assert_eq!(seen, ["cpu0", "cpu6"]);
    // '-' en '_' zijn in ADT-namen uitwisselbaar; de firmware is daar niet
    // consequent in.
    assert_eq!(t.u32(cpus, "#address_cells"), Some(2));
    let c6 = t.child(cpus, "cpu6").unwrap();
    assert_eq!(t.u32(c6, "reg"), Some(0x100));
    assert_eq!(t.u64(c6, "cpu-impl-reg"), Some(0x2_1105_0000));
    // Een u64 uit een property van vier bytes bestaat niet.
    assert_eq!(t.u64(c6, "reg"), None);
    // De kinderen van de wortel lopen voorbij een node met kinderen.
    let top: Vec<_> = t.children(Node::ROOT).filter_map(|c| t.name(c)).collect();
    assert_eq!(top, ["cpus", "arm-io"]);
}

/// Een lege property (size 0) is geldig en betekent "deze vlag staat aan":
/// `nvme-secure-bar` is er zo een, en daar hangt op de M4 de hele
/// NVMe-registerkaart aan.
#[test]
fn empty_property_is_a_flag() {
    let b = sample().encode();
    let t = Adt::new(&b).unwrap();
    let ans = t.path("/arm-io/ans").unwrap();
    assert_eq!(t.prop(ans, "nvme-secure-bar"), Some(&[][..]));
}

/// Onvertrouwde input: een verkeerde maat mag geen panic geven en niets
/// buiten de boom lezen.
#[test]
fn broken_tree_is_refused() {
    let b = sample().encode();
    assert_eq!(Adt::new(&b[..4]).unwrap_err(), Error::BadSize(4));
    assert!(
        Adt::new(&[0u8; 16]).is_err(),
        "een wortel zonder properties"
    );
    // Halverwege afgekapt: een pad vinden mag niet lukken, en niets panict.
    let half = &b[..b.len() / 2];
    if let Ok(t) = Adt::new(half) {
        assert_eq!(t.path("/arm-io/uart0"), None);
        let _ = t.children(Node::ROOT).count();
    }
    // Elke afkapping: nooit een panic.
    for cut in 8..b.len() {
        if let Ok(t) = Adt::new(&b[..cut]) {
            let _ = t.reg("/arm-io/uart0", 0);
            let _ = t.children(Node::ROOT).count();
        }
    }
}

/// Een nesting dieper dan [`MAX_DEPTH`] is een `None`, geen
/// stack-overloop (de Go-lezer liep recursief).
#[test]
fn deep_nesting_is_bounded() {
    let mut node = n("leaf", vec![], vec![]);
    for _ in 0..MAX_DEPTH + 4 {
        node = n("x", vec![], vec![node]);
    }
    let root = n(
        "device-tree",
        vec![],
        vec![node, n("after", vec![], vec![])],
    );
    let b = root.encode();
    let t = Adt::new(&b).unwrap();
    // Het tweede kind ligt achter de te diepe boom: niet bereikbaar.
    assert_eq!(t.path("/after"), None);
}

/// De ranges-vertaling met de echte getallen van de M4-mini: de opslag-
/// coprocessor heet ans@81600000 en woont op 0x481600000.
#[test]
fn ranges_translation() {
    let arm_io = n(
        "arm-io",
        vec![
            ("#address-cells", u32b(2)),
            ("#size-cells", u32b(2)),
            // bus 0x0 → ouder 0x4_0000_0000, venster 8 GB.
            (
                "ranges",
                cat(&[u64b(0), u64b(0x4_0000_0000), u64b(0x2_0000_0000)]),
            ),
        ],
        vec![
            n(
                "ans",
                vec![("reg", cat(&[u64b(0x8160_0000), u64b(0x8_8000)]))],
                vec![],
            ),
            n(
                "sart-ans",
                vec![("reg", cat(&[u64b(0x85c5_0000), u64b(0xc000)]))],
                vec![],
            ),
        ],
    );
    let root = n(
        "device-tree",
        vec![("#address-cells", u32b(2)), ("#size-cells", u32b(2))],
        vec![arm_io],
    );
    let b = root.encode();
    let t = Adt::new(&b).unwrap();
    assert_eq!(t.reg("/arm-io/ans", 0), Some((0x4_8160_0000, 0x8_8000)));
    assert_eq!(
        t.reg("/arm-io/sart-ans", 0).map(|r| r.0),
        Some(0x4_85c5_0000)
    );
}

/// Een adres buiten élk ranges-venster komt onvertaald terug, niet
/// stilletjes op een verkeerd adres.
#[test]
fn address_outside_ranges_stays_put() {
    let bus = n(
        "bus",
        vec![
            ("#address-cells", u32b(2)),
            ("#size-cells", u32b(2)),
            (
                "ranges",
                cat(&[u64b(0x1000), u64b(0x9_0000_0000), u64b(0x1000)]),
            ),
        ],
        vec![n(
            "dev",
            vec![("reg", cat(&[u64b(0xdead_0000), u64b(0x100)]))],
            vec![],
        )],
    );
    let root = n(
        "device-tree",
        vec![("#address-cells", u32b(2)), ("#size-cells", u32b(2))],
        vec![bus],
    );
    let b = root.encode();
    let t = Adt::new(&b).unwrap();
    assert_eq!(t.reg("/bus/dev", 0), Some((0xdead_0000, 0x100)));
}

/// Cellen van 32 bits (ac=1) komen op Apple-SoC's voor bij sommige bussen;
/// de lezer leest dan vier bytes, geen acht.
#[test]
fn single_cell_addresses() {
    let bus = n(
        "bus",
        vec![("#address-cells", u32b(1)), ("#size-cells", u32b(1))],
        vec![n(
            "dev",
            vec![("reg", cat(&[u32b(0x1234), u32b(0x40)]))],
            vec![],
        )],
    );
    let root = n(
        "device-tree",
        vec![("#address-cells", u32b(2)), ("#size-cells", u32b(2))],
        vec![bus],
    );
    let b = root.encode();
    let t = Adt::new(&b).unwrap();
    assert_eq!(t.reg("/bus/dev", 0), Some((0x1234, 0x40)));
}

/// Een propertymaat met bit 31 (de vlag van de firmware) telt als lengte
/// zonder die bit.
#[test]
fn size_flag_is_not_a_length() {
    let mut b = n("device-tree", vec![("x", u32b(7))], vec![]).encode();
    // De tweede property ("x") begint na de header en "name" (36 + 12).
    let size_at = NODE_HDR_LEN + PROP_HDR_LEN + 12 + PROP_NAME_LEN;
    b[size_at + 3] |= 0x80;
    let t = Adt::new(&b).unwrap();
    assert_eq!(t.u32(Node::ROOT, "x"), Some(7));
}

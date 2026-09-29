//! Een minimale, allocatievrije lezer van Apple's Device Tree (ADT): de
//! boom die iBoot op elke Apple-SoC achterlaat, en de enige bron voor wat
//! dit silicium écht bevat: welke cores er zijn en hoe je ze start, waar
//! de dockchannel zit, waar de opslag-coprocessor woont, welk MAC de NIC
//! draagt.
//!
//! Waarom naast [`crate::fdt`] en niet erin: het is een ánder formaat. FDT
//! is big-endian met een aparte stringtabel en tokens; de ADT is
//! little-endian, draagt de naam ván een node als property `name`, en
//! heeft geen tokens: een node is een telling van properties en kinderen,
//! en dan die properties en kinderen achter elkaar. Twee formaten in één
//! parser persen zou van beide een slechtere lezer maken.
//!
//! Bewust géén boom in geheugen: de ADT is 448 KB en HopOS leest er een
//! handvol waarden uit. Elke functie loopt de boom opnieuw af vanaf de
//! wortel; dat kost microseconden en scheelt een allocator. De blob is
//! onvertrouwde firmware-input: alles gaat met `get`, en een kromme ADT is
//! een `None`, geen panic. Ook de diepte is begrensd ([`MAX_DEPTH`]): de
//! Go-lezer liep de kinderen recursief af, en een kwaadaardige nesting is
//! dan een stack-overloop.
//!
//! Formaat (m1n1 `proxyclient/m1n1/adt.py`, de leidende referentie):
//!
//! ```text
//! node:     property_count u32, child_count u32, properties…, children…
//! property: name [32]byte, size u32 (bit 31 = vlag, niet de lengte),
//!           value [size]byte, uitgevuld tot een viervoud
//! ```
//!
//! Host-getest én op ijzer gelijk aan wat de Python-loader eruit haalde
//! (29-08), inclusief de ranges-vertaling:
//!
//! ```text
//! adt: /arm-io/dockchannel-uart   reg[0] 0x388128000+0x10000
//! adt: /arm-io/ans                reg[0] 0x481600000  reg[3] 0x485cc0000  reg[9] 0x4c5cc0000
//! adt: /arm-io/sart-ans           reg[0] 0x485c50000
//! ```
//!
//! Twee lessen van de Go-lezer die hier in de vorm zitten: een
//! property-waarde begint 36 bytes na zijn property (naam 32 + grootte 4)
//! en staat dus op een viervoud, niet op een achtvoud; en twee u16's in één
//! woord zijn één lees. Beide gaven op device-geheugen een alignment-fault
//! (29-08). Deze lezer werkt op een slice die het board uit Normal-geheugen
//! maakt, en leest elk getal bytegewijs: er bestaat hier geen scheve
//! woordlees meer.

use core::fmt;

const PROP_NAME_LEN: usize = 32;
const PROP_HDR_LEN: usize = PROP_NAME_LEN + 4;
const NODE_HDR_LEN: usize = 8;
/// Bit 31 van de grootte is een vlag van de firmware, geen lengte.
const SIZE_MASK: u32 = 0x7fff_ffff;

/// De grootste ADT die we wegen. Die van een M4 is ~448 KB; 8 MB is ruim
/// en houdt een kapotte grootte uit de lussen.
pub const MAX_TREE: usize = 8 << 20;
/// Zo diep nesten we hoogstens. De M4 komt tot een diepte van vijf
/// (`/arm-io/apcie/pci-bridge1/ethernet`); 32 is ruim.
pub const MAX_DEPTH: usize = 32;
/// De wortel mag niet meer properties of kinderen melden dan dit: een
/// grove plausibiliteitstoets op de header (Go: `Open`).
const MAX_ROOT_COUNT: u32 = 1024;

/// Waarom een blob geen ADT is.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// De blob is kleiner dan een node-header of groter dan [`MAX_TREE`].
    BadSize(usize),
    /// De wortel draagt geen plausibele header.
    BadRoot {
        /// Aantal properties dat de wortel meldt.
        props: u32,
        /// Aantal kinderen dat de wortel meldt.
        children: u32,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::BadSize(n) => write!(f, "adt: size {n} bytes is not a tree"),
            Self::BadRoot { props, children } => write!(
                f,
                "adt: root claims {props} properties and {children} children"
            ),
        }
    }
}

/// De `Result` van deze module.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Een node: de offset van zijn header in de blob. De wortel is 0.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Node(usize);

impl Node {
    /// De wortel.
    pub const ROOT: Node = Node(0);

    /// De offset in de blob.
    #[must_use]
    pub const fn offset(self) -> usize {
        self.0
    }
}

/// Eén property: naam (zonder de NUL-opvulling) en waarde.
#[derive(Copy, Clone, Debug)]
pub struct Prop<'a> {
    /// De naam, tot de eerste NUL.
    pub name: &'a [u8],
    /// De waarde, precies `size` bytes.
    pub value: &'a [u8],
}

/// De keten van een pad: de wortel eerst, de node zelf als laatste. De
/// ranges-vertaling heeft alle ouders nodig (m1n1's `adt_get_reg` werkt om
/// die reden op een pad en niet op een losse node).
#[derive(Copy, Clone, Debug)]
pub struct Chain {
    nodes: [Node; MAX_DEPTH],
    len: usize,
}

impl Chain {
    /// De nodes, wortel eerst.
    #[must_use]
    pub fn as_slice(&self) -> &[Node] {
        self.nodes.get(..self.len).unwrap_or_default()
    }

    /// De node aan het eind van de keten.
    #[must_use]
    pub fn node(&self) -> Node {
        self.as_slice().last().copied().unwrap_or(Node::ROOT)
    }
}

/// Een gewogen ADT: de enige plek waar deze firmware-input als geheel
/// gewogen wordt, zodat de lopers eronder alleen binnen de boom lopen.
#[derive(Copy, Clone, Debug)]
pub struct Adt<'a> {
    blob: &'a [u8],
}

/// Een little-endian u32 op `off`, bytegewijs gelezen.
fn le32(b: &[u8], off: usize) -> Option<u32> {
    let w = b.get(off..off.checked_add(4)?)?;
    Some(u32::from_le_bytes([w[0], w[1], w[2], w[3]]))
}

/// `n` cellen van 32 bits als één getal, laagste cel eerst (little-endian,
/// anders dan FDT). Meer dan twee cellen past niet in een u64.
fn cells(b: &[u8], off: usize, n: usize) -> Option<u64> {
    let mut v = 0u64;
    for i in 0..n.min(2) {
        v |= u64::from(le32(b, off + 4 * i)?) << (32 * i);
    }
    Some(v)
}

/// Vergelijkt een propertynaam uit de boom met `s`. Apple gebruikt in
/// ADT-namen zowel `-` als `_`; de firmware is daar niet consequent in, dus
/// gelden ze als gelijk, zoals in m1n1's Python-kant.
fn name_matches(n: &[u8], s: &str) -> bool {
    let norm = |c: u8| if c == b'_' { b'-' } else { c };
    n.len() == s.len() && n.iter().zip(s.bytes()).all(|(a, b)| norm(*a) == norm(b))
}

impl<'a> Adt<'a> {
    /// Weegt de blob: een maat tussen een node-header en [`MAX_TREE`], en een
    /// wortel met properties die binnen de boom zijn eigen header draagt.
    pub fn new(blob: &'a [u8]) -> Result<Self> {
        if blob.len() < NODE_HDR_LEN || blob.len() > MAX_TREE {
            return Err(Error::BadSize(blob.len()));
        }
        let t = Adt { blob };
        let (props, children) = t.counts(Node::ROOT).ok_or(Error::BadSize(blob.len()))?;
        if props == 0 || props > MAX_ROOT_COUNT || children > MAX_ROOT_COUNT {
            return Err(Error::BadRoot { props, children });
        }
        Ok(t)
    }

    /// De maat van de blob.
    #[must_use]
    pub fn size(&self) -> usize {
        self.blob.len()
    }

    /// De node-header op `n`: (properties, kinderen).
    fn counts(&self, n: Node) -> Option<(u32, u32)> {
        Some((le32(self.blob, n.0)?, le32(self.blob, n.0 + 4)?))
    }

    /// De property op offset `p` en de offset van de volgende.
    fn prop_at(&self, p: usize) -> Option<(Prop<'a>, usize)> {
        let raw = self.blob.get(p..p.checked_add(PROP_NAME_LEN)?)?;
        let nul = raw.iter().position(|&c| c == 0).unwrap_or(PROP_NAME_LEN);
        let size = (le32(self.blob, p + PROP_NAME_LEN)? & SIZE_MASK) as usize;
        let val = p + PROP_HDR_LEN;
        let value = self.blob.get(val..val.checked_add(size)?)?;
        let next = (val + size).next_multiple_of(4);
        Some((
            Prop {
                name: raw.get(..nul)?,
                value,
            },
            next,
        ))
    }

    /// Waar de kinderen van `n` beginnen.
    fn after_props(&self, n: Node) -> Option<usize> {
        let (props, _) = self.counts(n)?;
        let mut p = n.0 + NODE_HDR_LEN;
        for _ in 0..props {
            p = self.prop_at(p)?.1;
        }
        Some(p)
    }

    /// De offset net voorbij `n` (die van zijn volgende broer), zonder
    /// recursie: een stapel van resterende kinderen, begrensd op
    /// [`MAX_DEPTH`].
    fn node_end(&self, n: Node) -> Option<usize> {
        let mut left = [0u32; MAX_DEPTH];
        let mut depth = 0usize;
        let mut p = n.0;
        loop {
            let (_, children) = self.counts(Node(p))?;
            p = self.after_props(Node(p))?;
            *left.get_mut(depth)? = children;
            depth += 1;
            loop {
                let top = left.get_mut(depth.checked_sub(1)?)?;
                if *top > 0 {
                    *top -= 1;
                    break;
                }
                depth -= 1;
                if depth == 0 {
                    return Some(p);
                }
            }
            if p >= self.blob.len() {
                return None;
            }
        }
    }

    /// Alle properties van `n`, in boomvolgorde. Stopt stil bij een
    /// property die buiten de boom valt.
    pub fn props(&self, n: Node) -> impl Iterator<Item = Prop<'a>> + 'a {
        let t = *self;
        let count = self.counts(n).map_or(0, |c| c.0);
        let mut p = Some(n.0 + NODE_HDR_LEN);
        (0..count).map_while(move |_| {
            let (prop, next) = t.prop_at(p?)?;
            p = Some(next);
            Some(prop)
        })
    }

    /// De waarde van property `name` op `n`. Een lege waarde is geldig en
    /// betekent "deze vlag staat aan" (`nvme-secure-bar` op de M4).
    #[must_use]
    pub fn prop(&self, n: Node, name: &str) -> Option<&'a [u8]> {
        self.props(n)
            .find(|p| name_matches(p.name, name))
            .map(|p| p.value)
    }

    /// De naam van `n`: de property `name` tot de eerste NUL.
    #[must_use]
    pub fn name(&self, n: Node) -> Option<&'a str> {
        self.str(n, "name")
    }

    /// Een tekst-property tot de eerste NUL (`compatible`, `name`,
    /// `serial-number`); alleen als het UTF-8 is.
    #[must_use]
    pub fn str(&self, n: Node, name: &str) -> Option<&'a str> {
        let v = self.prop(n, name)?;
        let end = v.iter().position(|&c| c == 0).unwrap_or(v.len());
        core::str::from_utf8(v.get(..end)?).ok()
    }

    /// Een u32-property (de eerste vier bytes).
    #[must_use]
    pub fn u32(&self, n: Node, name: &str) -> Option<u32> {
        le32(self.prop(n, name)?, 0)
    }

    /// Een u64-property. Twee helften van 32 bits: de waarde staat op een
    /// viervoud, niet op een achtvoud (zie de module-doc, 29-08).
    #[must_use]
    pub fn u64(&self, n: Node, name: &str) -> Option<u64> {
        let v = self.prop(n, name)?;
        cells(v, 0, 2).filter(|_| v.len() >= 8)
    }

    /// De directe kinderen van `n`. Stopt stil bij een kind dat buiten de
    /// boom valt.
    pub fn children(&self, n: Node) -> impl Iterator<Item = Node> + 'a {
        let t = *self;
        let count = self.counts(n).map_or(0, |c| c.1);
        let mut p = self.after_props(n);
        (0..count).map_while(move |_| {
            let at = p?;
            if at >= t.blob.len() {
                return None;
            }
            p = t.node_end(Node(at));
            Some(Node(at))
        })
    }

    /// Het directe kind van `n` met naam `name` (exact).
    #[must_use]
    pub fn child(&self, n: Node, name: &str) -> Option<Node> {
        self.children(n).find(|c| self.name(*c) == Some(name))
    }

    /// De node op pad `path` (`/arm-io/uart0`); `/` is de wortel.
    #[must_use]
    pub fn path(&self, path: &str) -> Option<Node> {
        self.trace(path).map(|c| c.node())
    }

    /// De keten van `path`: de wortel en elke node onderweg.
    #[must_use]
    pub fn trace(&self, path: &str) -> Option<Chain> {
        let mut chain = Chain {
            nodes: [Node::ROOT; MAX_DEPTH],
            len: 1,
        };
        let mut n = Node::ROOT;
        for part in path.split('/').filter(|s| !s.is_empty()) {
            n = self.child(n, part)?;
            *chain.nodes.get_mut(chain.len)? = n;
            chain.len += 1;
        }
        Some(chain)
    }

    /// `#address-cells` en `#size-cells` van `n`, standaard 2 en 2 (wat elke
    /// Apple-SoC voert die we kennen).
    fn cell_counts(&self, n: Node) -> (usize, usize) {
        let get = |k| self.u32(n, k).map_or(2, |v| v as usize);
        (get("#address-cells"), get("#size-cells"))
    }

    /// Het `i`-de (adres, grootte)-paar van de laatste node van `chain`,
    /// vertaald naar een fysiek adres.
    ///
    /// Een reg-waarde in de ADT is RELATIEF aan de bus waar de node aan
    /// hangt, en de breedte van zijn velden komt van de OUDER. Wie de
    /// vertaling overslaat, krijgt een adres dat er plausibel uitziet en
    /// nergens heen wijst: de opslag-coprocessor heet `ans@81600000` en
    /// woont op `0x481600000`.
    #[must_use]
    pub fn reg_at(&self, chain: &Chain, i: usize) -> Option<(u64, u64)> {
        let nodes = chain.as_slice();
        let (&me, up) = nodes.split_last()?;
        let &parent = up.last()?;
        let (ac, sc) = self.cell_counts(parent);
        if ac == 0 || ac > 2 || sc > 2 {
            return None;
        }
        let reg = self.prop(me, "reg")?;
        let entry = (ac + sc) * 4;
        let off = i.checked_mul(entry)?;
        if reg.len() < off.checked_add(entry)? {
            return None;
        }
        let mut base = cells(reg, off, ac)?;
        let size = cells(reg, off + ac * 4, sc)?;
        // Omhoog door de keten: elke ouder met ranges vertaalt.
        for k in (1..up.len()).rev() {
            base = self.translate(*up.get(k)?, *up.get(k - 1)?, base)?;
        }
        Some((base, size))
    }

    /// Het gemak voor het gewone geval: pad opzoeken en het `i`-de venster
    /// vertaald teruggeven.
    #[must_use]
    pub fn reg(&self, path: &str, i: usize) -> Option<(u64, u64)> {
        self.reg_at(&self.trace(path)?, i)
    }

    /// Past de ranges van `node` toe op een busadres. Zonder ranges houdt de
    /// vertaling daar op (het adres is dan al dat van de ouder), en een adres
    /// buiten elk venster komt onvertaald terug: m1n1's `translate()`.
    /// `None` = cellen die niet te lezen zijn.
    fn translate(&self, node: Node, parent: Node, addr: u64) -> Option<u64> {
        let Some(ranges) = self.prop(node, "ranges") else {
            return Some(addr);
        };
        let (ac, sc) = self.cell_counts(node);
        let (pac, _) = self.cell_counts(parent);
        if ac == 0 || ac > 2 || sc > 2 || pac == 0 || pac > 2 {
            return None;
        }
        let entry = (ac + pac + sc) * 4;
        for o in (0..ranges.len() / entry).map(|k| k * entry) {
            let bus = cells(ranges, o, ac)?;
            let par = cells(ranges, o + ac * 4, pac)?;
            let size = cells(ranges, o + (ac + pac) * 4, sc)?;
            if addr >= bus && addr - bus < size {
                return Some(addr - bus + par);
            }
        }
        Some(addr)
    }
}

#[cfg(test)]
mod tests;

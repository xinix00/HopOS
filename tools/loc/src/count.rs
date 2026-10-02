//! De teller van één Rust-bron: welke regels code zijn en welke toets.
//!
//! Een regel telt als er na het schrappen van commentaar nog iets op staat.
//! De inhoud van een string telt mee (een `global_asm!`-blok is code; Go
//! telde zijn raw strings ook per regel). Een item onder `#[cfg(test)]` of
//! `#[test]` telt als toets: Go had zijn toetsen in `_test.go`-files die
//! `go list` niet noemde, Rust heeft ze in hetzelfde bestand.

use core::ops::AddAssign;

/// De telling van één bestand of één emmer.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct Count {
    /// Regels productiecode.
    pub(crate) code: usize,
    /// Regels onder `#[cfg(test)]` of `#[test]`.
    pub(crate) tests: usize,
}

impl AddAssign for Count {
    fn add_assign(&mut self, o: Self) {
        self.code += o.code;
        self.tests += o.tests;
    }
}

/// Telt een Rust-bron.
pub(crate) fn rust(src: &str) -> Count {
    let text = strip(src.as_bytes());
    let in_test = test_lines(&text);
    let mut c = Count::default();
    for (line, is_test) in text.split(|&b| b == b'\n').zip(in_test) {
        if line.iter().all(u8::is_ascii_whitespace) {
            continue;
        }
        if is_test {
            c.tests += 1;
        } else {
            c.code += 1;
        }
    }
    c
}

/// Zet commentaar om in spaties en string- en tekenliteralen in `x`;
/// regeleinden blijven staan. De uitkomst is even lang als de bron, dus
/// regel n is nog regel n, en een `{`, `;` of `#[cfg(test)]` die overblijft
/// is er echt één en staat niet in een string.
fn strip(src: &[u8]) -> Vec<u8> {
    let mut out = src.to_vec();
    let mut i = 0;
    while i < src.len() {
        i = if src[i..].starts_with(b"//") {
            let e = line_end(src, i);
            blank(&mut out[i..e], b' ');
            e
        } else if src[i..].starts_with(b"/*") {
            let e = block_end(src, i);
            blank(&mut out[i..e], b' ');
            e
        } else if let Some(e) = literal_end(src, i) {
            blank(&mut out[i..e], b'x');
            e
        } else {
            i + 1
        };
    }
    out
}

/// Vervangt alles behalve regeleinden door `with`.
fn blank(s: &mut [u8], with: u8) {
    for b in s.iter_mut().filter(|b| **b != b'\n') {
        *b = with;
    }
}

/// Het eerste regeleinde op of na `i`, of het einde.
fn line_end(src: &[u8], i: usize) -> usize {
    src[i..]
        .iter()
        .position(|&b| b == b'\n')
        .map_or(src.len(), |p| i + p)
}

/// Het einde van het blokcommentaar dat op `i` begint; Rust nest ze.
fn block_end(src: &[u8], i: usize) -> usize {
    let mut depth = 0usize;
    let mut j = i;
    while j < src.len() {
        if src[j..].starts_with(b"/*") {
            depth += 1;
            j += 2;
        } else if src[j..].starts_with(b"*/") {
            depth -= 1;
            j += 2;
            if depth == 0 {
                return j;
            }
        } else {
            j += 1;
        }
    }
    src.len()
}

fn is_ident(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b == b'_'
}

/// Het einde van de string- of tekenliteraal die op `i` begint, of None als
/// daar geen literaal begint (ook niet bij een lifetime `'a`). Voorvoegsels:
/// `b`, `c`, `r`, `br`, `cr`; midden in een naam (`for`, `rx`) begint niets.
fn literal_end(src: &[u8], i: usize) -> Option<usize> {
    if i > 0 && is_ident(src[i - 1]) {
        return None;
    }
    let mut j = i;
    if matches!(src.get(j), Some(b'b' | b'c')) {
        j += 1;
    }
    if src.get(j) == Some(&b'r') {
        j += 1;
        let mut hashes = 0;
        while src.get(j) == Some(&b'#') {
            hashes += 1;
            j += 1;
        }
        if src.get(j) != Some(&b'"') {
            return None;
        }
        return Some(raw_end(src, j + 1, hashes));
    }
    match src.get(j) {
        Some(b'"') => Some(string_end(src, j)),
        Some(b'\'') => char_end(src, j),
        _ => None,
    }
}

/// Het einde van een raw string: `"` gevolgd door `hashes` keer `#`.
fn raw_end(src: &[u8], from: usize, hashes: usize) -> usize {
    let mut j = from;
    while j < src.len() {
        if src[j] == b'"'
            && src[j + 1..]
                .iter()
                .take(hashes)
                .filter(|&&b| b == b'#')
                .count()
                == hashes
        {
            return j + 1 + hashes;
        }
        j += 1;
    }
    src.len()
}

/// Het einde van een gewone string die op `q` (de `"`) begint.
fn string_end(src: &[u8], q: usize) -> usize {
    let mut j = q + 1;
    while j < src.len() {
        match src[j] {
            b'\\' => j += 2,
            b'"' => return j + 1,
            _ => j += 1,
        }
    }
    src.len()
}

/// Het einde van een tekenliteraal die op `q` (de `'`) begint, of None als
/// het een lifetime is: `'a'` is een teken, `'a>` niet. Een escape
/// (`'\n'`, `'\''`, `'\u{1F600}'`) sluit binnen twaalf tekens.
fn char_end(src: &[u8], q: usize) -> Option<usize> {
    let c = *src.get(q + 1)?;
    if c == b'\\' {
        let from = q + 3;
        let to = (q + 14).min(src.len());
        return src
            .get(from..to)?
            .iter()
            .position(|&b| b == b'\'')
            .map(|p| from + p + 1);
    }
    if c == b'\'' || c == b'\n' {
        return None;
    }
    let len = utf8_len(c);
    (src.get(q + 1 + len) == Some(&b'\'')).then_some(q + 2 + len)
}

/// De lengte van een UTF-8-teken aan zijn eerste byte.
fn utf8_len(lead: u8) -> usize {
    match lead {
        0x00..=0x7F => 1,
        0xC0..=0xDF => 2,
        0xE0..=0xEF => 3,
        _ => 4,
    }
}

/// Welke regels onder `#[cfg(test)]` of `#[test]` staan: het attribuut en
/// het item erna, tot zijn sluitaccolade of zijn `;` op diepte nul. Een
/// `mod tests;` in een eigen bestand komt hier niet langs: dat bestand
/// linkt alleen in een testbuild en staat dus in geen enkele dep-info.
fn test_lines(text: &[u8]) -> Vec<bool> {
    let mut marks = vec![false; text.iter().filter(|&&b| b == b'\n').count() + 1];
    for pat in [&b"#[cfg(test)]"[..], b"#[test]"] {
        let mut from = 0;
        while let Some(p) = find(&text[from..], pat).map(|p| p + from) {
            let end = item_end(text, p + pat.len());
            for m in &mut marks[line_of(text, p)..=line_of(text, end)] {
                *m = true;
            }
            from = p + pat.len();
        }
    }
    marks
}

/// De eerste plek van `pat` in `text`.
fn find(text: &[u8], pat: &[u8]) -> Option<usize> {
    text.windows(pat.len()).position(|w| w == pat)
}

/// De regel (vanaf nul) waar byte `pos` op staat.
fn line_of(text: &[u8], pos: usize) -> usize {
    text[..pos.min(text.len())]
        .iter()
        .filter(|&&b| b == b'\n')
        .count()
}

/// De byte die het item sluit dat na `from` begint: de `}` die zijn eerste
/// `{` sluit, of een `;` op diepte nul daarvóór.
fn item_end(text: &[u8], from: usize) -> usize {
    let mut depth = 0usize;
    for (k, &b) in text.iter().enumerate().skip(from) {
        match b {
            b'{' => depth += 1,
            b'}' => {
                depth = depth.saturating_sub(1);
                if depth == 0 {
                    return k;
                }
            }
            b';' if depth == 0 => return k,
            _ => {}
        }
    }
    text.len()
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE: &str = r##"//! Module.
use core::fmt; // trailing

/* block
   comment */
/// Doc.
pub fn f<'a>(s: &'a str) -> char {
    let q = '{'; let e = '\''; let n = '\n';
    let raw = r#"not a } comment // here"#;
    let multi = "line one
line two";
    /* nested /* deep */ still */ q
}

#[cfg(test)]
mod tests {
    #[test]
    fn t() { assert_eq!(1, 1); }
}
"##;

    #[test]
    fn sample_counts_code_and_tests() {
        assert_eq!(rust(SAMPLE), Count { code: 8, tests: 5 });
    }

    #[test]
    fn single_item_under_cfg_test() {
        let src = "#[cfg(test)]\nuse std::fmt;\nfn f() {}\n";
        assert_eq!(rust(src), Count { code: 1, tests: 2 });
    }

    #[test]
    fn literals() {
        assert_eq!(literal_end(b"'a>", 0), None);
        assert_eq!(literal_end(b"'static", 0), None);
        assert_eq!(literal_end(b"'a'", 0), Some(3));
        assert_eq!(literal_end(b"'\\n'", 0), Some(4));
        assert_eq!(literal_end(b"'\\''", 0), Some(4));
        assert_eq!(literal_end("'é'".as_bytes(), 0), Some(4));
        assert_eq!(literal_end(b"b'x'", 0), Some(4));
        assert_eq!(literal_end(b"\"a\\\"b\"", 0), Some(6));
        assert_eq!(literal_end(b"b\"ab\"", 0), Some(5));
        assert_eq!(literal_end(br##"r#"a"#"##, 0), Some(6));
        assert_eq!(literal_end(b"rx\"", 0), None);
        assert_eq!(literal_end(b"for\"", 2), None);
    }

    #[test]
    fn strip_keeps_length_and_lines() {
        let out = strip(SAMPLE.as_bytes());
        assert_eq!(out.len(), SAMPLE.len());
        assert_eq!(
            out.iter().filter(|&&b| b == b'\n').count(),
            SAMPLE.matches('\n').count()
        );
    }
}

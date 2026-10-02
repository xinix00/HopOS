//! Een minimale JSON-lezer voor de berichten van `cargo --message-format=json`.
//! Cargo schrijft één bericht per regel en de meter leest er vijf velden
//! uit; dat is geen reden voor een crate van buiten (handboek §8).

/// Eén JSON-waarde.
#[derive(Clone, Debug, PartialEq)]
pub(crate) enum Json {
    Null,
    Bool(bool),
    Num(f64),
    Str(String),
    Arr(Vec<Json>),
    Obj(Vec<(String, Json)>),
}

impl Json {
    /// Het veld `key` van een object.
    pub(crate) fn get(&self, key: &str) -> Option<&Json> {
        match self {
            Json::Obj(f) => f.iter().find(|(k, _)| k == key).map(|(_, v)| v),
            _ => None,
        }
    }

    /// De tekst van een string.
    pub(crate) fn str(&self) -> Option<&str> {
        match self {
            Json::Str(s) => Some(s),
            _ => None,
        }
    }

    /// De elementen van een array.
    pub(crate) fn arr(&self) -> Option<&[Json]> {
        match self {
            Json::Arr(a) => Some(a),
            _ => None,
        }
    }
}

/// Leest één JSON-tekst.
pub(crate) fn parse(text: &str) -> Result<Json, String> {
    let mut p = Parser {
        src: text.as_bytes(),
        pos: 0,
    };
    let v = p.value()?;
    p.skip_ws();
    if p.pos != p.src.len() {
        return Err(p.err("trailing data"));
    }
    Ok(v)
}

struct Parser<'a> {
    src: &'a [u8],
    pos: usize,
}

impl Parser<'_> {
    fn err(&self, what: &str) -> String {
        format!("json: {what} at byte {}", self.pos)
    }

    fn skip_ws(&mut self) {
        while matches!(self.peek(), Some(b' ' | b'\t' | b'\n' | b'\r')) {
            self.pos += 1;
        }
    }

    fn peek(&self) -> Option<u8> {
        self.src.get(self.pos).copied()
    }

    fn next(&mut self) -> Result<u8, String> {
        let b = self.peek().ok_or_else(|| self.err("unexpected end"))?;
        self.pos += 1;
        Ok(b)
    }

    fn eat(&mut self, lit: &str) -> bool {
        let hit = self.src[self.pos..].starts_with(lit.as_bytes());
        if hit {
            self.pos += lit.len();
        }
        hit
    }

    fn value(&mut self) -> Result<Json, String> {
        self.skip_ws();
        match self.peek() {
            Some(b'{') => self.object(),
            Some(b'[') => self.array(),
            Some(b'"') => self.string().map(Json::Str),
            Some(b't') if self.eat("true") => Ok(Json::Bool(true)),
            Some(b'f') if self.eat("false") => Ok(Json::Bool(false)),
            Some(b'n') if self.eat("null") => Ok(Json::Null),
            Some(b'-' | b'0'..=b'9') => self.number(),
            _ => Err(self.err("unexpected byte")),
        }
    }

    fn object(&mut self) -> Result<Json, String> {
        self.pos += 1;
        let mut fields = Vec::new();
        loop {
            self.skip_ws();
            if self.peek() == Some(b'}') {
                self.pos += 1;
                return Ok(Json::Obj(fields));
            }
            let key = self.string()?;
            self.skip_ws();
            if self.next()? != b':' {
                return Err(self.err("expected ':'"));
            }
            fields.push((key, self.value()?));
            self.skip_ws();
            match self.next()? {
                b',' => {}
                b'}' => return Ok(Json::Obj(fields)),
                _ => return Err(self.err("expected ',' or '}'")),
            }
        }
    }

    fn array(&mut self) -> Result<Json, String> {
        self.pos += 1;
        let mut items = Vec::new();
        loop {
            self.skip_ws();
            if self.peek() == Some(b']') {
                self.pos += 1;
                return Ok(Json::Arr(items));
            }
            items.push(self.value()?);
            self.skip_ws();
            match self.next()? {
                b',' => {}
                b']' => return Ok(Json::Arr(items)),
                _ => return Err(self.err("expected ',' or ']'")),
            }
        }
    }

    fn string(&mut self) -> Result<String, String> {
        if self.next()? != b'"' {
            return Err(self.err("expected string"));
        }
        let mut out = Vec::new();
        loop {
            match self.next()? {
                b'"' => break,
                b'\\' => {
                    let e = self.next()?;
                    let c = match e {
                        b'n' => b'\n',
                        b't' => b'\t',
                        b'r' => b'\r',
                        b'b' => 8,
                        b'f' => 12,
                        b'"' | b'\\' | b'/' => e,
                        b'u' => {
                            let ch = self.unicode()?;
                            out.extend_from_slice(ch.encode_utf8(&mut [0; 4]).as_bytes());
                            continue;
                        }
                        _ => return Err(self.err("bad escape")),
                    };
                    out.push(c);
                }
                b => out.push(b),
            }
        }
        String::from_utf8(out).map_err(|_| self.err("string is not UTF-8"))
    }

    /// De vier hexcijfers na `\u`, met een tweede `\u` als het een
    /// surrogaatpaar is.
    fn unicode(&mut self) -> Result<char, String> {
        let hi = self.hex4()?;
        let code = if (0xD800..0xDC00).contains(&hi) && self.eat("\\u") {
            let lo = self.hex4()?;
            0x10000 + ((hi - 0xD800) << 10) + (lo.wrapping_sub(0xDC00) & 0x3FF)
        } else {
            hi
        };
        char::from_u32(code).ok_or_else(|| self.err("bad code point"))
    }

    fn hex4(&mut self) -> Result<u32, String> {
        let digits = self
            .src
            .get(self.pos..self.pos + 4)
            .and_then(|d| core::str::from_utf8(d).ok())
            .ok_or_else(|| self.err("short \\u escape"))?;
        let v = u32::from_str_radix(digits, 16).map_err(|_| self.err("bad \\u escape"))?;
        self.pos += 4;
        Ok(v)
    }

    fn number(&mut self) -> Result<Json, String> {
        let start = self.pos;
        while matches!(
            self.peek(),
            Some(b'0'..=b'9' | b'-' | b'+' | b'.' | b'e' | b'E')
        ) {
            self.pos += 1;
        }
        core::str::from_utf8(&self.src[start..self.pos])
            .ok()
            .and_then(|s| s.parse().ok())
            .map(Json::Num)
            .ok_or_else(|| self.err("bad number"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cargo_message() {
        let line = r#"{"reason":"compiler-artifact","package_id":"path+file:///x/abi#3.0.0","manifest_path":"/x/abi/Cargo.toml","target":{"kind":["lib"],"name":"abi"},"filenames":["/x/target/deps/libabi-1.rlib","/x/target/deps/libabi-1.rmeta"],"fresh":true,"n":1.5e3}"#;
        let m = parse(line).unwrap_or(Json::Null);
        assert_eq!(
            m.get("reason").and_then(Json::str),
            Some("compiler-artifact")
        );
        assert_eq!(
            m.get("target")
                .and_then(|t| t.get("name"))
                .and_then(Json::str),
            Some("abi")
        );
        assert_eq!(
            m.get("filenames").and_then(Json::arr).map(<[Json]>::len),
            Some(2)
        );
        assert_eq!(m.get("fresh"), Some(&Json::Bool(true)));
        assert_eq!(m.get("n"), Some(&Json::Num(1500.0)));
        assert_eq!(m.get("missing"), None);
    }

    #[test]
    fn escapes_and_nesting() {
        let m = parse(r#" [ "a\"b\\c\n", "\u00e9\ud83d\ude00", {"k": [null, false, -2]}, {} ] "#)
            .unwrap_or(Json::Null);
        let a = m.arr().unwrap_or(&[]);
        assert_eq!(a[0].str(), Some("a\"b\\c\n"));
        assert_eq!(a[1].str(), Some("é😀"));
        assert_eq!(
            a[2].get("k").and_then(Json::arr).map(<[Json]>::len),
            Some(3)
        );
        assert_eq!(a[3], Json::Obj(Vec::new()));
    }

    #[test]
    fn errors() {
        assert!(parse("{\"a\":1} x").is_err());
        assert!(parse("[1,").is_err());
        assert!(parse("\"open").is_err());
        assert!(parse("tru").is_err());
    }
}

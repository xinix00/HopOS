//! De platform-config: de tekst waarmee een operator een node instelt
//! zonder te herbouwen.
//!
//! Eén parser voor élk kanaal waarlangs die tekst binnenkomt (de
//! firmware-FAT, de initramfs-regio, het ingebakken blob, de
//! kernel-cmdline), omdat drie iets verschillende lezingen van hetzelfde
//! bestand precies de klasse fout opleveren die je bij boot niet ziet: de
//! node komt op, maar met een ándere config dan er staat.
//!
//! Twee formaten, en dat verschil is echt:
//!
//! - [`all`]: het CONFIGBESTAND (`hopos.cfg`). Eén `key=value` per regel,
//!   `#` vooraan is commentaar, en een waarde mag spaties bevatten.
//! - [`cmdline`]: de KERNEL-CMDLINE (/chosen/bootargs). Eén regel met
//!   whitespace-gescheiden tokens, dus een waarde heeft nooit een spatie.
//!
//! Waarom [`all`] niet op whitespace splitst: dan wordt
//! `# hopos.insecure=1` twee tokens, "#" en "hopos.insecure=1", en dat
//! tweede token matcht de sleutel gewoon. Een uitgecommentarieerde regel
//! MET spatie achter de # zette in de Go-kern zo de API open. Een
//! tekstformaat waarin een spatie een auth-poort opent is geen formaat om
//! op te vertrouwen.
//!
//! Geen allocatie: de waarden zijn slices van de tekst.

/// Alle waarden van `key` uit een configbestand, in bestandsvolgorde.
/// Enkelvoudige sleutels (`hopos.node`) hebben er één, herhaalde
/// (`hopos.init[]`) meerdere.
pub fn all<'a>(text: &'a str, key: &'a str) -> impl Iterator<Item = &'a str> + 'a {
    text.split('\n').filter_map(move |line| {
        let line = line.strip_suffix('\r').unwrap_or(line).trim();
        if line.is_empty() || line.starts_with('#') {
            return None;
        }
        let v = line.strip_prefix(key)?.strip_prefix('=')?;
        Some(v.trim())
    })
}

/// Alle waarden van `key` uit een kernel-cmdline-regel. Niet-hopos-tokens
/// (Linux-restanten op de kaart) vallen vanzelf af.
pub fn cmdline<'a>(args: &'a str, key: &'a str) -> impl Iterator<Item = &'a str> + 'a {
    args.split_ascii_whitespace()
        .filter_map(move |tok| tok.strip_prefix(key)?.strip_prefix('='))
}

/// De eerste waarde, of "": de enkelvoudige-sleutel-vorm.
pub fn first<'a>(mut values: impl Iterator<Item = &'a str>) -> &'a str {
    values.next().unwrap_or("")
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Het configbestand: één sleutel per regel, commentaar weg, waarden mét
    /// spaties intact.
    #[test]
    fn all_reads_the_file_format() {
        let text = "# HopOS node config\r\n\nhopos.node=hopos-lrv\nhopos.insecure=1\n\
            hopos.init[]={\"name\":\"welcome\",\"ports\":{\"http\":80}}\n\
            hopos.init[]={\"name\":\"tweede\", \"cmd\":\"met spaties\"}\n   hopos.console=5555\n";
        assert_eq!(all(text, "hopos.node").collect::<Vec<_>>(), ["hopos-lrv"]);
        let inits: Vec<_> = all(text, "hopos.init[]").collect();
        assert_eq!(inits.len(), 2);
        // Een waarde is de REST VAN DE REGEL.
        assert_eq!(inits[1], r#"{"name":"tweede", "cmd":"met spaties"}"#);
        // Ingesprongen regels werken; een bestand is geen assembly.
        assert_eq!(all(text, "hopos.console").collect::<Vec<_>>(), ["5555"]);
        assert_eq!(all(text, "hopos.cluster").count(), 0);
        // Een sleutel die een prefix van een andere is, matcht die niet.
        assert_eq!(all("hopos.nodes=x\n", "hopos.node").count(), 0);
    }

    /// DE REGRESSIE: een uitgecommentarieerde regel is commentaar, óók met
    /// een spatie achter de #.
    #[test]
    fn comment_with_a_space_is_not_config() {
        for text in [
            "# hopos.insecure=1\n",
            "#hopos.insecure=1\n",
            "#\thopos.insecure=1\n",
            "   # hopos.insecure=1\n",
        ] {
            assert_eq!(all(text, "hopos.insecure").count(), 0, "{text:?}");
        }
        assert_eq!(first(all("hopos.insecure=1\n", "hopos.insecure")), "1");
    }

    /// De cmdline: tokens op één regel, Linux-restanten negeren.
    #[test]
    fn cmdline_reads_tokens() {
        let args = "console=serial0,115200 root=/dev/mmcblk0p2 hopos.node=hop-1 \
            hopos.init[]={\"name\":\"a\"} hopos.init[]={\"name\":\"b\"}";
        assert_eq!(first(cmdline(args, "hopos.node")), "hop-1");
        assert_eq!(cmdline(args, "hopos.init[]").count(), 2);
        assert_eq!(cmdline(args, "hopos.cores").count(), 0);
    }

    #[test]
    fn first_of_nothing_is_empty() {
        assert_eq!(first(core::iter::empty()), "");
        assert_eq!(first(["a", "b"].into_iter()), "a");
    }
}

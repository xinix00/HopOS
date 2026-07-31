// Package bootcfg leest de platform-config: de tekst waarmee een operator een
// node instelt zonder te herbouwen. Eén parser voor élk kanaal waarlangs die
// tekst binnenkomt — de firmware-FAT (UEFI-stub), de initramfs-regio (Pi), het
// ingebakken blob (LicheeRV) en de kernel-cmdline (Pi) — omdat drie iets
// verschillende lezingen van hetzelfde bestand precies de klasse fout oplevert
// die je bij boot niet ziet: de node komt op, maar met een ándere config dan er
// staat.
//
// Er zijn twee formaten, en dat verschil is echt:
//
//   - All: het CONFIGBESTAND (hopos.cfg). Eén `key=value` per regel, `#` in de
//     eerste kolom is commentaar, en een waarde mag spaties bevatten — het is
//     een bestand, geen commandoregel. Dit is het formaat van élke
//     config-template in image/.
//   - Cmdline: de KERNEL-CMDLINE (/chosen/bootargs, cmdline.txt). Eén regel met
//     whitespace-gescheiden tokens, dus een waarde kan er per definitie geen
//     spatie in hebben. Alleen de Pi heeft dit kanaal, naast zijn bestand.
//
// Waarom All géén Fields gebruikt (de vorm die de UEFI-stub en het blob hadden):
// op whitespace splitsen maakt van
//
//	# hopos.insecure=1
//
// twee tokens — "#" en "hopos.insecure=1" — en dat tweede token matcht de
// sleutel gewoon. Een uitgecommentarieerde regel MET spatie achter de # zette de
// API dus open. Dat de templates hun sleutels bewust zonder spatie
// uitcommentariëren (`#hopos.insecure=1`) verborg het; een tekstformaat waarin
// een spatie een auth-poort opent is geen formaat om op te vertrouwen.
package bootcfg

import "strings"

// All geeft alle waarden van key uit een configbestand, in bestandsvolgorde.
// Enkelvoudige sleutels (hopos.node) hebben er één, herhaalde (hopos.init[])
// meerdere. Leeg/nil = niet gezet.
func All(text, key string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

// Cmdline geeft alle waarden van key uit een kernel-cmdline-regel. Tokens zijn
// whitespace-gescheiden, dus een waarde bevat nooit een spatie; niet-hopos-
// tokens (Linux-restanten op de kaart) vallen vanzelf af.
func Cmdline(args, key string) []string {
	var out []string
	for _, tok := range strings.Fields(args) {
		if v, ok := strings.CutPrefix(tok, key+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

// First geeft de eerste waarde van vs, of "" — de enkelvoudige-sleutel-vorm.
func First(vs []string) string {
	if len(vs) > 0 {
		return vs[0]
	}
	return ""
}

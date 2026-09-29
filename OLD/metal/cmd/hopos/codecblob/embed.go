//go:build embedcodec

package codecblob

import _ "embed"

// Deze bestanden zet image/flip-bundle.sh hier neer (FWDIR=...). Ze zijn
// gitignored: de firmware is van CIX en hoort niet in onze geschiedenis.
//
//go:embed hevcdec.fwb
var hevcdec []byte

//go:embed clip.hevc
var testclip []byte

func firmware(name string) []byte {
	if name == "hevcdec" {
		return hevcdec
	}
	return nil
}

func clip() []byte { return testclip }

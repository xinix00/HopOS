//go:build embedcfg

package cfgblob

import _ "embed"

// hopos.cfg staat hier door image/licheerv-agent.sh (uit image/hopos-headless.cfg
// of uit een eigen CFG=...) en is gitignored: er kan een echte apikey in staan.
//
//go:embed hopos.cfg
var text string

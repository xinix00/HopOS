package hop

import "github.com/xinix00/HopOS/metal/board/licheerv"

// hopCoreMirror is licheerv.HopHart (= layout.HopCore) als DATA-woord: de
// cpuinit-asm kan geen Go-const lezen maar wél een geïnitialiseerde var —
// zo bestaat er precies één bron voor de knop.
var hopCoreMirror uint64 = licheerv.HopHart

//go:build linkramsize

package licheerv

// De S-mode-cpuinit van slotstart hoort alleen bij APP-images (die bouwen
// met -tags linkramsize); de KERN heeft zijn eigen M-mode-cpuinit met de
// boot-hart-loterij (hop/cpuinit_riscv64.s) en die twee mogen nooit samen
// linken — vandaar deze tag-scheiding (gemeten 16-08: duplicated symbol).
import _ "github.com/xinix00/HopOS/metal/cpu/slotstart" // levert cpuinit (-tags linkcpuinit)

//go:build embedcagestub

package cagestub

import _ "embed"

// stub-slot.bin staat hier door image/licheerv-agent.sh (riscv64-elf-as +
// objcopy) en is gitignored: het is een build-artefact, net als de
// apploader-blob.
//
//go:embed stub-slot.bin
var stub []byte

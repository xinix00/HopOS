// Package cfgblob draagt de platform-config ín de node-binary, voor boards die
// hun bootmedium niet kunnen lezen.
//
// Op elk ander board komt de config van buiten het image: de Pi's lezen hem uit
// de device-tree die de firmware doorgeeft, de UEFI-stub leest hopos.cfg van de
// ESP. De LicheeRV Nano heeft geen van beide — de vendor-FSBL geeft ons niets
// mee, en de FAT-bootpartitie waar fip.bin op staat kunnen we zelf niet lezen
// (dat vraagt een SDHCI- plus FAT-driver; de FSBL las de kaart, wij niet).
//
// Dus bakken we hem in, net als de andere build-input-blobs (kern/cagestub):
// image/licheerv-agent.sh zet de gekozen config op de embed-plek (gitignored,
// want daar kan een echte apikey in staan) en bouwt met -tags embedcfg. De
// config is daarmee deel van het ondertekende image — hetzelfde
// update-mechanisme als de rest: nieuw image = nieuwe config.
//
// Zonder de tag is Text leeg en gedraagt het board zich als voorheen. Zodra dit
// board wél van de FAT kan lezen hoort dat pad de voorkeur te krijgen (een kaart
// in een laptop wijzigen is nu eenmaal makkelijker dan een image bouwen) en
// blijft dit de terugval.
package cfgblob

import "github.com/xinix00/HopOS/metal/fw/bootcfg"

// Text is de ingebakken config in het gewone hopos.cfg-formaat (key=value per
// regel, #-regels zijn commentaar). Leeg = geen ingebakken config.
var Text = text

// All geeft alle waarden van een sleutel — de bootParamAll-vorm die de main
// verwacht. Exact dezelfde lezing als de Pi en de UEFI-stub op hún hopos.cfg
// doen: één parser (fw/bootcfg), dus geen board waar hetzelfde bestand iets
// anders betekent.
func All(key string) []string { return bootcfg.All(Text, key) }

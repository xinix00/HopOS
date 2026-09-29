// Package codecblob draagt een codec-firmware en een teststream ín de
// node-binary. Tijdelijk, en met een duidelijke reden.
//
// De firmware van de VPU hoort bij de NODE en niet bij de kern: hij is van
// CIX, hij verandert niet mee met onze code, en een kern-flip zou hem elke
// keer opnieuw meesjouwen. De plek is dus /firmware op het volume.
//
// Alleen: er is nog geen weg om bestanden op HOP's eigen volume te krijgen.
// De download-op is ooit gesloopt omdat HOP daarmee met zijn volle rechten
// een door een app gekozen URL opende (SSRF), en die beslissing staat nog
// steeds. Tot er een nette weg is — een bestand naast de kern op de ESP, of
// een expliciete operator-actie — bakken we wat de bring-up nodig heeft in,
// achter een build-tag die alleen een meetbundel zet.
//
// Bouwen met: FWDIR=<map> image/flip-bundle.sh o6n   (zie het script)
package codecblob

// Firmware geeft de ingebakken firmware voor een codecnaam ("hevcdec"), of
// nil als deze bundel er geen draagt.
func Firmware(name string) []byte { return firmware(name) }

// Clip geeft de ingebakken teststream, of nil.
func Clip() []byte { return clip() }

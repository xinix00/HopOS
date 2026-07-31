// Package conlog bewaart de laatste console-uitvoer van de node in RAM, zodat
// die over het netwerk op te vragen is.
//
// Bestaansreden: op een headless board was de seriële console de énige plek waar
// HOP zijn eigen redenering kwijt kon — en dat is een slecht kanaal. Er hoeft
// maar geen kabel aan te zitten en de reden waarom een job niet start is
// onvindbaar; zit er wél een kabel aan, dan verliest de lijn bytes op 115200
// (gemeten 31-07: een misa-waarde kwam als "rv128 …0094112d" binnen, alleen te
// lezen doordat de extensieletters de hex bevestigden). Een node hoort te kunnen
// vertellen wat er net gebeurde zonder dat iemand er fysiek bij zit.
//
// Waarom een byte-ring en geen regels-met-tijdstempel: dit vult zich vanuit
// printk, dus vanuit élk pad — inclusief het panic-pad, waar de scheduler niet
// meer draait en alloceren de node kan ophangen. Deze kant alloceert daarom
// niets en neemt geen enkel slot; het kost één store per byte, en dat is niets
// naast de UART-poll die er direct achter zit (~87µs per byte op 115200).
//
// Gevolg van diezelfde keuze: de bewaartermijn is niet in tijd uitgedrukt maar
// in bytes. Een venster van vijf minuten kan leeg zijn precies wanneer je het
// nodig hebt; de laatste 32KB is altijd de meest recente context, en dat is bij
// het tempo waarop HOP logt in de praktijk véél meer dan vijf minuten.
package conlog

// Size is de ring: 32KB. Groot genoeg voor een hele boot plus een restart-storm,
// klein genoeg om op elk board (de kleinste heeft 256MB) niet uit te maken.
const Size = 32 << 10

var (
	buf [Size]byte

	// pos is het aantal bytes dat ooit langskwam; de index is pos&(Size-1).
	// Bewust geen atomic en geen lock: twee harten die tegelijk schrijven
	// kunnen elkaars byte overschrijven, en dat is precies wat de UART zélf
	// ook doet — regels van twee schrijvers lopen dan door elkaar. Wat NIET
	// mag gebeuren is dat de console de node ophoudt, en dat is de reden dat
	// hier geen slot zit (zie board/licheerv/console.go voor die les).
	pos uint64
)

// Put legt één console-byte in de ring. Aangeroepen uit printk, dus uit élk pad.
func Put(c byte) {
	buf[pos&(Size-1)] = c
	pos++
}

// Snapshot geeft de bewaarde console-uitvoer in de juiste volgorde. Dit is de
// enige kant die alloceert, en hij wordt alleen door een lezer aangeroepen (de
// API-handler) — nooit uit printk.
func Snapshot() []byte {
	n := pos
	if n > Size {
		n = Size
	}
	out := make([]byte, n)
	start := pos - n
	for i := range n {
		out[i] = buf[(start+i)&(Size-1)]
	}
	return out
}

// Dropped meldt hoeveel bytes er niet meer in de ring staan — nul zolang de
// hele geschiedenis past. Zonder dit getal leest een afgekapte snapshot als een
// complete: de eerste regel is dan stil een halve regel. Het is tegelijk de
// startpositie voor een verse meelezer (zie Since).
func Dropped() uint64 {
	if pos <= Size {
		return 0
	}
	return pos - Size
}

// Since geeft de bytes ná positie from, plus de positie waar de lezer dan staat.
// Zo kan een meelezer (net/conport) de console volgen zonder dat conlog iets van
// hem hoeft te weten: hij houdt zijn eigen plek bij, en meerdere lezers zitten
// elkaar niet in de weg.
//
// Is from te ver achtergebleven — de ring liep rond terwijl hij niet keek — dan
// begint hij bij het oudste dat er nog is. Dan mist hij bytes, en dat is eerlijk:
// de ring is per definitie de láátste Size bytes.
func Since(from uint64) (data []byte, next uint64) {
	oldest := Dropped()
	if from < oldest {
		from = oldest
	}
	n := pos - from
	if n == 0 {
		return nil, from
	}
	out := make([]byte, n)
	for i := range n {
		out[i] = buf[(from+i)&(Size-1)]
	}
	return out, from + n
}

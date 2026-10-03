# decode

Een stream door de hardwaredecoder van de node, met de fps erbij. De
kleinste app die de codec-dienst gebruikt zoals Lumen dat doet: een sessie
openen met `applib::codec`, bitstream in de eigen partitie schrijven en
voeren, beeldbuffers aanbieden, events ophalen en tellen. Er gaat geen beeld
over de verbinding: een buffer is een stuk van de eigen partitie, de kern
hangt het in de page tables van de codec (docs/media.md).

Aan het eind één regel op de console van de kern (de vorm; op ijzer nog
niet gemeten):

```
slot 2: decode: <n> frames <w>x<h> p010 from /data/clip.hevc (<MB> MB) in <ms> ms: <fps> fps, <MB/s> MB/s through the grant HOPOS_DECODE fps=<fps> MBps=<MB/s>
```

De lat is die van docs/media.md: 24 fps 4K P010 is 597 MB/s door de grant.

Daarna blijft de app staan (geen herstartlus bij Hop); weg is `DELETE`.

## De env

| Sleutel | Standaard | Wat |
| --- | --- | --- |
| `DECODE_FILE` | `/data/clip.hevc` | de stream in het zicht van de app: een volume uit de jobspec |
| `DECODE_URL` | | de stream van een `http://`-URL over `appnet` (wint van `DECODE_FILE`) |
| `DECODE_CODEC` | uit de extensie | `hevc`, `h264`, `av1`, `vp9`, `vp8`, `mpeg2`, `vc1`, `jpeg` |
| `DECODE_PIXEL` | `p010` | het uitvoerformaat (`nv12` voor 8 bit) |
| `DECODE_BUFS` | `12` | de beeldbuffers (minstens wat de firmware vraagt plus één, hooguit 32) |

## Markers

| Marker | Wanneer |
| --- | --- |
| `HOPOS_DECODE_OPEN` | de kern gaf een sessie |
| `HOPOS_DECODE fps=… MBps=…` | de meting, na Done en een lege rij |
| `HOPOS_DECODE_NOCODEC` | de open werd geweigerd (geen VPU, geen firmware voor deze codec, alle sessies bezet); de app blijft leven zonder iets te doen |
| `HOPOS_DECODE_FAIL` | de stream, de heap, een fout van de decoder of 20 s zonder event |

## Geheugen

De buffers komen uit de heap van de app, op hele pagina's: vier invoerbuffers
van 1 MB en `DECODE_BUFS` beeldbuffers van de maat die de firmware noemt. 4K
P010 is 24 MB per beeld, dus twaalf beelden is bijna 300 MB: `memory_limit`
512 MiB voor 4K, 128 MiB voor 1080p. Op QEMU (geen VPU) is 64 MiB genoeg.

## Bouwen

```sh
cargo build --release --target aarch64-unknown-none-softfloat -p decode
rust-objcopy --strip-debug target/aarch64-unknown-none-softfloat/release/decode decode.elf
```

## De jobspec

Naar de leader van Hop (poort 9080 op het adres van de node), met de stream
op het volume `/data`:

```sh
curl -X POST -H 'Content-Type: application/json' \
  -d '{"name":"decode","driver":"hop",
       "artifacts":[{"url":"http://LAPTOP:8000/decode.elf"}],
       "memory_limit":536870912,
       "env":{"DECODE_FILE":"/data/clip.hevc"},
       "volumes":{"/data":"/data"}}' \
  http://NODE:9080/v1/jobs
```

Of van een URL, zonder volume:

```sh
  -d '{"name":"decode","driver":"hop",
       "artifacts":[{"url":"http://LAPTOP:8000/decode.elf"}],
       "memory_limit":536870912,
       "env":{"DECODE_URL":"http://LAPTOP:8000/clip.hevc"}}'
```

De stream op het volume zetten kan met elke app die `/data` mount, of met
Lumen; de kern leest hem daar ook voor het meetinstrument.

De happen van 1 MB knippen op de laatste Annex-B-startcode, zodat elke
NAL-eenheid heel bij de decoder komt en de rest naar de volgende hap gaat.
GEMETEN 30-09 op de O6N: een NAL die over twee happen liep liet de decoder
faulten na 14 beelden; heel gevoerd deed dezelfde clip 85,7 fps.

## Op QEMU

Zonder VPU weigert de kern de open luid, en de app zegt dat en blijft staan
(gemeten 29-09 met een media-kern op QEMU virt en Hop in slot 1):

```
slot 2: decode: the node has no hevc decoder for this app: system call op 14: status 1: this node has no codec hardware; staying up without one HOPOS_DECODE_NOCODEC
```

## Vorm

- `src/main.rs`: de start, de meting (voeren, aanbieden, ophalen) en de
  regel. Eén taak: de codec-calls zijn niet idempotent en gaan over één
  system-client, na elkaar.
- `src/mem.rs`: buffers op hele pagina's in de eigen partitie (de grant).
- `src/source.rs`: de stream van een bestand (system-API) of een URL
  (leanhttp over `applib::tcp`), rechtstreeks in de invoerbuffer.

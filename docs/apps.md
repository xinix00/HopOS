# Apps schrijven voor HopOS

Een app is één ELF in één slot: een kooi met eigen partitie, een control-page
naar de kern, een netstack (Lean) op zijn eigen slot-IP, en een executor die
de taken van de app draait. `applib::main!` zet dat op; daarna is de app een
verzameling `async fn`-taken op die executor (het Rust-handboek, §2 en §4).
Dit document gaat over het ene ding dat een app goed moet doen om een goede
buur te zijn: **slapen op gebeurtenissen, niet op de klok.**

## Waarom dit het verschil maakt

Een slot krijgt zijn core van de kern. Idlet de executor (geen taak heeft een
gezet bit), dan geeft de slaper van applib de core terug met een wektijd: de
eerstvoorkomende echte timer. Op een eigen core is dat een `WFE`; op een
gedeelde core (`sharegroup` in de jobspec, of een board dat yield-idle
vraagt) is het een yield naar de kern, en de switcher geeft de core aan de
volgende bewoner. Bij de wektijd, of eerder bij de deurbel (een frame in de
RX-ring), komt de app terug.

De kern verdeelt de tijd dus eerlijk tussen bewoners die slapen. Wat hij niet
kan, is tijd afpakken van een bewoner die niet slaapt: er is geen preëmptie.
Een beurt duurt tot de executor idle is. Daar volgen twee regels uit:

1. Elk dutje (`after(5 ms)` om te kijken of er al iets is) is een wek van het
   slot, een volle ronde van alles wat klaarstaat, en pas daarna weer
   loslaten voor hooguit die 5 ms. Tien taken die zo dutten zijn tweeduizend
   wekken per seconde. Op een eigen core kost dat stroom; op een gedeelde
   core kost het de buren hun beurt.
2. Een lange synchrone berekening in één poll (crypto, een groot JSON-document
   serialiseren) houdt de core vast tot hij klaar is. De buren zien dan
   niets, ook niet hun hartslagen.

Hoe dat eruitziet als het misgaat, gemeten op de LicheeRV op 02-10-2026 met
drie Rust-slots in één sharegroup: een lege 404 van de controller duurde via
het LAN 0,2 tot 2 s en één keer meer dan 20 s; elke 8 tot 11 minuten verloren
tien plugins tegelijk hun hartslag (5 s) naar de controller. De Go-voorganger
op dezelfde node en dezelfde kern had daar nooit last van. Niet omdat Go
sneller is, maar omdat Go blokkeerde op het echte ding: een `Read`, een
`select` op channels, een timer voor de eerstvolgende deadline. De Rust-port
keek elke 1 tot 5 ms of er al iets lag.

## De regel

Een taak wacht op precies de dingen die hem werk kunnen geven, met één
`select`, en op niets anders:

```rust
loop {
    match select(self.stop.wait(), self.conn.readable()).await {
        Either::Left(()) => return,          // Drop ruimt op.
        Either::Right(Err(e)) => return self.fail(e),
        Either::Right(Ok(())) => {}
    }
    while let Some(frame) = self.read_frame()? {   // leest tot WouldBlock
        self.handle(frame).await?;
    }
}
```

Wat je daarvoor hebt, allemaal uit applib en de `sync`-crate:

| Wachten op | Bouwsteen |
| --- | --- |
| bytes op een TCP-verbinding, zonder ze al te lezen | `TcpStream::readable()` (de read zelf blokkeert ook netjes: `read()` wacht via de waker van Lean) |
| een datagram | `UdpSocket::readable()`, of `recv_from()` |
| een nieuwe verbinding | `TcpListener::accept()` |
| een bericht van een andere taak | `Channel<T, N>` en `Mailbox<T, N>` (`recv().await`) |
| een bel van een andere taak | `Signal::wait()` (level-triggered: tien bellen tegelijk zijn er één) |
| een echte deadline: hartslag, time-out, keepalive | `exec.after(d)` of `exec.until(at)` |
| een vangnet dat alleen telt zolang de core toch wakker is | `exec.after_deferrable(d)`: vuurt in de eerste ronde na zijn deadline, maar wekt een slapende core niet |
| twee of meer van het bovenstaande | `select(a, b)`, genest voor meer |

`after(d)` is dus een deadline, geen peiling. Als je `after(d)` in een lus
schrijft met een `d` kleiner dan de termijn die je werkelijk bewaakt, kijk je
rond: dan ontbreekt er een `readable()`, een `Signal` of een `Channel` in de
`select`.

### Lange berekeningen

Een handshake, een PBKDF2, een document van een megabyte: knip het op en zet
`yield_now().await` tussen de stukken. Een netwerkrondreis is al een
`.await` en dus al een knip; de rekenstap ertussen hoort kort te blijven. De
maat: één poll van één taak blijft onder een milliseconde op het traagste
board waar de app voor bedoeld is. Wat langer duurt, is werk voor een
`stacktask` (zie [stacktask.md](stacktask.md)) of voor meer `yield_now`.

### Pools, geen taak per verbinding

Het aantal taken staat vast bij de compiler (het handboek, §2). Een werker
wacht op zijn mailbox; een acceptor deelt uit. Een pool die vol is, laat de
verbinding wachten in de accept-wachtrij; hij gaat niet rondkijken of er al
een werker vrij is.

## De meetlat

Zonder cijfers is "de app slaapt" niet te onderscheiden van "de app dut".
Elke app publiceert twee tellers op zijn control-page, zonder er iets voor te
doen: `CTRL_IDLE` (de ticks van de architectuurteller die de slaper weg was,
alle cores van de app bij elkaar) en `CTRL_WAKES` (hoe vaak de slaper is
aangeroepen). De kern leest ze en zet voor elk levend slot om de dertig
seconden één regel op de console:

```
slot 3: idle=94% wakes=41/s cores=1 HOPOS_SLOT_LOAD
```

Lees hem zo:

- **idle** is de tijd die de app de core niet nodig had, over de laatste
  dertig seconden. Op ijzer zit een stille app boven de 95%; een app die
  werkt zakt, en dat hoort: de vraag is of hij weer stijgt als het werk
  klaar is. De norm geldt alleen op ijzer: op QEMU-TCG is `WFE` een no-op en
  meet een eigen core ~0%, dus een QEMU-run is op dit getal nooit rood.
- **wakes** is hoe vaak de app wakker werd. Een stille app met één hartslag
  van 50 ms (de `watch`-taak van applib) en een paar echte timers zit in de
  tientallen per seconde. Honderden per seconde zonder verkeer is dutten;
  duizenden is rondkijken.
- Op QEMU-TCG zegt daarom alleen `wakes` iets, behalve op een gedeelde
  core: daar telt de yield wél als idle, ook onder QEMU
  (`tools/qemu-test-share.sh`).
- Hop leest dezelfde twee tellers voor het cpu-percentage van een taak in
  zijn API, over zijn eigen poll-interval: dat getal is "100 min idle" over
  dát venster, de consoleregel is idle over dertig seconden. Ze komen uit
  één bron en lopen alleen in ritme uiteen; leg ze niet tegen elkaar op de
  seconde.

In de app zelf staat hetzelfde fijner: `EXEC.get().stats` (`rounds`, `polls`,
`sleeps`, `slept_ns`). Een app die een eigen diagnoseregel heeft, zet die
getallen erin.

## Het Go-equivalent, voor wie van daar komt

| Go op tamago | Rust op applib |
| --- | --- |
| `conn.Read(buf)` blokkeert de goroutine | `stream.read(&mut buf).await`, of eerst `stream.readable().await` |
| `select { case <-ch: case <-time.After(d): }` | `select(rx.recv(), exec.after(d))` |
| `time.Sleep(d)` | `exec.after(d).await`, en alleen als `d` een echte termijn is |
| een goroutine per verbinding | een werker uit een vaste pool |
| `runtime.Gosched()` | `yield_now().await` |
| de runtime parkeert als alles blokkeert | de executor idlet als geen taak een bit heeft; applib yieldt of `WFE`'t |

Go's runtime deed het loslaten voor je: zolang je blokkeerde, sliep de app.
Hier is het hetzelfde, met één verschil: een `after(1 ms)` is in Go ook
mogelijk, maar daar schrijft niemand hem omdat `Read` blokkeert. In Rust ligt
`poll::once(read)` plus `after(1 ms)` voor de hand als je een niet-blokkerende
lus vertaalt. Dat is de valkuil, en `readable()` is de uitweg.

## Checklist voor de review

- Geen `after(d)` in een lus met een `d` korter dan de termijn die de lus
  bewaakt. Elke `after` noemt in zijn commentaar welke termijn hij is.
- Elke lus die op I/O wacht, wacht via `readable()`, `accept()`, `recv()` of
  `Signal::wait()`, niet via een niet-blokkerende poging plus een dutje.
- Een synchrone stap die op het traagste board meer dan een milliseconde
  kost, heeft een `yield_now` of een `stacktask`.
- De `HOPOS_SLOT_LOAD`-regel van de app op een stille node: idle boven de
  95%, wakes in de tientallen. Zet die regel in het testlogboek van de app.
- Op een gedeelde core: de 404 van de buurman blijft onder de 50 ms terwijl
  jouw app werkt.

Zie ook: het Rust-handboek (`rustdoc/README.md`, §2 Taken en §4 De executor),
[stacktask.md](stacktask.md) voor synchrone code op een eigen stack,
[go-apps.md](go-apps.md) voor apps die in Go blijven, en
`tools/qemu-test-share.sh` voor de rotatie op een gedeelde core.

# De slot-lifecycle: wie garandeert wat, en waarom slots dun blijft

> **Waarom dit dossier bestaat:** in augustus 2026 groeide `kern/slots` in één
> ronde met ~600 regels aan lifecycle-machinerie — een generatie-lease per
> stream, stop-intenties, quarantaine-met-reconcile, een ABA-vrije
> cage-boekhouding. Alles correct opgeschreven, en het meeste bewees een
> eigenschap die de laag erboven al garandeert of die dit ontwerp bewust niet
> wil. Zulke code komt bij elke AI-revisie terug, want lokaal ziet elke race
> echt uit. Dit is de grens; de code is de waarheid. Bij tegenspraak wint de
> code en is dít bestand de bug.

## De drie toetsen

Elke wijziging in het start/stop-pad haalt deze drie, of hij gaat er niet in:

1. **Welke storing?** Niet "welke race is denkbaar" maar: wat ging er stuk, op
   welke datum, met welk symptoom. Een race die alleen in een diagram bestaat
   kost hier permanente complexiteit op een ABI-kritisch pad.
2. **Wordt hij al elders afgevangen?** Lees eerst de aanroeper. `slots` is de
   onderste laag van een keten waarin HOP's runner de opdrachten serialiseert;
   wat daar al gebeurt hoort hier niet nóg eens te gebeuren.
3. **Is dit het kleinste mechanisme?** Een vlag is kleiner dan een lease, een
   luide fout is kleiner dan een herstelprotocol, en niets is het kleinste.

## Wat de laag erboven al garandeert

`HopRunner` (hop, `internal/runner/hopos.go`) is de enige aanroeper van
`slots.Start*`/`Stop`. Wat hij doet vóórdat de node iets hoort:

| Runner | Wat het garandeert |
|---|---|
| `r.mu` + `r.inUse`/`r.slots` | één eigenaar per kooi; twee jobs kunnen niet om hetzelfde slot vechten |
| `r.stopping[task]` vóór `sm.Stop` | een start die nog moet beginnen stapt er graceful uit |
| `cancel()` + wachten op `dones[task]` | Stop wacht tot een lopende stream is afgewikkeld — de runner-code zegt het zelf: *"Zonder dit wachten zou sm.Stop hieronder een partitie kunnen vrijgeven waar de stream nog in schrijft."* |
| `r.armed[task]` | `sm.Stop` wordt overgeslagen als er nooit iets gearmd is |

Dat is vier van de vier eigenschappen waarvoor de stream-lease werd gebouwd:
stop-intentie, wachten-op-afwikkeling, en weten of er iets te stoppen valt.
Dus: **geen tweede exemplaar in `slots`.**

De rest-gat dat wél bestaat: de runner geeft na `2 × downloadStallTimeout`
(120s) op en stopt het slot alsnog, met een luide logregel. Wil je dát dichten,
dan hoort de fix in de runner (niet opgeven, of de node vragen) — niet in een
generatie-lease onderin. Documenteer dan hier waarom het alsnog moest.

## Het herstelmodel: luid falen, niet repareren

HopOS reconcilieert geen in-memory staat. Een node die zijn eigen toestand niet
meer kan bewijzen hoort te falen zodat de watchdog hem reset (`hopos.wd`,
[config](config.md)) — een verse boot is hier goedkoop en aantoonbaar schoon,
een half-herstelde toestand niet. Dat is dezelfde regel als "élke start is een
schone lei" en "niets is persistent".

Twee mechanismen bestaan daarom al, en die zijn genoeg:

- **Quarantaine bij een onbekende uitkomst.** Faalde het startschot zelf
  (`ErrDispatch`) of kon een intrekking niet bevestigd worden, dan gaat het RAM
  *niet* terug de pool in (`HOPOS_PART_QUARANTINE`). Fail-closed, geen protocol.
- **De lijk-opruiming in `claimSlot`.** Een "levende" ctx zonder eigenaar is een
  lijk (DRAM-residu overleeft een warme herstart; gemeten 02-08, toen een verse
  boot zijn állereerste plaatsing weigerde op het Saved-lijk van de vorige run).
  Wie plaatst heeft het slot vrij bevonden, dus die mag het lijk evicten.

> **Val, en dit is de belangrijkste regel in dit bestand:** een quarantaine-poort
> vóór `claimSlot` knijpt precies die lijk-opruiming af. Een slot dat vroeger bij
> de volgende Start zelf herstelde, heeft dan een geslaagde reconcile nodig om
> ooit nog bruikbaar te zijn — en lukt die niet, dan is het slot pas na een
> reboot terug. Dat is strikter dan het ontwerp wil en slechter dan wat er stond.

## Wat er van die ronde wél in hoort

Vier fixes, elk met een symptoom dat je op ijzer kon zien, en elk klein:

1. **`ErrDispatch` exporteren en de kooi vasthouden.** `slotmgr` gaf op élk
   `StartStream`-faalpad de kooi terug — óók op het pad waar de core misschien
   tóch aanging. Het RAM stond al in quarantaine, de *core* niet: die kon aan de
   volgende job worden uitgedeeld. Eén error-check, geen protocol.
2. **De DeviceGrant pas ná `claimSlot` muteren.** `prepStart` riep `grantEnv`
   vóór het lifecycle-venster, dus een dubbele Start pakte de fb-grant af van de
   *levende* app (HOP-console zwart) en de faal-defer gaf daarna de grant van die
   levende app vrij. Zichtbaar, echt, en met de verplaatsing weg.
3. **De grant van een dode eigenaar vrijgeven vóór een re-Start.** `partAlloc`
   staat een re-Start zonder voorafgaande Stop toe; zijn grant-token mocht niet
   naar de nieuwe task doorlekken.
4. **De SMP-plaatsingscheck ná de core-assignment.** De check stond in
   `prepStart` en las `coreOf(i)` vóórdat `hostCore[i]` gezet was — hij toetste
   dus de vorige plaatsing. Verplaatsen, niet uitbreiden.

## Wat er bewust NIET in hoort

Zodat de volgende ronde ziet dat dit weloverwogen ontbreekt en niet vergeten is:

- **Stream-lease met generaties, stop-intentie, `lockAfterStreams`.** Dekt wat de
  runner al doet (zie de tabel). De generaties bestaan alleen om de lease zelf
  veilig te maken: machinerie op machinerie.
- **`Reconcile` + quarantaine opheffen.** In-memory herstel van een onbewijsbare
  hardwaretoestand. Botst met het herstelmodel én met de lijk-opruiming, en maakt
  van een gewone Stop-timeout een toestand die alleen een reconcile nog opheft.
- **`ErrPlacementReleased` / `PlacementCleanupHandled` / de ABA-test.** Die
  ABA-race bestaat pas *doordat* de rollback complexer werd. Weg met de rollback,
  weg met de race.
- **Een `partition`-vlag in `slots`.** Dat is `r.armed` in de runner, nog eens.
- **Task-root opruimen bij Stop.** `armSlot` veegt de root bij élke start
  ("schone lei per start"), dus dit maakt geen enkel gedrag correcter — het
  verschuift alleen wanneer opslag vrijkomt, en kost een faalpad.
- **`hostCore` terugzetten op de vorige waarde.** `0` betekent "default:
  `coreOf(i) == i`" en is na een mislukte plaatsing eerlijker dan de mapping van
  een task die niet meer draait.

## Als het tóch moet

Prima — maar dan met een meting erbij (datum, symptoom, logregel), en met een
regel in dit bestand die uitlegt welke van de drie toetsen hem haalde. Een
nieuwe exportnaam in `slots` is een belofte aan HOP's runner: negen erbij in
één ronde is negen beloftes die niemand vroeg.

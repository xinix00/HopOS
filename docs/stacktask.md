# Synchrone C-code als parkeervrije executor-taak

`applib::stacktask::Task` geeft één synchrone functie een eigen, volledig vooraf
gereserveerde stack. Een callback gebruikt `Suspender::wait(future)`: bij
`Pending` keert de oorspronkelijke executor terug en blijven de C-frames en
hun geleende buffers bestaan. De executor blijft dus de netwerkpomp draaien
die de system-API-antwoorden voor die callback moet afleveren. Er is geen
geneste executor en er wordt geen extra core gereserveerd.

De eigenaar maakt, pollt en dropt de taak op dezelfde core. Het handvat is
`!Send`/`!Sync`; state en stack behouden hun heapadres als het handvat verhuist.
Alleen de kleine module met contextswitch en stackallocatie bevat unsafe.
De Rust/C-functies hebben tijdens uitvoering gewone ABI-conforme stacks.
ARM64 bewaart de callee-saved registers en, buiten de softfloat-ABI, FP-state;
RISC-V bewaart ook de D-registers voor rv64gc/lp64d. De x86_64 SysV-variant
bestaat, maar is hier nog niet op een x86_64-host gedraaid.

`Task::new` is expliciet unsafe: de aanroeper moet aantonen dat het werk in de
gekozen 64 KiB..16 MiB stack past. De slot-heap heeft geen guard pages. Een
callback moet na `Cancelled` terugkeren; geen longjmp buiten de taak en geen
bewaarde verwijzingen naar de private stack. Panics over de extern-C-ingang
aborteren. Dit is geen vrijbrief voor onbeperkte recursie in externe C-code.

Drop geeft annulering door en hervat de taak tot de functie terugkeert. Zo
worden zowel lokale Rust-waarden als C-handvatten op hun eigen stack opgeruimd
vóór vrijgave. Een future waarvan de I/O-bevestiging onbekend is, vereist
daarnaast een vergiftigde I/O-eigenaar: alleen de C-stack opruimen maakt een
onbekende write-uitkomst niet weer betrouwbaar.

De hosttests toetsen diepe lokale frames, herhaald parkeren, integer/FP-state,
opruimen van geannuleerde futures en nooit gestarte taken. Replica's
`tools/qemu-persist-candidate.py` draait de echte SQLite-C-engine via deze brug
op ARM64: commit, rollback, SIGKILL, koude herstart, integriteitscontrole,
annuleren van een lange CPU-query via SQLite's progresscallback en heropenen
met dezelfde SQLite-heap. De complete RISC-V-app bouwt; deze proef claimt geen
RISC-V-runtimebewijs. Bronpins blijven op een gepubliceerde SDK-tag: uitsluitend
de tijdelijke kandidaatkopie van Replica patcht naar deze werkboom.

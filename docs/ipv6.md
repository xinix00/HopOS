# IPv6 voor slot-apps

De opt-in Lean-baan voor Matter/Thread is naar Rust geport: UDP6, ICMPv6/NDP,
RS/RA, één SLAAC-adres en begrensde PIO/RIO-routes. `appnet::Udp6Socket` heeft
async send/recv, deadlines en Drop; `Net::join_group6` joint ff02-multicast.
`Net::ipv6_addresses` geeft de actuele link-local/SLAAC-identiteit.
`Net::resolve6` en `resolve6_via` vragen AAAA via de bestaande DNS-server.

Binden is wildcard-only; `local()` geeft `[::]:port`. De bron wordt per
bestemming gekozen. IPv4 en IPv6 hebben aparte poorttabellen. Scopekeuze hoort
bij de app-adapter: elk slot heeft precies één interface. De IPv6-pakketgrens
is 1280 bytes, dus maximaal 1232 bytes UDP-payload. Zonder socket of groepsjoin
is de IPv6-baan uit en heeft zij geen dynamische tabellen of timers.

IPv6 heeft één bron: `leannet` in Lean, sinds tag v3.1.4. HopOS bevat geen
kopie van de netstack en geen pad buiten deze repo (PORT.md §6.6).

Gevalideerd: Lean-hosttests, Go-draadfixtures, SDK-UDP/AAAA-tests met de echte
async pompen, HopOS-switchtests en de Stulp-QEMU-proef met twee slots. De
bestaande switch draagt IPv6 al native op laag 2; er is geen IPv6-NAT toegevoegd.

De hardwarepoort uit Lean/KAM.md blijft gelden: één echte border-routerketen
(RA → SLAAC → RIO → UDP 5540 terug) en expliciet ff02::fb-verkeer per gedragen
NIC. Een QEMU-resultaat vervangt die proef niet. TCPv6, MLD, DHCPv6,
fragmentatie en een volledige NUD-machine blijven buiten het Go-profiel.

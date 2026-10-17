# IPv6 for slot apps

The opt-in lean path for Matter/Thread has been ported to Rust: UDP6,
ICMPv6/NDP, RS/RA, one SLAAC address and bounded PIO/RIO routes.
`appnet::Udp6Socket` has async send/recv, deadlines and Drop;
`Net::join_group6` joins ff02 multicast. `Net::ipv6_addresses` gives the
current link-local/SLAAC identity. `Net::resolve6` and `resolve6_via` ask for
AAAA via the existing DNS server.

Binding is wildcard-only; `local()` gives `[::]:port`. The source is chosen
per destination. IPv4 and IPv6 have separate port tables. Scope choice
belongs to the app adapter: every slot has exactly one interface. The IPv6
packet limit is 1280 bytes, so at most 1232 bytes of UDP payload. Without a
socket or group join the IPv6 path is off and has no dynamic tables or
timers.

IPv6 has one source: `leannet` in lean, since tag v3.1.4 (now pinned at
v3.1.12), behind its feature `ipv6`. applib turns that on
(`applib/Cargo.toml`); the kernel builds without it and carries no IPv6
code. HopOS contains no copy of the netstack and no path outside this repo
(PORT.md §6.6).

Validated: lean host tests, Go wire fixtures, SDK UDP/AAAA tests with the
real async pumps, HopOS switch tests and the Stulp QEMU test with two slots.
The existing switch already carries IPv6 natively at layer 2; no IPv6 NAT
was added.

The hardware gate from lean/KAM.md still applies: one real border router
chain (RA → SLAAC → RIO → UDP 5540 back) and explicit ff02::fb traffic per
supported NIC. A QEMU result does not replace that test. TCPv6, MLD, DHCPv6,
fragmentation and a full NUD machine stay outside the Go profile.

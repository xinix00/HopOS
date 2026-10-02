# Go-apps

Een app waarvan de bron Go is blijft Go: hij bouwt met tamago tegen de Go-SDK
(`github.com/xinix00/HopOS/metal/v2`, v2.2.8) en draait in een slot zoals een
Rust-app. Hoe de kern dat toelaat en hoe je bouwt: [docs/go-apps.md](../docs/go-apps.md).

| Wat | Waarom |
| --- | --- |
| [cloudflared/](cloudflared/) | cloudflared's eigen `tunnel run` als slot-app |
| [tamago-go/](tamago-go/) | de drie runtime-patches voor de tamago-toolchain, met `apply.sh` |
| [apps-release.sh](apps-release.sh) | bouwt de Go-apps (en publiceert ze naar de release `apps`); `tools/release.sh` roept het aan met `PUBLISH=0` |

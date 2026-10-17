# Go apps

An app whose source is Go stays Go: it builds with tamago against the Go SDK
(`github.com/xinix00/HopOS/metal/v2`, v2.2.8) and runs in a slot like a
Rust app. How the kernel allows that and how to build: [docs/go-apps.md](../docs/go-apps.md).

| What | Why |
| --- | --- |
| [cloudflared/](cloudflared/) | cloudflared's own `tunnel run` as a slot app |
| [tamago-go/](tamago-go/) | the three patches for the tamago toolchain (runtime and net), with `apply.sh` |
| [apps-release.sh](apps-release.sh) | builds the Go apps (and publishes them to the `apps` release); `tools/release.sh` calls it with `PUBLISH=0` |

#!/bin/sh
# apps-release.sh — bouwt en publiceert ÉLKE HopOS-app-image: alles wat
# metal/app/applib linkt, ongeacht in welke repo de bron staat.
#
#   tools/apps-release.sh                  # tegen de metal in DEZE boom
#   METAL=v1.12.0 tools/apps-release.sh    # tegen een gepubliceerde metal-tag
#   PUBLISH=0 tools/apps-release.sh        # alleen bouwen (gate), niets uploaden
#   TAG=apps-beta COMPAT=0 ...             # een beta-app-release (chain-beta.sh)
#
# WAAROM DIT HIER STAAT EN NIET IN HOP. Een app-image is een metal-artefact: het
# linkt applib, appnet en de slot-ABI, en zijn linkadres komt uit de partitie-
# indeling van deze boom. hop zelf hangt NIET aan metal — zijn go.mod noemt het
# niet en geen pakket buiten apps/ importeert het — dus hop hoort niet opnieuw
# uitgebracht te worden omdat metal beweegt. Die twee cadansen zaten aan elkaar
# vast doordat hop's release.sh de app-elfs meebouwde.
#
# WAT DAT KOSTTE, gemeten 11-08: de app-modules pinden élk hun eigen metal en
# alle zeven liepen achter — hopdns/hoplb/hopprom en welcome/cloudflared op
# v1.8.3 (nog het gVisor-tijdperk), vitals op v1.11.1, surf op v1.9.2. De
# keten-beta's patchten alleen hop/apps/*, dus de drie satellieten zijn in élke
# beta tegen v1.8.3 gebouwd. Hier is dat structureel weg: dit script bouwt
# iedereen tegen dezelfde metal, en de lijst hieronder is de enige plek waar
# staat wie dat zijn.
set -e

DIR="$(cd "$(dirname "$0")/.." && pwd)"
EASY="${EASY_DIR:-$HOME/Git/easy}"
HOP_DIR="${HOP_DIR:-$EASY/hop}"
TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
PUBLISH="${PUBLISH:-1}"
STAMP="${STAMP:-$(date -u +%Y.%m.%d-%H%M)}"
# TAG is de rollende app-release in de HopOS-repo waar de bootconfigs naar
# wijzen. COMPAT voedt daarnaast hop's oude rolling-release, waar de nodes die
# nu in het veld staan hun apps halen. Een BETA moet die laatste met rust laten
# (TAG=apps-beta COMPAT=0), anders krijgt een stabiele node beta-apps.
TAG="${TAG:-apps}"
COMPAT="${COMPAT:-1}"

# De app-lijst staat in tools/hopos-apps.list — één plek, ook voor
# chain-release.sh (die bumpt eruit de metal-pins). Twee lijsten die uit elkaar
# lopen is precies hoe de satellieten vier metal-versies achterop raakten.
LIST="${LIST:-$DIR/tools/hopos-apps.list}"
[ -f "$LIST" ] || { echo "FOUT: $LIST ontbreekt" >&2; exit 1; }
APPS="$(sed 's/#.*//' "$LIST" | grep . | sed -e "s|~|$HOME|g" -e "s|\./|$DIR/|g")"

# cloudflared bouwt tegen een gepatchte kopie van zijn eigen module (twee
# platform-fallbacks die upstream mist) en zijn go.mod verwijst daarnaar met een
# replace — zonder die map faalt élk go-commando in die module. Idempotent.
PREPARE="$DIR/apps/cloudflared/tools/prepare-cloudflared.sh"

OUT="$DIR/metal/out/apps"
mkdir -p "$OUT"
[ -x "$TAMAGO" ] || { echo "FOUT: tamago ontbreekt op $TAMAGO (zet TAMAGO)" >&2; exit 1; }
command -v gh >/dev/null || [ "$PUBLISH" != "1" ] || { echo "FOUT: gh ontbreekt" >&2; exit 1; }
# MAG DIT TOKEN HIER SCHRIJVEN. Niet "hoe heet het account" maar de vraag die
# telt, en die is per repo te stellen. Eerder las dit `gh api user`: precies de
# endpoint die tijdens de GitHub-storing van 17-08 als enige 503 bleef geven
# terwijl repos en releases allang antwoordden — dan valt een publicatie om met
# een account-melding terwijl het account goed staat. Met het verkeerde account
# is push hier false (gemeten 11-08), dus de dekking blijft.
if [ "$PUBLISH" = "1" ]; then
	for r in xinix00/HopOS xinix00/hop; do
		perm="$(gh api "repos/$r" --jq .permissions.push 2>/dev/null || true)"
		[ "$perm" = "true" ] || {
			echo "FOUT: WRONG USER — dit gh-token mag niet schrijven in $r (push=${perm:-onbekend})." >&2
			echo "      herstel: gh auth switch --user xinix00" >&2; exit 1; }
	done
fi


# 1. Alle app-modules op DEZELFDE metal. Zonder METAL is dat de boom waar dit
#    script in staat (pad-replace), met METAL een gepubliceerde tag.
#
#    Een pad-replace mag hier, en dat is geen slordigheid: een app-module is een
#    EINDPUNT. Niemand importeert hem, er komt alleen een ELF uit. Waarom een
#    replace elders gevaarlijk is — hij geldt alleen in de main module, dus
#    consumers zien hem niet en bouwen stil iets anders — geldt hier dus niet.
#    De trap draait alles terug, ook bij een gefaalde build of Ctrl-C.
MODS=""
restore() {
	for m in $MODS; do
		mv -f "$m/go.mod.appsbak" "$m/go.mod" 2>/dev/null || true
		mv -f "$m/go.sum.appsbak" "$m/go.sum" 2>/dev/null || true
	done
}
trap restore EXIT INT TERM

echo "== HopOS app-images ==" >&2
echo "   metal: ${METAL:-$DIR/metal (werkboom)}" >&2

# Eerst kijken, dan pas aanraken: zodra we één go.mod patchen is de boom vuil en
# zegt de controle hieronder niets meer (alle drie de app-modules van HopOS zitten
# in dezelfde repo).
for entry in $APPS; do
	name="${entry%%:*}"; mod="$(echo "$entry" | cut -d: -f2)"
	[ -f "$mod/go.mod" ] || { echo "FOUT: $name — geen go.mod op $mod" >&2; exit 1; }
done

# cloudflared klaarzetten vóór de tidy-ronde, niet erna: zonder
# build/cloudflared-patched faalt élk go-commando in die module — inclusief tidy,
# en dan mist go.sum de netstack-regels en valt de LINK om (gemeten 11-08, en het
# zat verstopt achter een `|| true` op tidy).
[ -x "$PREPARE" ] && { echo ">> cloudflared klaarzetten" >&2; sh "$PREPARE" >/dev/null; }

for entry in $APPS; do
	name="${entry%%:*}"; rest="${entry#*:}"
	mod="${rest%%:*}"
	cp "$mod/go.mod" "$mod/go.mod.appsbak"
	[ -f "$mod/go.sum" ] && cp "$mod/go.sum" "$mod/go.sum.appsbak"
	MODS="$MODS $mod"
	if [ -n "${METAL:-}" ]; then
		( cd "$mod" && GOWORK=off go mod edit -require "github.com/xinix00/HopOS/metal@$METAL" )
	else
		( cd "$mod" && GOWORK=off go mod edit -replace "github.com/xinix00/HopOS/metal=$DIR/metal" )
	fi
	# NIET stilzwijgend: een gefaalde tidy laat go.sum incompleet achter en dat
	# komt pas als een linkfout naar boven, drie apps later.
	( cd "$mod" && GOWORK=off GOFLAGS=-mod=mod GOTOOLCHAIN=local go mod tidy 2>&1 ) ||
		{ echo "FOUT: go mod tidy faalt in $mod" >&2; exit 1; }
done

# 2. Bouwen. Canonieke app-link (docs/app.md): één artifact draait in élk slot,
#    dus arm64 linkt op SlotBase(1)+0x10000 en riscv64 op de fysieke partitie
#    (daar is geen tweede translatiefase). houdt gVisor uit het
#    image — op de nieuwe netstack 0 gvisor-symbolen i.p.v. 3649.
ELFS=""
for entry in $APPS; do
	name="${entry%%:*}"; rest="${entry#*:}"
	mod="${rest%%:*}"; rest="${rest#*:}"
	cmd="${rest%%:*}"; arches="$(echo "${rest#*:}" | tr ',' ' ')"
	for arch in $arches; do
		case "$arch" in
		arm64)   tags="linkcpuinit";             ld="-w -T 0x50010000 -R 0x1000" ;;
		riscv64) tags="linkramsize linkcpuinit"; ld="-w -T 0x88010000 -R 0x1000" ;;
		*) echo "FOUT: onbekende arch $arch bij $name" >&2; exit 1 ;;
		esac
		elf="$OUT/$name-$arch-tamago.elf"
		printf '   %-30s' "$(basename "$elf")" >&2
		( cd "$mod" && GOWORK=off GOTOOLCHAIN=local \
			GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH="$arch" \
			"$TAMAGO" build -tags "$tags" -trimpath \
			-ldflags "$ld -X main.version=$STAMP" -o "$elf" "./cmd/$cmd" )
		echo "$(( $(wc -c < "$elf") / 1024 )) kB" >&2
		ELFS="$ELFS $elf"
	done
done

# 3. Bewijs dat de netstack niet stil gesplitst is: geen enkel gvisor-symbool in
#    welk image dan ook. Dit is de check die de v1.8.3-pins had moeten
#    tegenhouden — een oude metal levert een wérkende app op, alleen met de
#    verkeerde stack erin, en dat ziet niemand aan de buitenkant.
echo ">> netstack-controle (0 gvisor-symbolen verwacht)" >&2
for elf in $ELFS; do
	n="$("$TAMAGO" tool nm "$elf" 2>/dev/null | grep -c gvisor || true)"
	[ "$n" -eq 0 ] || { echo "FOUT: $(basename "$elf") linkt $n gvisor-symbolen" >&2; exit 1; }
done
echo "   alle $(echo $ELFS | wc -w | tr -d ' ') images schoon" >&2

[ "$PUBLISH" = "1" ] || { echo "KLAAR (PUBLISH=0, niets geüpload) — $OUT" >&2; exit 0; }

# 4. Publiceren. HopOS/$TAG is de thuisbasis: de bootconfigs wijzen daarheen en
#    de release hangt niet aan hop's versie. hop/rolling-release wordt daarnaast
#    gevoed zolang COMPAT=1, want de nodes die vandaag in het veld staan hebben
#    die URL in hun config staan — die mag niet doodvallen omdat wij verhuizen.
PRE=""
case "$TAG" in *beta*) PRE="--prerelease" ;; esac
U="https://github.com/xinix00/HopOS/releases/download/$TAG"
NOTES="Ready-to-run HopOS app images — canonically linked, so one artifact runs in any slot. Drop a URL in a jobspec and the node streams it straight onto a partition.

Built $STAMP against metal ${METAL:-(working tree)} — all of them against the same one, verified to link zero gVisor symbols. Rolling: every publish replaces the assets, the URLs stay put.

**welcome** gives a fresh node a face: one self-contained HTML page on the published port, no env, no cluster key. This is the default \`hopos.init[]\` of the headless images (arm64 and riscv64).

\`\`\`
hopos.init[]={\"name\":\"welcome\",\"driver\":\"hop\",\"artifacts\":[{\"url\":\"$U/welcome-arm64-tamago.elf\",\"match\":{\"node.arch\":\"arm64\"}},{\"url\":\"$U/welcome-riscv64-tamago.elf\",\"match\":{\"node.arch\":\"riscv64\"}}],\"memory_limit\":67108864,\"ports\":{\"http\":80}}
\`\`\`

**vitals** is the board doctor: idle, CPU (including thermals), memory and network of the node it runs on.

**hopdns / hoplb / hopprom** are the cluster satellites — DNS, load balancer, metrics. Configured through jobspec env (HOP_ADDR defaults to 10.100.0.1:8080, the node itself): hopdns takes ER_PORT_DNS/HOPDNS_PEERS/HOPDNS_DOMAIN, hoplb takes ER_PORT_HTTP/ER_PORT_ADMIN/HOPLB_TAG, hopprom takes ER_PORT_METRICS/HOPPROM_INTERVAL. See the \`cmd/<app>-hopos\` mains.

\`\`\`json
{\"name\":\"hopdns\",\"driver\":\"hop\",\"count\":-1,
 \"artifacts\":[{\"url\":\"$U/hopdns-arm64-tamago.elf\"}],
 \"memory_limit\":134217728,
 \"ports\":{\"dns\":5353},
 \"env\":{\"HOP_API_KEY\":\"...\",\"HOPDNS_DOMAIN\":\"hop.local\"}}
\`\`\`

**cloudflared** runs cloudflared's own \`tunnel run\` as a slot app: a node behind NAT, with no inbound port, reachable over Cloudflare Tunnel. It publishes no port (it dials out) and wants a ~256 MB partition. Without TUNNEL_TOKEN you get a quick tunnel whose trycloudflare URL shows up in \`hop logs cloudflared\`; without TUNNEL_URL it points at \`http://\$HOPOS_HOST\` — port 80 of the node, so the welcome page by default. Config uses cloudflared's own env names (TUNNEL_TOKEN/TUNNEL_URL/TUNNEL_TRANSPORT_PROTOCOL/TUNNEL_LOGLEVEL, plus CFD_EXTRA_ARGS); the default protocol is http2 rather than quic, because QUIC through the per-slot netstack has not been measured.

\`\`\`json
{\"name\":\"cloudflared\",\"driver\":\"hop\",\"artifacts\":[
  {\"url\":\"$U/cloudflared-arm64-tamago.elf\",\"match\":{\"node.arch\":\"arm64\"}},
  {\"url\":\"$U/cloudflared-riscv64-tamago.elf\",\"match\":{\"node.arch\":\"riscv64\"}}],
 \"memory_limit\":268435456,
 \"env\":{\"TUNNEL_TOKEN\":\"...\"}}
\`\`\`

The GUI apps (display, launcher, taskman, browser, …) live in [hop-os-surf](https://github.com/xinix00/hop-os-surf/releases/tag/rolling-release)."

echo ">> uploaden naar HopOS/$TAG" >&2
# shellcheck disable=SC2086 — woordsplitsing over de elf-lijst is de bedoeling
if gh release view "$TAG" --repo xinix00/HopOS >/dev/null 2>&1; then
	gh release upload "$TAG" --repo xinix00/HopOS --clobber $ELFS >/dev/null
	gh release edit "$TAG" --repo xinix00/HopOS --notes "$NOTES" >/dev/null
else
	gh release create "$TAG" --repo xinix00/HopOS --latest=false $PRE \
		--title "HopOS app images${PRE:+ (beta)}" --notes "$NOTES" $ELFS >/dev/null
fi

if [ "$COMPAT" = "1" ]; then
	echo ">> uploaden naar hop/rolling-release (de nodes die er al staan)" >&2
	# shellcheck disable=SC2086
	gh release upload rolling-release --repo xinix00/hop --clobber $ELFS >/dev/null
fi

echo "KLAAR" >&2
echo "  apps:  https://github.com/xinix00/HopOS/releases/download/$TAG/<app>-arm64-tamago.elf" >&2
[ "$COMPAT" = "1" ] && echo "  oud:   hop/rolling-release bijgewerkt (compat)" >&2

#!/bin/sh
# chain-beta.sh — bouwt en publiceert de HELE keten als één beta:
# hop (+welcome) → hop-os → hop-os-surf, alles uit de WERKBOMEN die je nu hebt.
#
#   tools/chain-beta.sh          # beta.1
#   tools/chain-beta.sh 2        # beta.2
#
# WAAROM DIT BESTAAT. Een gewone release bouwt hop-os tegen gepubliceerde tags:
# wat hier bouwt, bouwt overal (v1.8.4). Een keten-beta wil het omgekeerde —
# alles uit de bomen zoals ze nu zijn, ook wat nog nergens getagd staat. Vandaag
# is dat geen luxe maar noodzaak: de lneto-fixes (window scaling, ARP, ports,
# deadlines) staan in ~/Git/lneto en moeten nog upstream. Zonder replaces zou de
# NODE die fixes hebben en de APPS niet — go-mod-replaces gelden namelijk alleen
# in de hoofdmodule, dus hop-os-surf lost soypat/lneto op naar upstream. Dat is
# geen compilefout maar iets ergers: twee netstacks in één vloot, stil.
#
# Daarom zet dit script de replaces TIJDELIJK in de go.mod's, bouwt de hele
# keten daarmee, en draait ze daarna terug (ook bij een fout of Ctrl-C — zie de
# trap). Main blijft dus replace-vrij; alleen deze build gebruikt ze.
#
# GEVOLG, en dat hoort in de release-notes: deze artefacten zijn NIET uit
# module-versies te reproduceren. Ze zijn geïdentificeerd door drie commit-SHA's,
# en die zet het script er zelf bij.
#
# Alles wordt als PRERELEASE gepubliceerd, zodat `latest` in beide repo's op de
# stabiele versie blijft staan — de README en de docs linken daarheen.
set -e

N="${1:-1}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
HOP_DIR="${HOP_DIR:-$HOME/Git/easy/hop}"
SURF_DIR="${SURF_DIR:-$HOME/Git/hop-os-surf}"
LNETO_DIR="${LNETO_DIR:-$HOME/Git/lneto}"

HOPOS_VER="${HOPOS_VER:-v1.12.0-beta.$N}"
SURF_TAG="${SURF_TAG:-beta}"        # rollend, net als surf's rolling-release
SURF_PIN="${SURF_PIN:-beta.$N}"     # vastgepind op deze iteratie

# 0. Preflight. Liever nu luid stoppen dan halverwege een keten publiceren.
for d in "$DIR" "$HOP_DIR" "$SURF_DIR" "$LNETO_DIR"; do
	[ -d "$d/.git" ] || { echo "FOUT: $d is geen git-repo (zet HOP_DIR/SURF_DIR/LNETO_DIR)" >&2; exit 1; }
done
# hop-os en surf moeten schoon zijn: het script zet zelf replaces in hun go.mod
# en draait die terug — met andere wijzigingen erbij weet niemand meer wat er
# gepubliceerd is. hop en lneto worden alleen GELEZEN, dus die mogen vuil zijn
# (maar hun SHA staat dan wel in de notes met een -dirty erachter).
for d in "$DIR" "$SURF_DIR"; do
	[ -z "$(git -C "$d" status --porcelain)" ] || {
		echo "FOUT: $d niet schoon — eerst committen (dit script patcht go.mod)" >&2; exit 1; }
done
command -v gh >/dev/null || { echo "FOUT: gh ontbreekt" >&2; exit 1; }

sha() { git -C "$1" describe --tags --always --dirty 2>/dev/null || git -C "$1" rev-parse --short HEAD; }
HOP_SHA="$(sha "$HOP_DIR")"; HOPOS_SHA="$(sha "$DIR")"
SURF_SHA="$(sha "$SURF_DIR")"; LNETO_SHA="$(sha "$LNETO_DIR")"

echo "== keten-beta $HOPOS_VER ==" >&2
echo "   hop    $HOP_SHA" >&2
echo "   hop-os $HOPOS_SHA" >&2
echo "   surf   $SURF_SHA" >&2
echo "   lneto  $LNETO_SHA" >&2

# 1. De replaces erin, met een trap die ze er ALTIJD weer uit haalt — ook bij
#    een gefaalde gate of Ctrl-C. Een achtergebleven pad-replace in main is
#    precies de bus-factor die v1.8.4 wegwerkte.
cp "$DIR/metal/go.mod" "$DIR/metal/go.mod.chainbak"
cp "$SURF_DIR/go.mod" "$SURF_DIR/go.mod.chainbak"
restore() {
	mv -f "$DIR/metal/go.mod.chainbak" "$DIR/metal/go.mod" 2>/dev/null || true
	mv -f "$SURF_DIR/go.mod.chainbak" "$SURF_DIR/go.mod" 2>/dev/null || true
	git -C "$DIR" checkout -q -- metal/go.sum 2>/dev/null || true
	git -C "$SURF_DIR" checkout -q -- go.sum 2>/dev/null || true
}
trap restore EXIT INT TERM

# hop-os: hop uit de werkboom (die loopt vóór op zijn laatste tag). lneto stond
# er al in; die laten we staan zoals hij is.
( cd "$DIR/metal" && go mod edit -replace "github.com/xinix00/hop=$HOP_DIR" )
# surf: metal én lneto uit de werkbomen. Zonder de lneto-replace zou surf
# upstream pakken — precies de stille splitsing uit de kop.
( cd "$SURF_DIR" && go mod edit \
	-replace "github.com/xinix00/HopOS/metal=$DIR/metal" \
	-replace "github.com/soypat/lneto=$LNETO_DIR" )
( cd "$DIR/metal" && GOWORK=off go mod tidy >/dev/null 2>&1 || true )
( cd "$SURF_DIR" && GOWORK=off go mod tidy >/dev/null 2>&1 || true )

# 2. Gates. Niets wordt gepubliceerd voordat beide bomen groen zijn — dat is het
#    enige verschil tussen een beta en een willekeurige build.
echo ">> gate hop-os" >&2
( cd "$DIR" && sh tools/test.sh >/dev/null )
echo ">> gate surf (bouwt meteen out/*.elf)" >&2
( cd "$SURF_DIR" && sh tools/test.sh >/dev/null )

# 3. hop-os: eerst de prerelease AANMAKEN (dan wordt hij nooit `latest`), daarna
#    er de getekende images op. RELEASE_ALLOW_DIRTY: de dirt is de replace die
#    dit script er zelf in zette, en die is de bedoeling.
NOTES="**Chain beta $N — every part built from its working tree, not from published tags.**

This is the first beta on the new network stack (lneto instead of gVisor for apps). Expect a considerably smaller footprint; the sizes are listed below.

Built from:

| part | commit |
|---|---|
| hop | \`$HOP_SHA\` |
| hop-os | \`$HOPOS_SHA\` |
| hop-os-surf | \`$SURF_SHA\` |
| lneto (patched) | \`$LNETO_SHA\` |

The netstack fixes (TCP window scaling, ARP resolving all pending entries, sequential ephemeral ports, deadline-driven waits) are not upstream yet, so this build uses a patched lneto for **both** the node and the apps. That is why it is built with path replacements and cannot be reproduced from module versions alone — pin the commits above instead. A stable release will require published versions again.

Apps for this chain: https://github.com/xinix00/hop-os-surf/releases/tag/$SURF_TAG"

if ! gh release view "$HOPOS_VER" --repo xinix00/HopOS >/dev/null 2>&1; then
	git -C "$DIR" tag -a "$HOPOS_VER" -m "HopOS $HOPOS_VER (chain beta $N)" 2>/dev/null || true
	git -C "$DIR" tag "metal/$HOPOS_VER" 2>/dev/null || true
	git -C "$DIR" push -q origin "$HOPOS_VER" "metal/$HOPOS_VER"
	gh release create "$HOPOS_VER" --repo xinix00/HopOS --prerelease \
		--title "HopOS $HOPOS_VER — chain beta $N" --notes "$NOTES" >/dev/null
fi
echo ">> hop-os images bouwen + tekenen" >&2
( cd "$DIR" && RELEASE_ALLOW_DIRTY=1 sh tools/release.sh "$HOPOS_VER" >/dev/null )

# 4. surf: dezelfde elfs naar een rollende `beta` en een vastgepinde `beta.N`.
#    De boot-configs van deze hop-os-beta wijzen naar de ROLLENDE tag, zodat de
#    URL vast ligt terwijl de inhoud met de keten meebeweegt.
SURF_NOTES="SURF apps for HopOS chain beta $N — built against hop-os \`$HOPOS_SHA\` with the patched lneto (\`$LNETO_SHA\`).

\`\`\`json
{\"name\":\"taskman\",\"driver\":\"hop\",
 \"artifacts\":[{\"url\":\"https://github.com/xinix00/hop-os-surf/releases/download/$SURF_TAG/taskman.elf\"}],
 \"memory_limit\":67108864,
 \"env\":{\"SURF_ADDR\":\"{{host}}:7878\",\"HOP_ADDR\":\"10.100.0.1:8080\"}}
\`\`\`

Beta: built from working trees with path replacements, matching the HopOS beta of the same chain. For stable apps use \`rolling-release\`."
SURF_FILES=""
for app in display clock calc browser dash taskman launcher; do
	SURF_FILES="$SURF_FILES $SURF_DIR/out/$app.elf"
done
echo ">> surf-apps publiceren ($SURF_TAG + $SURF_PIN)" >&2
# shellcheck disable=SC2086 — woordsplitsing is hier de bedoeling
gh release create "$SURF_PIN" --repo xinix00/hop-os-surf --prerelease \
	--title "SURF apps $SURF_PIN" --notes "$SURF_NOTES" $SURF_FILES >/dev/null 2>&1 ||
	gh release upload "$SURF_PIN" --repo xinix00/hop-os-surf --clobber $SURF_FILES >/dev/null
if gh release view "$SURF_TAG" --repo xinix00/hop-os-surf >/dev/null 2>&1; then
	# shellcheck disable=SC2086
	gh release upload "$SURF_TAG" --repo xinix00/hop-os-surf --clobber $SURF_FILES >/dev/null
	gh release edit "$SURF_TAG" --repo xinix00/hop-os-surf --prerelease --notes "$SURF_NOTES" >/dev/null
else
	# shellcheck disable=SC2086
	gh release create "$SURF_TAG" --repo xinix00/hop-os-surf --prerelease \
		--title "SURF apps (beta)" --notes "$SURF_NOTES" $SURF_FILES >/dev/null
fi

# 5. De footprint-meting. De claim van deze beta is "veel kleiner", dus die
#    hoort met een getal in de notes en niet in een gevoel (huisregel).
echo ">> footprint meten" >&2
sizes=""
for app in display launcher taskman browser; do
	f="$SURF_DIR/out/$app.elf"
	[ -f "$f" ] && sizes="$sizes| $app | $(( $(wc -c < "$f") / 1024 )) kB |
"
done
gh release edit "$HOPOS_VER" --repo xinix00/HopOS --notes "$NOTES

**App image sizes in this chain** (uncompressed ELF, canonically linked):

| app | size |
|---|---|
$sizes" >/dev/null

restore
trap - EXIT INT TERM
echo "" >&2
echo "KLAAR" >&2
echo "  hop-os  https://github.com/xinix00/HopOS/releases/tag/$HOPOS_VER" >&2
echo "  apps    https://github.com/xinix00/hop-os-surf/releases/tag/$SURF_TAG" >&2
echo "  go.mod's teruggedraaid — main blijft replace-vrij" >&2

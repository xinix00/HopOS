#!/bin/sh
# chain-beta.sh — bouwt en publiceert de HELE keten als één beta:
# hop (+welcome) → hop-os → hop-os-surf, alles uit de WERKBOMEN die je nu hebt.
#
#   tools/chain-beta.sh          # beta.1
#   tools/chain-beta.sh 2        # beta.2
#
# WAAROM DIT BESTAAT. Een gewone release bouwt hop-os tegen gepubliceerde tags:
# wat hier bouwt, bouwt overal (v1.8.4). Een keten-beta wil het omgekeerde —
# alles uit de bomen zoals ze nu zijn, ook wat nog nergens getagd staat: hop en
# hop-os-surf leunen op elkaar en op metal, en die drie lopen tijdens een
# verbouwing niet in stap.
#
# De NETSTACK zat hier ook in, en dat is 10-08 opgelost bij de bron: lneto en
# go-net zijn nu échte forks met een eigen module-pad (xinix00/lneto,
# xinix00/go-net) en een tag, dus metal requiret ze normaal. Dat moest, want een
# replace geldt alleen in de hoofdmodule — surf loste soypat/lneto dus op naar
# upstream terwijl de node de gepatchte draaide. Twee netstacks in één vloot,
# stil, en dat is erger dan een compilefout. De doorgifte hieronder blijft staan
# als vangnet voor een volgende pad-replace, maar staat vandaag leeg.
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
GONET_DIR="${GONET_DIR:-$HOME/Git/go-net}"

HOPOS_VER="${HOPOS_VER:-v1.12.0-beta.$N}"
SURF_TAG="${SURF_TAG:-beta}"        # rollend, net als surf's rolling-release
SURF_PIN="${SURF_PIN:-beta.$N}"     # vastgepind op deze iteratie

# 0. Preflight. Liever nu luid stoppen dan halverwege een keten publiceren.
for d in "$DIR" "$HOP_DIR" "$SURF_DIR" "$LNETO_DIR" "$GONET_DIR"; do
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
SURF_SHA="$(sha "$SURF_DIR")"; LNETO_SHA="$(sha "$LNETO_DIR")"; GONET_SHA="$(sha "$GONET_DIR")"

echo "== keten-beta $HOPOS_VER ==" >&2
echo "   hop    $HOP_SHA" >&2
echo "   hop-os $HOPOS_SHA" >&2
echo "   surf   $SURF_SHA" >&2
echo "   lneto  $LNETO_SHA" >&2
echo "   go-net $GONET_SHA" >&2

# 1. De replaces erin, met een trap die ze er ALTIJD weer uit haalt — ook bij
#    een gefaalde gate of Ctrl-C. Een achtergebleven pad-replace in main is
#    precies de bus-factor die v1.8.4 wegwerkte.
cp "$DIR/metal/go.mod" "$DIR/metal/go.mod.chainbak"
cp "$SURF_DIR/go.mod" "$SURF_DIR/go.mod.chainbak"
HOP_APP_MODS=""; CFG_BAKS=""
restore() {
	mv -f "$DIR/metal/go.mod.chainbak" "$DIR/metal/go.mod" 2>/dev/null || true
	mv -f "$SURF_DIR/go.mod.chainbak" "$SURF_DIR/go.mod" 2>/dev/null || true
	git -C "$DIR" checkout -q -- metal/go.sum 2>/dev/null || true
	git -C "$SURF_DIR" checkout -q -- go.sum 2>/dev/null || true
	# hop's app-modules en de config-templates die we omleidden (1b/1d).
	for f in $HOP_APP_MODS; do mv -f "$f.chainbak" "$f" 2>/dev/null || true; done
	for f in $CFG_BAKS; do mv -f "$f.chainbak" "$f" 2>/dev/null || true; done
	git -C "$HOP_DIR" checkout -q -- . 2>/dev/null || true
}
trap restore EXIT INT TERM

# hop-os: hop uit de werkboom (die loopt vóór op zijn laatste tag). De overige
# metal heeft sinds 10-08 geen pad-replaces meer (netstack = fork met tag).
( cd "$DIR/metal" && go mod edit -replace "github.com/xinix00/hop=$HOP_DIR" )

# surf: metal uit de werkboom, PLUS élke pad-replace die metal zelf heeft. Die
# doorgifte was de hele reden dat dit script bestond zolang de netstack een
# pad-replace was; nu de forks getagd zijn is de lijst leeg en is dit een
# vangnet. Het blijft staan omdat de volgende pad-replace anders precies
# dezelfde stille splitsing oplevert (surf op upstream, node op de patch) —
# dat kostte de eerste poging een compilefout op SeedNeighbor, en zonder
# compilefout was het onzichtbaar geweest.
#
# Bewust geen lijstje modulenamen hier: dan moet iemand eraan denken als er een
# fork bijkomt. We lezen ze uit metal/go.mod, dus het klopt vanzelf.
REPLACES="$(cd "$DIR/metal" && go mod edit -json | python3 -c '
import json,sys
for r in json.load(sys.stdin).get("Replace") or []:
    new = r["New"]["Path"]
    if new.startswith("/"):            # alleen pad-replaces; versie-replaces zijn geen forks
        print(r["Old"]["Path"] + "=" + new)
')"
[ -n "$REPLACES" ] && echo "   doorgeven aan surf: $(echo "$REPLACES" | tr '\n' ' ')" >&2
( cd "$SURF_DIR" && go mod edit -replace "github.com/xinix00/HopOS/metal=$DIR/metal" )
for r in $REPLACES; do
	( cd "$SURF_DIR" && go mod edit -replace "$r" )
done
( cd "$DIR/metal" && GOWORK=off go mod tidy >/dev/null 2>&1 || true )
( cd "$SURF_DIR" && GOWORK=off go mod tidy >/dev/null 2>&1 || true )

# 1b. HOP's eigen HopOS-apps (welcome, vitals, cloudflared) zijn aparte modules
#     die élk een metal-versie pinnen — en die pins lopen achter (welcome en
#     cloudflared op v1.8.3, dus nog het gVisor-tijdperk). Zonder deze stap
#     draait de beta-node op lneto terwijl zijn EIGEN standaard-job (welcome)
#     een oude metal met gVisor meebrengt: precies de stille splitsing die dit
#     script voor surf al voorkomt, maar dan via de andere kant.
for m in $(cd "$HOP_DIR" && ls -d apps/*/ 2>/dev/null); do
	gm="$HOP_DIR/$m/go.mod"
	grep -q "xinix00/HopOS/metal" "$gm" 2>/dev/null || continue
	cp "$gm" "$gm.chainbak"
	HOP_APP_MODS="$HOP_APP_MODS $gm"
	( cd "$HOP_DIR/$m" && go mod edit -replace "github.com/xinix00/HopOS/metal=$DIR/metal" )
	for r in $REPLACES; do ( cd "$HOP_DIR/$m" && go mod edit -replace "$r" ); done
	( cd "$HOP_DIR/$m" && GOWORK=off go mod tidy >/dev/null 2>&1 || true )
done
[ -n "$HOP_APP_MODS" ] && echo "   hop-apps op lokale metal: $(echo $HOP_APP_MODS | tr ' ' '\n' | wc -l | tr -d ' ') modules" >&2

# 1c. hop + zijn app-elfs publiceren via HOP's eigen release.sh in zijn
#     beta-kanaal (`testing`). Dat script gate't zelf en laat de stabiele
#     rolling-release bewust ongemoeid voor testing-builds, dus dit kan geen
#     stabiele app-URL overschrijven. De tag die eruit komt hebben we hierna
#     nodig: de configs van deze beta moeten ernaar wijzen i.p.v. naar rolling.
echo ">> hop + app-elfs publiceren (release.sh testing)" >&2
( cd "$HOP_DIR/.." && ./release.sh testing ) >"$DIR/metal/out/chain-hop.log" 2>&1 || {
	echo "FOUT: hop release.sh faalde — zie metal/out/chain-hop.log" >&2; exit 1; }
HOP_TAG="$(gh release list --repo xinix00/hop --limit 20 --json tagName,isPrerelease \
	--jq '[.[]|select(.tagName|test("-testing\\."))][0].tagName')"
[ -n "$HOP_TAG" ] || { echo "FOUT: geen testing-tag van hop gevonden" >&2; exit 1; }
# hop's release.sh zet de prerelease-vlag niet, waardoor een testing-build even
# hop's `latest` werd (gemeten bij beta.3). Hier rechtzetten: `latest` hoort
# altijd de laatste STABIELE hop te zijn — daar wijzen andermans jobspecs naar.
gh release edit "$HOP_TAG" --repo xinix00/hop --prerelease >/dev/null 2>&1 || true
echo "   hop-apps op $HOP_TAG (prerelease)" >&2

# 1d. De configs van DEZE beta naar de beta-artifacts laten wijzen. Anders trekt
#     een beta-node zijn welcome/vitals uit rolling-release — de stabiele build,
#     tegen een oude metal. Zelfde reden voor surf.
for cfg in "$DIR"/image/hopos-*.cfg; do
	cp "$cfg" "$cfg.chainbak"
	CFG_BAKS="$CFG_BAKS $cfg"
	sed -i '' \
		-e "s|xinix00/hop/releases/download/rolling-release|xinix00/hop/releases/download/$HOP_TAG|g" \
		-e "s|xinix00/hop-os-surf/releases/download/rolling-release|xinix00/hop-os-surf/releases/download/$SURF_TAG|g" \
		"$cfg"
done

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

Node and apps run the same network stack (lneto, not gVisor) from the same patched sources — see the commit table. App sizes are listed at the bottom.

Built from:

| part | commit |
|---|---|
| hop | \`$HOP_SHA\` |
| hop-os | \`$HOPOS_SHA\` |
| hop-os-surf | \`$SURF_SHA\` |
| lneto (patched) | \`$LNETO_SHA\` |
| go-net (patched) | \`$GONET_SHA\` |

The netstack fixes (TCP window scaling, reassembly and loss recovery, deadline-driven waits, sequential ephemeral ports, ARP/neighbor resolution, listener-pool maintenance) are not upstream yet, so node and apps both build against our fork of lneto and go-net. Those are published modules with a tag, not path replacements, so the stack itself is reproducible: a clone of surf or of an app module gets the patched stack with no go.mod surgery. What still comes from working trees is hop into hop-os, and metal into surf and the app modules — those three move together during a rebuild, so pin the versions above. A stable release will require published versions for all of them, and the fork requires come out once the PRs land.

**Every app in this chain was rebuilt too**, against the same metal and the same patched netstack — the node's own default job included. HOP's app modules pin metal versions that lag (welcome and cloudflared sat on v1.8.3, still the gVisor era), so without that a beta node would have run lneto while its own \`welcome\` brought an old stack along. The boot configs in these images therefore point at the beta artifacts, not at the stable rolling URLs:

- HOP apps (welcome, vitals, cloudflared): https://github.com/xinix00/hop/releases/tag/$HOP_TAG
- SURF apps (display, launcher, taskman, …): https://github.com/xinix00/hop-os-surf/releases/tag/$SURF_TAG"

git -C "$DIR" tag -a "$HOPOS_VER" -m "HopOS $HOPOS_VER (chain beta $N)" 2>/dev/null || true
git -C "$DIR" tag "metal/$HOPOS_VER" 2>/dev/null || true
git -C "$DIR" push -q origin "$HOPOS_VER" "metal/$HOPOS_VER" 2>/dev/null || true
# Eén release-route: release.sh doet het bouwen, tekenen én publiceren, met
# PRERELEASE=1 als enige verschil (dan blijft `latest` op de stabiele versie).
# RELEASE_ALLOW_DIRTY: de dirt is de replace die dit script er zelf in zette.
echo ">> hop-os images bouwen + tekenen (pre-release)" >&2
( cd "$DIR" && PRERELEASE=1 RELEASE_ALLOW_DIRTY=1 sh tools/release.sh "$HOPOS_VER" >/dev/null )
# De keten-notes eroverheen: release.sh schrijft zijn standaard-assetlijst, hier
# komen de vier SHA's bij die deze beta identificeren.
gh release edit "$HOPOS_VER" --repo xinix00/HopOS \
	--title "HopOS $HOPOS_VER — chain beta $N" --notes "$NOTES" >/dev/null

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

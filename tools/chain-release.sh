#!/bin/sh
# chain-release.sh — de STABIELE keten: metal taggen, alle consumers erop
# zetten, app-images bouwen, images ondertekenen en publiceren. In die orde,
# want die orde is niet vrij (zie hieronder).
#
#   tools/chain-release.sh v1.12.0            # de echte release
#   DRYRUN=1 tools/chain-release.sh v1.12.0   # alles bouwen, niets publiceren
#   FROM=4 tools/chain-release.sh v1.12.0     # hervatten vanaf fase 4
#
# HERVATTEN. De keten raakt zes repo's en een half gepubliceerde release is
# geen toestand om met de hand uit te poetsen. Faalt er iets halverwege (11-08:
# surf's push viel om op een credential-helper die alleen in hop-os stond), los
# dat op en start met FROM=<fase>. Alle fasen zijn idempotent: pins die al goed
# staan leveren geen commit op, en assets worden geclobberd.
#
# VERSCHIL MET chain-beta.sh. De beta bouwt uit de werkbomen met tijdelijke
# pad-replaces en draait die terug: niets buiten deze Mac verandert. Deze bouwt
# uit GEPUBLICEERDE versies en COMMIT de bumps in élke repo. Een replace die na
# een release blijft staan is een bus-factor; een pin die achterloopt is erger,
# want die werkt.
#
# DE ORDE, en waarom er geen kringloop is. Twee richtingen, twee verschillende
# modules:
#
#   metal            -> hop            (de kern draagt de orchestrator)
#   hop/apps/*, surf -> metal          (apps linken applib)
#   hopdns/hoplb/hopprom -> metal      (idem, elk in hun eigen repo)
#
# `github.com/xinix00/hop` zelf noemt metal niet in zijn go.mod en geen pakket
# buiten apps/ importeert het. De graaf is dus een DAG en de release-orde is de
# topologische sortering ervan: hop (indien gewijzigd) -> metal -> alle apps.
#
# Waarom de app-images vóór de images komen: een image draagt alleen de URL's
# van zijn jobs, niet hun bytes. Publiceer je de images eerst, dan trekt een
# verse node in dat gat nog de vorige apps.
set -e

VER="${1:?gebruik: tools/chain-release.sh vX.Y.Z}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
EASY="${EASY_DIR:-$HOME/Git/easy}"
HOP_DIR="${HOP_DIR:-$EASY/hop}"
SURF_DIR="${SURF_DIR:-$HOME/Git/hop-os-surf}"
LIST="$DIR/tools/hopos-apps.list"
DRY="${DRYRUN:-}"

case "$VER" in
v*.*.*) ;;
*) echo "FOUT: '$VER' ziet er niet uit als vX.Y.Z" >&2; exit 1 ;;
esac
case "$VER" in
*-*) echo "FOUT: '$VER' heeft een suffix — pre-releases doet chain-beta.sh" >&2; exit 1 ;;
esac

FROM="${FROM:-1}"
say() { echo "$@" >&2; }
want() { [ "$FROM" -le "$1" ]; }   # draaien we deze fase nog?
run() { # run <beschrijving> <commando...> — in DRYRUN alleen melden
	d="$1"; shift
	if [ -n "$DRY" ]; then say "   [dry] $d"; else "$@"; fi
}

# De modulepaden uit de lijst (~ -> $HOME), plus surf: dat zijn alle consumers
# van metal. Één lijst, want twee lijsten die uit elkaar lopen is exact hoe de
# satellieten vier metal-versies achterop raakten.
mods() { # alle consumers van metal
	mods_in; mods_ext
}
# In deze repo: apps/{welcome,vitals,cloudflared}. Hun require is cosmetisch (de
# replace naar ../../metal bepaalt waartegen ze bouwen), maar hij moet kloppen —
# en dus BINNEN de tag vallen. Daarom bumpen we die vóór het taggen.
mods_in() {
	sed 's/#.*//' "$LIST" | grep . | cut -d: -f2 | grep '^\./' | sed "s|^\./|$DIR/|"
}
# Buiten deze repo: de satellieten en surf. Die kunnen pas ná de tag, want zij
# requiren een gepubliceerde versie.
mods_ext() {
	sed 's/#.*//' "$LIST" | grep . | cut -d: -f2 | grep '^~' | sed "s|^~|$HOME|"
	echo "$SURF_DIR"
}

say ""
say "=== HopOS $VER ${DRY:+(DRYRUN — er wordt niets gepubliceerd)} ==="

# ---------------------------------------------------------------- 0. preflight
say ""
say ">> 0. preflight"
for d in "$DIR" "$SURF_DIR" "$HOP_DIR" $(mods); do
	[ -d "$d/.git" ] || [ -d "$d/../.git" ] || [ -d "$d/../../.git" ] || {
		say "FOUT: $d hoort niet bij een git-repo"; exit 1; }
done
# Élke boom die we committen moet schoon zijn: anders weet niemand achteraf wat
# er in de release zat.
# hop staat hier NIET bij: sinds de apps-verhuizing committen we daar niets
# meer. Loopt hop's code vóór, dan checkt fase 1 zijn boom alsnog.
for d in "$DIR" "$SURF_DIR" "$EASY/hopdns" "$EASY/hoplb" "$EASY/hopprom"; do
	[ -z "$(git -C "$d" status --porcelain)" ] || {
		say "FOUT: $(basename "$d") niet schoon — eerst committen"; exit 1; }
done
# Geen pad-replaces in wat we publiceren. Dit is HET verschil met een beta: een
# release die op een pad op deze Mac leunt, bouwt nergens anders.
for gm in "$DIR/metal/go.mod" "$SURF_DIR/go.mod"; do
	! grep -qE '=> +/' "$gm" || { say "FOUT: pad-replace in $gm"; exit 1; }
done
command -v gh >/dev/null || { say "FOUT: gh ontbreekt"; exit 1; }
# MAG DIT TOKEN HIER SCHRIJVEN. Niet "hoe heet het account" maar de vraag die
# telt — en die is los te stellen per repo. Eerder las deze check `gh api user`,
# maar dat is precies de endpoint die tijdens de GitHub-storing van 17-08 als
# enige 503 bleef geven terwijl releases en repos allang antwoordden: dan slaat
# een release af met een account-melding terwijl het account goed staat.
#
# Blijft dekken waarvoor hij bedoeld was: met het verkeerde actieve account is
# push=false op deze repo's (gemeten 11-08 met derekdhaas), en dan stoppen we
# vóór metal getagd is i.p.v. halverwege op een 403.
for r in xinix00/HopOS xinix00/hop-os-surf xinix00/hop; do
	p="$(gh api "repos/$r" --jq .permissions.push 2>/dev/null || true)"
	[ "$p" = "true" ] || {
		echo "FOUT: WRONG USER — dit gh-token mag niet schrijven in $r (push=${p:-onbekend})." >&2
		echo "      herstel: gh auth switch --user xinix00" >&2; exit 1; }
done
WHO="$(gh api user --jq .login 2>/dev/null || echo '?')"

say "   bomen schoon, geen pad-replaces, sleutel aanwezig, gh = $WHO"

# ------------------------------------------------------- 1. hop: nog actueel?
# metal requiret een hop-TAG. Loopt hop's code daarop vóór, dan moet hop eerst
# uitgebracht worden — anders draagt deze release een orchestrator die nergens
# staat.
#
# "Code" is hier exact *.go + go.mod + go.sum, en dat is geen luiheid: hop
# embedt niets (geen enkele go:embed in de repo), dus alleen die drie kunnen de
# binary veranderen. De eerste versie vergeleek de hele boom en wilde vijf
# repo's taggen + notariseren omdat er één regel uit .gitignore was — gemeten in
# de repetitie van 11-08.
say ""
if want 1; then
say ">> 1. hop tegenover metal's require"
HOP_REQ="$(cd "$DIR/metal" && go mod edit -json | sed -n 's/.*"Path": "github.com\/xinix00\/hop",[^}]*"Version": "\([^"]*\)".*/\1/p' | head -1)"
[ -n "$HOP_REQ" ] || HOP_REQ="$(grep -oE 'github.com/xinix00/hop v[0-9][^ ]*' "$DIR/metal/go.mod" | head -1 | awk '{print $2}')"
say "   metal requiret hop $HOP_REQ"
# apps/ negeren we: dat zijn eigen modules die metal requiren, dus die komen in
# fase 3. Alleen hop's EIGEN code kan een hop-release nodig maken.
if ! git -C "$HOP_DIR" rev-parse -q --verify "$HOP_REQ" >/dev/null; then
	say "   LET OP: tag $HOP_REQ staat niet in $HOP_DIR — kan niet vergelijken, hop overgeslagen"
elif git -C "$HOP_DIR" diff --quiet "$HOP_REQ" HEAD -- '*.go' go.mod go.sum ':(exclude)apps'; then
	say "   hop-code identiek aan $HOP_REQ — geen hop-release nodig"
else
	say "   hop's code loopt vóór op $HOP_REQ — hop gaat eerst"
	git -C "$HOP_DIR" diff --stat "$HOP_REQ" HEAD -- '*.go' go.mod go.sum ':(exclude)apps' | tail -3 >&2
	# Dit kan onvoorwaardelijk vóór metal: hop hangt niet aan metal, dus zijn
	# release kan nooit op deze metal wachten. Sinds 11-08 bouwt hop's
	# release.sh alleen de gewone wereld (linux/darwin, amd64/arm64) — de
	# app-images komen uit tools/apps-release.sh, hier in fase 4.
	run "hop release.sh release" sh -c "cd '$EASY' && ./release.sh release >/dev/null"
	if [ -z "$DRY" ]; then
		NEW="$(git -C "$HOP_DIR" tag -l 'v*-release' | sort -V | tail -1)"
		[ -n "$NEW" ] || { say "FOUT: hop-release leverde geen tag"; exit 1; }
		say "   hop $NEW uit; metal's require bijwerken"
		( cd "$DIR/metal" && GOWORK=off go mod edit -require "github.com/xinix00/hop@$NEW" &&
		  GOWORK=off GOFLAGS=-mod=mod GOTOOLCHAIN=local go mod tidy >/dev/null 2>&1 )
		git -C "$DIR" add metal/go.mod metal/go.sum
		git -C "$DIR" commit -q -m "hop $NEW"
		git -C "$DIR" push -q
	fi
fi

fi

# --------------------------------------------------------- 2. metal: gate + tag
say ""
if want 2; then
say ">> 2. de app-modules in deze repo op $VER"
for m in $(mods_in); do
	old="$(grep -oE 'HopOS/metal v[0-9][^ ]*' "$m/go.mod" | head -1 | awk '{print $2}')"
	printf '   %-24s %s -> %s\n' "apps/$(basename "$m")" "${old:-?}" "$VER" >&2
	( cd "$m" && GOWORK=off go mod edit -require "github.com/xinix00/HopOS/metal/v2@$VER" )
done
if [ -n "$(git -C "$DIR" status --porcelain)" ]; then
	run "commit apps-pins" sh -c "
		git -C '$DIR' add -A apps &&
		git -C '$DIR' commit -q -m 'apps: metal $VER' &&
		git -C '$DIR' push -q"
	[ -n "$DRY" ] && git -C "$DIR" checkout -q -- apps
fi

say ">> 2b. gate hop-os"
( cd "$DIR" && sh tools/test.sh >/dev/null )
say "   groen"
say ">> 2c. taggen $VER + metal/$VER"
run "git tag -a $VER + metal/$VER, push" sh -c "
	git -C '$DIR' tag -a '$VER' -m 'HopOS $VER' &&
	git -C '$DIR' tag 'metal/$VER' &&
	git -C '$DIR' push -q origin '$VER' 'metal/$VER'"
# De proxy moet de tag zien vóórdat een consumer hem kan requiren; dat duurt
# soms tientallen seconden.
if [ -z "$DRY" ]; then
	say "   wachten tot de proxy metal@$VER serveert"
	i=0
	until (cd /tmp && GOFLAGS=-mod=mod go list -m "github.com/xinix00/HopOS/metal/v2@$VER" >/dev/null 2>&1); do
		i=$((i + 1)); [ "$i" -lt 30 ] || { say "FOUT: proxy serveert metal@$VER niet"; exit 1; }
		sleep 10
	done
	say "   metal@$VER opvraagbaar"
fi

fi

# ------------------------------------------------- 3. consumers op de nieuwe pin
# In DRYRUN bestaat de tag niet, dus daar zetten we een pad-replace (en draaien
# die terug) — anders is er niets te bouwen en zegt de repetitie niets.
say ""
if want 3; then
say ">> 3. metal-pin bijwerken bij alle consumers"
BAKS=""
restore_dry() {
	for m in $BAKS; do
		mv -f "$m/go.mod.relbak" "$m/go.mod" 2>/dev/null || true
		mv -f "$m/go.sum.relbak" "$m/go.sum" 2>/dev/null || true
	done
}
[ -n "$DRY" ] && trap restore_dry EXIT INT TERM
for m in $(mods_ext); do
	old="$(grep -oE 'HopOS/metal v[0-9][^ ]*' "$m/go.mod" | head -1 | awk '{print $2}')"
	printf '   %-46s %s -> %s\n' "$(basename "$(dirname "$m")")/$(basename "$m")" "${old:-?}" "$VER" >&2
	if [ -n "$DRY" ]; then
		cp "$m/go.mod" "$m/go.mod.relbak"; [ -f "$m/go.sum" ] && cp "$m/go.sum" "$m/go.sum.relbak"
		BAKS="$BAKS $m"
		( cd "$m" && GOWORK=off go mod edit -replace "github.com/xinix00/HopOS/metal/v2=$DIR/metal" )
	else
		( cd "$m" && GOWORK=off go mod edit -require "github.com/xinix00/HopOS/metal/v2@$VER" )
	fi
	( cd "$m" && GOWORK=off GOFLAGS=-mod=mod GOTOOLCHAIN=local go mod tidy >/dev/null 2>&1 || true )
done

say ">> 3b. gate surf (bouwt meteen out/*.elf)"
( cd "$SURF_DIR" && sh tools/test.sh >/dev/null )
say "   groen"

# De app-modules committen we per repo: hop draagt er drie in één commit.
say ">> 3c. bumps committen"
for d in "$EASY/hopdns" "$EASY/hoplb" "$EASY/hopprom" "$SURF_DIR"; do
	[ -n "$(git -C "$d" status --porcelain)" ] || { say "   $(basename "$d") — niets te committen"; continue; }
	run "commit+push in $(basename "$d")" sh -c "
		git -C '$d' add -A &&
		git -C '$d' commit -q -m 'metal $VER' &&
		git -C '$d' push -q"
	[ -n "$DRY" ] || say "   $(basename "$d") ✓"
done

fi

# ----------------------------------------------------------- 4. de app-images
# Eerst de apps, dan de images: de images dragen alleen URL's.
say ""
if want 4; then
say ">> 4. app-images bouwen en publiceren"
if [ -n "$DRY" ]; then
	PUBLISH=0 sh "$DIR/tools/apps-release.sh"
else
	METAL="$VER" sh "$DIR/tools/apps-release.sh"
fi
say ">> 4b. surf-apps publiceren"
run "surf publish-apps.sh" sh -c "cd '$SURF_DIR' && sh tools/publish-apps.sh >/dev/null"

fi

# --------------------------------------------------- 5. de images zelf, laatst
say ""
if want 5; then
say ">> 5. images bouwen, ondertekenen, publiceren"
if [ -n "$DRY" ]; then
	say "   [dry] tools/release.sh $VER"
else
	( cd "$DIR" && sh tools/release.sh "$VER" >/dev/null )
fi

fi

# ------------------------------------------------------------- 6. natrekken
say ""
say ">> 6. natrekken"
if [ -n "$DRY" ]; then
	restore_dry; trap - EXIT INT TERM
	say "   go.mod's teruggedraaid (DRYRUN)"
	for d in "$DIR" "$SURF_DIR" "$HOP_DIR" "$EASY/hopdns" "$EASY/hoplb" "$EASY/hopprom"; do
		printf '   %-14s %s gewijzigd\n' "$(basename "$d")" \
			"$(git -C "$d" status --porcelain | wc -l | tr -d ' ')" >&2
	done
	say ""
	say "DRYRUN klaar — niets getagd, niets gepubliceerd, niets gecommit."
	exit 0
fi
L="$(gh api repos/xinix00/HopOS/releases/latest --jq .tag_name)"
[ "$L" = "$VER" ] || say "LET OP: HopOS latest = $L (verwacht $VER)"
for m in $(mods); do
	p="$(grep -oE 'HopOS/metal v[0-9][^ ]*' "$m/go.mod" | head -1 | awk '{print $2}')"
	[ "$p" = "$VER" ] || say "LET OP: $(basename "$m") pint $p"
done
say "   HopOS latest = $L, alle consumers op $VER"
say ""
say "KLAAR"
say "  images  https://github.com/xinix00/HopOS/releases/tag/$VER"
say "  apps    https://github.com/xinix00/HopOS/releases/tag/apps"
say "  surf    https://github.com/xinix00/hop-os-surf/releases/tag/rolling-release"

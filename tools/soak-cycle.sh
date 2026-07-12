#!/bin/sh
# Soak-cyclusdriver (P2b): recyclet de burn-jobs elk uur — delete → 10s →
# opnieuw plaatsen. Zo test de soak de héle levenscyclus telkens opnieuw:
# submit → S3-fetch (TLS) → checksum → slot-alloc → stage-2-kooi → teardown.
# De dvfs-TERUGKLOK wordt getest binnen de app zelf (burn 10 min / rust 5 min,
# zie appspike burn()) — dit script test de job-churn eromheen.
#
# De S3-key komt uit ~/.hopos/s3-creds.txt (PRIVÉ, buiten de repo) — dit
# script bevat geen geheimen en mag dus wél in git.
#
#   tools/soak-cycle.sh | tee soak-cycle-$(date +%Y%m%d).log

KEY=$(awk -F': *' '/secret_access_key/{print $2}' "$HOME/.hopos/s3-creds.txt")
[ -n "$KEY" ] || { echo "geen S3-key in ~/.hopos/s3-creds.txt"; exit 1; }

PI5=192.168.178.14
PI4=192.168.178.20
PERIOD=3600 # elk uur recyclen
URL5=https://storage.bunnycdn.com/hop-os/app5.elf
URL4=https://storage.bunnycdn.com/hop-os/app4.elf

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }

submit() { # ip url
	for n in 1 2 3; do
		curl -s -m 15 -o /dev/null -X POST "http://$1:9080/v1/jobs" -d '{"name":"burn-'$n'","driver":"hop",
			"artifacts":[{"url":"'"$2"'","headers":{"AccessKey":"'"$KEY"'"}}],
			"memory_limit":268435456,"env":{"BURN":"1","ROLE":"soak"}}'
	done
}

del() { for n in 1 2 3; do curl -s -m 15 -o /dev/null -X DELETE "http://$1:9080/v1/jobs/burn-$n"; done; }
placed() { curl -s -m 5 "http://$1:8080/v1/status" 2>/dev/null | sed 's/.*total_placed"://; s/}.*//'; }

echo "$(ts) soak-cycle start — recycle elke ${PERIOD}s (app doet 10m/5m burn-ritme zelf)"
while true; do
	del "$PI5"; del "$PI4"; sleep 10
	submit "$PI5" "$URL5"; submit "$PI4" "$URL4"; sleep 8
	echo "$(ts) recycle — pi5 placed=$(placed $PI5) pi4 placed=$(placed $PI4)"
	sleep "$PERIOD"
done

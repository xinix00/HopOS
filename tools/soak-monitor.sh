#!/bin/sh
# Soak-monitor (P2b/D10, docs/archief/plan-p2b-soak.md): pollt elke minuut de
# HOP-nodes en logt één regel per node — agent- en leader-health plus
# latency. Een miss print ALARM (en blijft gewoon doorpollen: één hikje is
# meetdata, de reeks is het oordeel). Draaien op de Mac:
#
#   tools/soak-monitor.sh 192.168.178.14 192.168.178.20 | tee soak-$(date +%Y%m%d).log
#
# De UART-capture (debug-probe) is de zwarte doos ernaast: dvfs-telemetrie
# (temp/klok), BURN-regels van de app en eventuele crashes staan dáár.

NODES="${@:-192.168.178.14 192.168.178.20}"
INTERVAL=60

echo "# soak-monitor gestart $(date -u +%Y-%m-%dT%H:%M:%SZ) — nodes: $NODES"
while true; do
	TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	for ip in $NODES; do
		A=$(curl -s -m 5 -o /dev/null -w "%{http_code} %{time_total}" "http://$ip:8080/health" 2>/dev/null)
		L=$(curl -s -m 5 -o /dev/null -w "%{http_code}" "http://$ip:9080/health" 2>/dev/null)
		case "$A" in
		200*) echo "$TS $ip agent=ok(${A#200 }s) leader=$([ "$L" = 200 ] && echo ok || echo "ALARM($L)")" ;;
		*)    echo "$TS $ip ALARM agent=$A leader=$L" ;;
		esac
	done
	sleep $INTERVAL
done

#!/bin/sh
# Checks that every switchyard_* metric the alert rules and the Grafana
# dashboard query is defined in the gateway's code (a string literal in a
# non-test Go file under internal/), so a renamed metric cannot silently
# leave an alert that never fires or a panel that stays empty. Run from the
# repository root; `make check-deploy` runs it.
set -eu

files="deploy/prometheus/alerts.yml deploy/grafana/dashboards/switchyard.json"
names=$(grep -oh 'switchyard_[a-z0-9_]*' $files | sort -u)
defined=$(find internal -name '*.go' ! -name '*_test.go' -exec grep -oh '"switchyard_[a-z0-9_]*"' {} + | tr -d '"' | sort -u)

status=0
for name in $names; do
	base=$name
	case $name in
	*_bucket) base=${name%_bucket} ;;
	*_count) base=${name%_count} ;;
	*_sum) base=${name%_sum} ;;
	esac
	if printf '%s\n' "$defined" | grep -qx -e "$name" -e "$base"; then
		continue
	fi
	echo "check-metrics: FAIL: $name is queried in $files but not defined under internal/" >&2
	status=1
done
if [ $status -eq 0 ]; then
	echo "check-metrics: OK ($(printf '%s\n' "$names" | wc -l | tr -d ' ') metric names)"
fi
exit $status

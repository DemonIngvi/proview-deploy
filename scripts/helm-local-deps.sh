#!/usr/bin/env bash
# Packages the service charts from sibling repositories into helm/proview/charts/
# so the umbrella chart can be linted/installed before charts are published.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p helm/proview/charts
rm -f helm/proview/charts/*.tgz
for r in proview-banner-service proview-event-collector proview-event-ingester proview-analytics proview-web; do
  helm package "../$r/charts/$r" -d helm/proview/charts >/dev/null
done
ls helm/proview/charts

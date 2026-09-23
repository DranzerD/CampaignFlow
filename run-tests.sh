#!/usr/bin/env sh
# Runs both test suites inside the compose network so the integration tests can
# reach PostgreSQL, Redis and RabbitMQ by service name.
#
#   docker compose up -d --build
#   ./run-tests.sh
#
# Unit tests (ad ranking, CTR, privacy suppression, event validation) run
# without any infrastructure; the integration tests are gated on
# INTEGRATION_TEST=1, which this script sets.
set -e

ROOT="$(pwd)"
case "$(uname -s 2>/dev/null)" in
  MINGW* | MSYS* | CYGWIN*) ROOT="$(pwd -W)" ;;  # Docker needs a Windows path
esac
export MSYS_NO_PATHCONV=1

CONTAINER="$(docker compose ps -q ad-service)"
if [ -z "$CONTAINER" ]; then
  echo "compose stack is not running; start it with: docker compose up -d --build" >&2
  exit 1
fi
NETWORK="$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' "$CONTAINER")"

run_suite() {
  service="$1"
  shift
  echo "=== $service ==="
  docker run --rm \
    --network "$NETWORK" \
    -v "$ROOT/$service:/src" \
    -v ad-platform-gomodcache:/go/pkg/mod \
    -w /src \
    -e INTEGRATION_TEST=1 \
    "$@" \
    golang:1.22-alpine go test ./... -count=1 -v
}

run_suite ad-service \
  -e AD_DATABASE_URL=postgres://ads:ads@postgres-ads:5432/ads \
  -e REDIS_URL=redis://redis:6379

run_suite analytics-service \
  -e ANALYTICS_DATABASE_URL=postgres://analytics:analytics@postgres-analytics:5432/analytics

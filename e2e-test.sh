#!/usr/bin/env sh
# End-to-end test that exercises the real publish -> RabbitMQ -> consume ->
# ack lifecycle, as opposed to the unit/integration suites (run-tests.sh)
# which call RecordEvent directly and run the Ad Service with Publisher: nil.
# It talks to the services exactly as a client would, over HTTP.
#
#   docker compose up -d --build
#   ./e2e-test.sh
set -e

AD_URL=${AD_URL:-http://localhost:8080}
ANALYTICS_URL=${ANALYTICS_URL:-http://localhost:8081}
RABBITMQ_MGMT_URL=${RABBITMQ_MGMT_URL:-http://localhost:15672}

fail() { echo "FAIL: $1" >&2; exit 1; }

TODAY=$(date -u +%Y-%m-%d)
CAMPAIGN_END=$(date -u -d "+30 days" +%Y-%m-%d 2>/dev/null || date -u -v+30d +%Y-%m-%d)

echo "== checking RabbitMQ topology exists (proves no cold-start event loss) =="
# The exchange and queue must exist and be bound *before* any event is
# published -- both the Ad Service and the Analytics Service declare this
# topology on connect, whichever one starts first. If only the consumer
# declared it, an Ad Service that starts first would publish into a queue
# that does not exist yet, and RabbitMQ would drop the messages.
BINDINGS=$(curl -s -u guest:guest "$RABBITMQ_MGMT_URL/api/exchanges/%2F/ad-events/bindings/source")
echo "$BINDINGS" | grep -q '"analytics-events"' \
  || fail "queue analytics-events is not bound to exchange ad-events"
echo "ok: analytics-events is bound to ad-events"

echo "== driving the real HTTP path =="
TOKEN=$(curl -s -X POST "$AD_URL/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"advertiser@example.com","password":"password123"}' \
  | sed -E 's/.*"token":"([^"]+)".*/\1/')
[ -n "$TOKEN" ] || fail "login did not return a token"

KEYWORD="e2e-$(date +%s)"
CAMPAIGN=$(curl -s -X POST "$AD_URL/campaigns" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"E2E Campaign\",\"budget_cents\":10000000,\"daily_budget_cents\":1000000,\"start_date\":\"$TODAY\",\"end_date\":\"$CAMPAIGN_END\"}")
CID=$(echo "$CAMPAIGN" | sed -E 's/.*"id":"([^"]+)".*/\1/')
[ -n "$CID" ] || fail "campaign creation failed: $CAMPAIGN"

AD=$(curl -s -X POST "$AD_URL/campaigns/$CID/ads" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"E2E Ad\",\"target_keywords\":[\"$KEYWORD\"],\"bid_cents\":100,\"status\":\"active\"}")
ADID=$(echo "$AD" | sed -E 's/.*"id":"([^"]+)".*/\1/')
[ -n "$ADID" ] || fail "ad creation failed: $AD"

IMPRESSIONS=55
CLICKS=7
i=0
while [ "$i" -lt "$IMPRESSIONS" ]; do
  curl -s -o /dev/null "$AD_URL/serve?keyword=$KEYWORD&limit=1"
  i=$((i + 1))
done
i=0
while [ "$i" -lt "$CLICKS" ]; do
  # Each click needs a distinct caller so the dedup guard (fix for
  # unconstrained clicks) does not collapse them into one.
  curl -s -o /dev/null -X POST "$AD_URL/events/click?ad_id=$ADID" \
    -H "X-Forwarded-For: 203.0.113.$i"
  i=$((i + 1))
done

echo "== waiting for the Analytics consumer to process the queued events =="
ATTEMPT=0
REPORT=""
until [ "$ATTEMPT" -ge 15 ]; do
  REPORT=$(curl -s "$ANALYTICS_URL/reports/campaign/$CID?date=$TODAY")
  echo "$REPORT" | grep -q "\"impressions\":$IMPRESSIONS" && break
  ATTEMPT=$((ATTEMPT + 1))
  sleep 1
done

echo "$REPORT" | grep -q "\"impressions\":$IMPRESSIONS" \
  || fail "expected $IMPRESSIONS impressions after $ATTEMPT s, got: $REPORT"
echo "$REPORT" | grep -q "\"clicks\":$CLICKS" \
  || fail "expected $CLICKS clicks, got: $REPORT"

echo "ok: $REPORT"
echo "PASS: real publish -> RabbitMQ -> consume -> ack -> report path verified end to end"

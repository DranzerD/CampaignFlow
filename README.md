# Mini Ad-Serving & Analytics Platform

A small distributed ad platform in Go: one service manages advertisers,
campaigns and ads and serves ads under a budget, and a second service consumes
the resulting events and reports aggregated statistics. The two services own
separate PostgreSQL databases and talk to each other only through RabbitMQ.

## 1. Project overview

The **Ad Service** (`:8080`) lets an advertiser log in, create campaigns and
ads, and answers `GET /serve?keyword=...` with the best eligible ad. Every ad
served books its bid against the campaign's daily and total budget in Redis and
publishes an impression event. Clicks are reported back through
`POST /events/click`.

The **Analytics Service** (`:8081`) consumes those events, stores them raw,
folds them into a per-campaign-per-day aggregate, and serves
`GET /reports/campaign/{id}` with impressions, clicks and CTR — suppressed when
the campaign-day has fewer than 50 impressions.

## 2. Architecture

```text
                          Client
                            |
                            v
                   +------------------+
                   |   Ad Service     |
                   |     :8080        |
                   +------------------+
                     |      |      |
                     |      |      |
              Postgres(ads) Redis  RabbitMQ (exchange: ad-events)
                                          |
                                          v
                                 +---------------------+
                                 |  Analytics Service  |
                                 |        :8081        |
                                 +---------------------+
                                          |
                                Postgres(analytics)
```

Nothing crosses the dashed line between the two databases: the Ad Service never
reads the analytics database, and the Analytics Service never reads the ad
database.

## 3. Why two services?

* The **Ad Service** owns campaign and ad state, authentication, and the
  latency-sensitive serving path. It must answer `/serve` fast and must never
  overspend a budget.
* The **Analytics Service** owns event ingestion and reporting. Its work is
  bursty and can lag behind without hurting ad delivery.
* They have **separate PostgreSQL databases**, so neither can reach into the
  other's tables; the only contract between them is the JSON event body.
* **RabbitMQ** decouples them. `/serve` returns as soon as the event is queued,
  so an Analytics outage slows down reporting, not ad serving.
* The result is **eventual consistency**: a report reflects an impression a
  moment after it was served, not synchronously with it.

## 4. Technology stack

| Piece | Used for |
| --- | --- |
| Go 1.22 (`net/http`, `chi`) | both services |
| PostgreSQL 16 | two independent databases |
| Redis 7 | keyword cache + atomic daily/total spend counters |
| RabbitMQ 3.13 | impression and click events |
| Docker / Docker Compose | running the whole stack |

Libraries: `go-chi/chi/v5`, `jackc/pgx/v5`, `redis/go-redis/v9`,
`rabbitmq/amqp091-go`, `golang-jwt/jwt/v5`, `google/uuid`,
`golang.org/x/crypto` (bcrypt).

## 5. How to run

```bash
docker compose up --build
```

That starts `postgres-ads`, `postgres-analytics`, `redis`, `rabbitmq`,
`ad-service` and `analytics-service`. Each service creates its own tables on
startup from `migrations/001_init.sql`, so no manual database setup is needed.

Exposed ports: `8080` (Ad Service), `8081` (Analytics Service), `15672`
(RabbitMQ management UI, `guest` / `guest`). Postgres and Redis stay internal
to the compose network.

Health checks:

```bash
curl localhost:8080/health
curl localhost:8081/health
```

## 6. Authentication

Protected endpoints (`POST /campaigns`, `PATCH /campaigns/{id}`,
`POST /campaigns/{id}/ads`) need a bearer JWT. The Ad Service seeds a demo
advertiser on first start:

```text
email:    advertiser@example.com
password: password123
```

The password is stored as a bcrypt hash. `POST /auth/login` returns an HS256
token carrying `advertiser_id`, `email` and `exp` (24 hours). Send it as
`Authorization: Bearer <token>`. An advertiser can only modify campaigns and
create ads under campaigns they own; anything else is `403`.

## 7. API examples

**Login**

```bash
TOKEN=$(curl -s -X POST localhost:8080/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"advertiser@example.com","password":"password123"}' \
  | sed -E 's/.*"token":"([^"]+)".*/\1/')
```

**Create a campaign**

```bash
curl -s -X POST localhost:8080/campaigns \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Gaming Campaign","budget_cents":100000,"daily_budget_cents":10000,"start_date":"2026-09-23","end_date":"2026-10-23"}'
```

**Fetch a campaign** (public)

```bash
curl -s localhost:8080/campaigns/<CAMPAIGN_ID>
```

**Update a campaign**

```bash
curl -s -X PATCH localhost:8080/campaigns/<CAMPAIGN_ID> \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"daily_budget_cents":12000,"status":"paused"}'
```

**Create an ad**

```bash
curl -s -X POST localhost:8080/campaigns/<CAMPAIGN_ID>/ads \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Amazing RPG Game","target_keywords":["gaming","rpg","mobile"],"bid_cents":150,"status":"active"}'
```

**Serve an ad**

```bash
curl -s "localhost:8080/serve?keyword=gaming&limit=1"
# {"ad_id":"...","campaign_id":"...","title":"Amazing RPG Game","bid_cents":150}
```

**Record a click**

```bash
curl -s -X POST "localhost:8080/events/click?ad_id=<AD_ID>"
```

**Get a report**

```bash
curl -s "localhost:8081/reports/campaign/<CAMPAIGN_ID>?date=2026-09-23"
```

Errors always use one shape:

```json
{ "error": "no eligible ad found" }
```

with `400` invalid request, `401` missing/invalid JWT, `403` not your resource,
`404` unknown resource, `409` business conflict, `500` unexpected error.

## 8. Ad-ranking logic

```text
ad active
  → campaign active
  → today within [start_date, end_date]
  → keyword present in target_keywords
  → daily budget has room for the bid
  → total budget has room for the bid
  → highest bid wins
  → created_at (older ad) breaks a tie
```

Status, date and keyword filtering plus ranking happen in
`RankCandidates` (`ad-service/internal/models/selection.go`), which is a pure
function and is unit-tested on its own. The budget step is separate because it
must be atomic: `/serve` walks the ranked list and serves the first ad whose
campaign still has budget, so a budget-exhausted top bidder falls through to
the runner-up instead of returning nothing.

## 9. Redis usage

Redis is used for exactly two things.

**Keyword cache** — `ads:keyword:{keyword}` holds the JSON list of ads
targeting that keyword joined with their campaign (status, dates, budgets), so
the hot serving path does not hit PostgreSQL on every request. Entries carry a
60-second TTL and are explicitly invalidated whenever an ad is created, an ad
or campaign is paused, or a campaign's budget changes.

**Atomic spend counters** — `campaign:{campaign_id}:daily_spend:{YYYY-MM-DD}`
(expiring at the end of the UTC day) and `campaign:{campaign_id}:total_spend`.
Reading the spend, comparing it to the budget and incrementing it are done in a
single Lua script:

```lua
if daily + bid > daily_budget then return -1 end
if total + bid > total_budget then return -2 end
INCRBY daily_spend bid; EXPIRE daily_spend <end of day>
INCRBY total_spend bid
```

Doing this as three separate round trips (`GET`, compare, `INCR`) would let
concurrent `/serve` requests read the same pre-increment value and collectively
overspend. Because the script runs atomically on the Redis server, N concurrent
requests against a budget of `N/2 * bid` produce exactly `N/2` impressions —
this is what `TestConcurrentServeDoesNotOverspend` asserts.

## 10. RabbitMQ reliability

The Ad Service publishes persistent JSON messages to the durable fanout
exchange `ad-events` from a background goroutine, so HTTP handlers never block
on the broker. The Analytics Service binds the durable queue `analytics-events`
to that exchange and consumes with manual acknowledgement:

```text
consume → validate → write to PostgreSQL → ACK
```

If the database write fails the message is **not** acknowledged, so RabbitMQ
redelivers it. That gives **at-least-once** delivery: a message is never lost,
but it may arrive more than once (after a crash between the commit and the
ACK, or on a connection drop).

At-least-once only works if processing is idempotent, so the event UUID is the
primary key of `events_raw`. Each event is inserted with
`ON CONFLICT (id) DO NOTHING` and the `campaign_stats` counter is incremented
**only when that insert actually added a row** — both inside one transaction.
A redelivered event therefore cannot double-count.

Malformed or invalid messages are rejected without requeueing, since retrying
them forever would block the queue.

## 11. Privacy

`GET /reports/campaign/{id}` applies a fixed aggregation threshold:

```text
impressions < 50  →  { "campaign_id": ..., "date": ..., "status": "insufficient data" }
```

This is a deliberate privacy-preserving decision, not a missing feature. Small
aggregates are the ones that leak: with a handful of impressions, anyone who
knows roughly when an ad was shown can infer facts about the individual users
who saw it, and a report that changes from 3 to 4 impressions identifies a
single person's activity. Reporting only above 50 impressions means each
published number describes a group large enough that no single user's behaviour
is visible in it.

The suppressed response omits impressions, clicks and CTR entirely — it does
not return zeros or a rounded value, because either would still carry
information about the true count.

## 12. Testing

Unit tests cover the pure business logic (ad ranking and tie-breaking, CTR,
privacy suppression, event validation) and run with no infrastructure:

```bash
cd ad-service && go test ./... -run 'Rank' -v
```

The integration tests need the compose stack and are skipped unless
`INTEGRATION_TEST=1`:

```bash
docker compose up -d --build
./run-tests.sh
```

`run-tests.sh` runs both suites inside the compose network so they can reach
PostgreSQL, Redis and RabbitMQ by service name. Between them they cover
campaign create/fetch/update, ad creation, login, rejected and foreign-token
requests, highest-bid selection, tie-breaking, paused ads, paused campaigns,
expired campaigns, daily and total budget enforcement, concurrent serving under
a budget, malformed event ids, click deduplication, impression and click
aggregation, duplicate-event idempotency, CTR, and privacy suppression.

**`e2e-test.sh`** covers what the suites above deliberately don't: the real
publish → RabbitMQ → consume → ack lifecycle. The integration tests run the Ad
Service with `Publisher: nil` and call the Analytics `RecordEvent` function
directly, which is the right way to unit-test business logic but never
exercises the broker. `e2e-test.sh` instead drives both services purely over
HTTP -- login, create a campaign and ad, serve real impressions, record real
clicks, and poll the report endpoint until the Analytics consumer has
processed them -- and separately asserts the queue/exchange binding exists via
the RabbitMQ management API, which is what fix #2 below actually guarantees.

```bash
docker compose up -d --build
./e2e-test.sh
```

## 13. Fixes from review

An earlier review of this project found three correctness gaps and a testing
gap. Documenting them here rather than pretending the first version was
already right:

* **Invalid event ids could requeue forever.** `Event.Validate()` originally
  only checked that `id`/`ad_id`/`campaign_id` were non-empty, not that they
  were valid UUIDs. A malformed value passed validation, then failed the
  Postgres `INSERT` (a UUID column), and the consumer's
  ack-after-database-write contract nacked it with `requeue=true` -- so the
  same malformed message would fail identically forever, permanently stuck at
  the head of the queue. Fixed by parsing each id with `uuid.Parse` in
  `Validate()`, so a malformed id is now discarded (no requeue) before it ever
  reaches the database. See
  [`analytics-service/internal/models/models.go`](analytics-service/internal/models/models.go).
* **Events could be lost on a cold start.** The queue and its binding to the
  `ad-events` exchange were declared only by the Analytics consumer. A fanout
  exchange with no bound queue silently discards anything published to it, so
  if the Ad Service served ads before Analytics had started and bound its
  queue, those impressions vanished with no error anywhere. Fixed by having
  the Ad Service's publisher declare the same exchange, queue and binding on
  connect (idempotent, so declaring it twice is harmless) -- whichever service
  starts first now creates the topology. Also enabled RabbitMQ publisher
  confirms, so a broker-side rejection is now visible in the logs instead of
  silently assumed to have succeeded. Verified by starting only the Ad Service
  side of the stack, serving impressions with Analytics still down, and
  confirming they sit in the queue rather than disappear. See
  [`ad-service/internal/queue/publisher.go`](ad-service/internal/queue/publisher.go).
* **Clicks were unauthenticated and unconstrained.** `POST /events/click`
  carries no session or impression identity by spec, so nothing stopped
  unlimited clicks being recorded for any ad -- a real gap for trustworthy
  CTR. Full click-to-impression attribution (a token issued by `/serve` and
  checked on click) would change the fixed request/response shapes the spec
  defines, so instead added a lightweight, spec-compatible guard: an atomic
  Redis `SET NX` rejects a second click for the same `(ad, caller)` pair
  within a short window, returning `409 Conflict` (already one of the spec's
  defined error codes). This is **not** real fraud prevention -- it doesn't
  stop a determined attacker rotating IPs -- it only stops the trivial
  double-submit/retry case from inflating clicks past impressions. Real
  click-to-impression attribution stays a known limitation below. See
  `AllowClick` in
  [`ad-service/internal/cache/cache.go`](ad-service/internal/cache/cache.go).
* **The real message lifecycle was never tested end to end.** The Ad Service
  integration tests run with `Publisher: nil` and the Analytics tests call
  `RecordEvent` directly, so the actual publish → RabbitMQ → consume → ack
  path was only ever exercised by hand. Added `e2e-test.sh` (see above), which
  drives both services over real HTTP and confirms the RabbitMQ topology
  itself, not just the business logic.

## 14. Known limitations

This is a teaching-sized system, not production advertising infrastructure.

* **Spend lives in Redis.** Both the daily and the total spend counters are
  Redis keys. If Redis loses its data, spend resets and a campaign can serve
  past its budget. A real system would reconcile against a durable ledger.
* **Spend is booked on serve, not on a verified impression.** There is no
  viewability check and no refund path if the impression never renders.
* **Days are UTC.** Daily budgets and report dates use UTC, not the
  advertiser's timezone.
* **`limit` on `/serve` is validated but only the single best ad is returned**,
  which is what the response shape describes.
* **Events are still published fire-and-forget from the handler's point of
  view.** Publisher confirms are now enabled and checked (see the fixes
  above), so a broker-side rejection is logged rather than assumed to have
  succeeded -- but if the buffer to the background publisher goroutine is
  full, the event is still dropped with a log line rather than slowing down
  serving. The at-least-once guarantee covers the broker-to-consumer hop, not
  a full-buffer drop before the message ever reaches the broker.
* **Click deduplication is a short per-(ad, IP) debounce, not real
  attribution.** It stops accidental double-submits, not a determined client
  spoofing its address or a script clicking from many IPs. Real click fraud
  prevention needs clicks tied to a specific served impression (a token
  issued by `/serve` and checked on click), which is a bigger change to the
  spec's fixed API shapes than this project makes.
* **`events_raw` grows without bound.** Nothing prunes it; a real deployment
  would expire raw events once aggregated.
* **No pagination, no listing endpoints, no advertiser sign-up.** The demo
  advertiser is seeded at startup with a known password.
* **Cache invalidation is coarse.** Any ad or campaign write clears the whole
  keyword cache rather than just the affected keywords.
* **JWT secret and database passwords are the example values** from
  `.env.example`. They are fine for a local demo and nowhere else.

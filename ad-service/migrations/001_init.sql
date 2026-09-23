-- Ad Service schema.
CREATE TABLE IF NOT EXISTS advertisers (
    id            UUID PRIMARY KEY,
    name          TEXT NOT NULL,
    email         TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS campaigns (
    id                 UUID PRIMARY KEY,
    advertiser_id      UUID NOT NULL REFERENCES advertisers(id),
    name               TEXT NOT NULL,
    budget_cents       BIGINT NOT NULL,
    daily_budget_cents BIGINT NOT NULL,
    status             TEXT NOT NULL,
    start_date         DATE NOT NULL,
    end_date           DATE NOT NULL,
    created_at         TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ads (
    id              UUID PRIMARY KEY,
    campaign_id     UUID NOT NULL REFERENCES campaigns(id),
    title           TEXT NOT NULL,
    target_keywords JSONB NOT NULL,
    bid_cents       BIGINT NOT NULL,
    status          TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS ads_campaign_id_idx ON ads (campaign_id);
CREATE INDEX IF NOT EXISTS ads_target_keywords_idx ON ads USING GIN (target_keywords);

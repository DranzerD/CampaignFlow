-- Analytics Service schema. Completely separate database from the Ad Service.
CREATE TABLE IF NOT EXISTS events_raw (
    id          UUID PRIMARY KEY,
    ad_id       UUID NOT NULL,
    campaign_id UUID NOT NULL,
    event_type  TEXT NOT NULL,
    timestamp   TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS campaign_stats (
    campaign_id UUID NOT NULL,
    date        DATE NOT NULL,
    impressions BIGINT NOT NULL DEFAULT 0,
    clicks      BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (campaign_id, date)
);

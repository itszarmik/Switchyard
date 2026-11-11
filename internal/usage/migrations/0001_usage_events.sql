-- The usage ledger (DESIGN §12.2): one append-only row per gateway request,
-- partitioned by month on ts. The ledger creates the monthly partitions
-- (usage_events_pYYYYMM) itself, at startup and before writing a new month.
-- Money columns are exact decimals in USD, at femto-USD (1e-15) scale.
CREATE TABLE usage_events (
    id                  text             NOT NULL,
    ts                  timestamptz      NOT NULL,
    tenant              text             NOT NULL,
    key_id              text             NOT NULL,
    endpoint            text             NOT NULL,
    requested_model     text             NOT NULL,
    alias               text             NOT NULL,
    deployment          text             NOT NULL,
    provider            text             NOT NULL,
    stream              boolean          NOT NULL,
    status              integer          NOT NULL,
    attempts            integer          NOT NULL,
    fallback_reason     text             NOT NULL,
    route_reason        text             NOT NULL,
    cache_status        text             NOT NULL,
    input_tokens        bigint           NOT NULL,
    cached_input_tokens bigint           NOT NULL,
    cache_write_tokens  bigint           NOT NULL,
    output_tokens       bigint           NOT NULL,
    usage_known         boolean          NOT NULL,
    usage_estimated     boolean          NOT NULL,
    cost_usd            numeric(38, 15)  NOT NULL,
    cost_known          boolean          NOT NULL,
    -- The prices the cost was computed from, as exact decimal strings in USD
    -- per million tokens: {"input": "0.15", "output": "0.6", ...}.
    pricing             jsonb            NOT NULL,
    baseline_model      text             NOT NULL,
    baseline_cost_usd   numeric(38, 15)  NOT NULL,
    overhead_cost_usd   numeric(38, 15)  NOT NULL,
    latency_ms          double precision NOT NULL,
    ttft_ms             double precision NOT NULL,
    tags                jsonb            NOT NULL,
    config_version      text             NOT NULL,
    -- The key makes a retried batch idempotent: the writer inserts with
    -- ON CONFLICT DO NOTHING. A partitioned table's key must include ts.
    PRIMARY KEY (ts, id)
) PARTITION BY RANGE (ts);

CREATE INDEX usage_events_tenant_ts ON usage_events (tenant, ts);

-- Phase 8 quality feedback (DESIGN §10.7, §12.4).
--
-- usage_events.category is the quality category each routed request was
-- assessed in; reports attribute shadow verification and verified savings
-- by it. Rows written before this migration read as no category.
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS category text NOT NULL DEFAULT '';

-- shadow_evals holds one row per shadow-sampled request: which answers were
-- compared and the judge's verdict, never the prompt or the answers. It
-- joins to usage_events on (tenant, request_id) = (tenant, id). A second
-- evaluation of the same request is ignored (ON CONFLICT DO NOTHING).
CREATE TABLE IF NOT EXISTS shadow_evals (
    tenant               text             NOT NULL,
    request_id           text             NOT NULL,
    ts                   timestamptz      NOT NULL,
    key_id               text             NOT NULL,
    category             text             NOT NULL,
    difficulty           double precision NOT NULL,
    profile              text             NOT NULL,
    mode                 text             NOT NULL,
    candidate_deployment text             NOT NULL,
    reference_deployment text             NOT NULL,
    judge_model          text             NOT NULL,
    -- outcome is '' when error_kind says why there is no verdict.
    outcome              text             NOT NULL,
    acceptable           boolean          NOT NULL,
    -- The comparison and judge calls' cost, exact where known and the
    -- reserved estimate otherwise, in USD at femto-USD scale.
    shadow_cost_usd      numeric(38, 15)  NOT NULL,
    error_kind           text             NOT NULL,
    PRIMARY KEY (tenant, request_id)
);

CREATE INDEX IF NOT EXISTS shadow_evals_tenant_ts ON shadow_evals (tenant, ts);

-- feedback holds a client's latest score for one of its tenant's requests
-- (POST /switchyard/v1/feedback). It joins to usage_events on
-- (tenant, request_id) = (tenant, id), so feedback on another tenant's
-- request ID joins to nothing.
CREATE TABLE IF NOT EXISTS feedback (
    tenant     text        NOT NULL,
    request_id text        NOT NULL,
    key_id     text        NOT NULL,
    ts         timestamptz NOT NULL,
    score      smallint    NOT NULL CHECK (score BETWEEN -1 AND 1),
    -- The client's comment, at most 500 characters. Never logged.
    comment    text        NOT NULL,
    PRIMARY KEY (tenant, request_id)
);

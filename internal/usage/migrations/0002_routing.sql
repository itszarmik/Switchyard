-- Phase 5 routing fields (DESIGN §12.2, §12.4): the profile and routing
-- mode of each request and, in observe mode, the deployment the router
-- would have chosen with the request's tokens priced at it. Rows written
-- before this migration read as no profile, no routing mode and no shadow
-- decision. ADD COLUMN on the partitioned table adds the columns to every
-- partition; IF NOT EXISTS keeps the migration safe to re-run.
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS profile           text            NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS routing_mode      text            NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS shadow_deployment text            NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS shadow_cost_usd   numeric(38, 15) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS shadow_cost_known boolean         NOT NULL DEFAULT false;

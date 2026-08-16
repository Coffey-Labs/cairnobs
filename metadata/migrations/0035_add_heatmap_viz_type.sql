-- Phase 5 added a heatmap panel type (api/dashboards/types.go's
-- validVizType()) but missed updating the DB-level check constraint
-- that mirrors it, so heatmap panels passed Go validation and then
-- failed on insert. Postgres has no ALTER CHECK, so drop and recreate.
ALTER TABLE dashboard_panels DROP CONSTRAINT dashboard_panels_viz_type_check;

ALTER TABLE dashboard_panels
    ADD CONSTRAINT dashboard_panels_viz_type_check
    CHECK (viz_type IN ('table', 'line', 'bar', 'single_stat', 'top_n', 'heatmap'));

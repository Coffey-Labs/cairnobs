CREATE TABLE IF NOT EXISTS dashboard_panels
(
    id                UUID PRIMARY KEY,
    dashboard_id      UUID NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
    title             TEXT NOT NULL DEFAULT '',
    query             TEXT NOT NULL,
    query_language    TEXT NOT NULL DEFAULT '',
    viz_type          TEXT NOT NULL CHECK (viz_type IN ('table', 'line', 'bar', 'single_stat', 'top_n')),
    viz_config        JSONB NOT NULL DEFAULT '{}',
    position_x        INT NOT NULL,
    position_y        INT NOT NULL,
    width             INT NOT NULL,
    height            INT NOT NULL,
    earliest_override TEXT,
    latest_override   TEXT,
    sort_order        INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
)

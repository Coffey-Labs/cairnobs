// Package dashboards implements CRUD for saved, multi-panel dashboards
// -- see /docs/phase-3-dashboard-design.md. Deliberately pure CRUD: panel
// *query execution* happens client-side (the web UI calls the existing
// POST /query per panel), so this package never touches querylang.
package dashboards

import (
	"encoding/json"
	"fmt"
	"time"
)

// VizType is one of the panel visualization kinds. "top_n" renders
// through the same path as "table" -- the query itself already did the
// sort/limit -- so there's no execution-side difference, only UI framing.
type VizType string

const (
	VizTable      VizType = "table"
	VizLine       VizType = "line"
	VizBar        VizType = "bar"
	VizSingleStat VizType = "single_stat"
	VizTopN       VizType = "top_n"
)

func validVizType(v VizType) bool {
	switch v {
	case VizTable, VizLine, VizBar, VizSingleStat, VizTopN:
		return true
	default:
		return false
	}
}

type Dashboard struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenant_id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	DefaultEarliest string    `json:"default_earliest"`
	DefaultLatest   string    `json:"default_latest"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Panels          []Panel   `json:"panels,omitempty"`
}

type Panel struct {
	ID               string          `json:"id"`
	DashboardID      string          `json:"dashboard_id"`
	Title            string          `json:"title"`
	Query            string          `json:"query"`
	QueryLanguage    string          `json:"query_language"`
	VizType          VizType         `json:"viz_type"`
	VizConfig        json.RawMessage `json:"viz_config,omitempty"`
	PositionX        int             `json:"position_x"`
	PositionY        int             `json:"position_y"`
	Width            int             `json:"width"`
	Height           int             `json:"height"`
	EarliestOverride *string         `json:"earliest_override,omitempty"`
	LatestOverride   *string         `json:"latest_override,omitempty"`
	SortOrder        int             `json:"sort_order"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// validatePanel enforces the two rules /docs/phase-3-dashboard-design.md
// states as disclosed non-goals rather than silent gaps: raw-SQL panels
// aren't supported (time-range injection has no reliable splice point
// into arbitrary SQL), and viz_type must be one this API knows how to
// store/render.
func validatePanel(p *Panel) error {
	if p.Query == "" {
		return fmt.Errorf("query must not be empty")
	}
	if p.QueryLanguage == "sql" {
		return fmt.Errorf("raw-SQL panels are not supported -- dashboards only support pipe-syntax queries, since the dashboard time-range picker is injected as leading query terms")
	}
	if !validVizType(p.VizType) {
		return fmt.Errorf("viz_type must be one of table, line, bar, single_stat, top_n, got %q", p.VizType)
	}
	if len(p.VizConfig) == 0 {
		p.VizConfig = json.RawMessage(`{}`)
	}
	return nil
}

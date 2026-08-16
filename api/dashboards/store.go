package dashboards

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by Get/Delete when the id doesn't exist --
// including when it exists but belongs to a different tenant (see this
// file's tenant-scoping comment below): a 404 either way, never a 403
// that would confirm cross-tenant existence.
var ErrNotFound = errors.New("not found")

// Store is the pgx-backed CRUD implementation. IDs are assigned
// server-side (google/uuid), matching how /ingest assigns record_id --
// one place (Go) generates IDs, not split between the app and the
// database via a Postgres extension.
//
// Every method below except CreateDashboard/ImportDashboard takes a
// tenantID and filters by it (`WHERE ... AND tenant_id = $N`, or a join
// through dashboards for the panel methods, since dashboard_panels has
// no tenant_id column of its own). This is Phase 4 task 5/8 tenant
// scoping, added after the authz RBAC wiring shipped without it -- see
// /docs/security/threat-model.md's "application-layer tenant scoping"
// section for why that gap mattered even with RBAC live: a role check
// alone answers "is this identity allowed to edit *some* dashboard,"
// not "is this identity allowed to touch *this* dashboard." The
// handler (handler.go) resolves tenantID from the authenticated
// identity (authz.IdentityFromContext) -- never from a client-supplied
// request field, since Dashboard.TenantID is a JSON field a request
// body can set arbitrarily.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateDashboard trusts d.TenantID -- callers (handler.go) must set it
// from the authenticated identity before calling, never from client
// input. Not itself tenant-scoped (there's nothing to scope against
// yet; the row doesn't exist).
func (s *Store) CreateDashboard(ctx context.Context, d *Dashboard) error {
	d.ID = uuid.NewString()
	if d.TenantID == "" {
		d.TenantID = "default"
	}
	if d.CreatedBy == "" {
		d.CreatedBy = "anonymous"
	}
	if d.DefaultEarliest == "" {
		d.DefaultEarliest = "-1h"
	}
	if d.DefaultLatest == "" {
		d.DefaultLatest = "now"
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO dashboards (id, tenant_id, name, description, default_earliest, default_latest, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, updated_at`,
		d.ID, d.TenantID, d.Name, d.Description, d.DefaultEarliest, d.DefaultLatest, d.CreatedBy)
	return row.Scan(&d.CreatedAt, &d.UpdatedAt)
}

func (s *Store) ListDashboards(ctx context.Context, tenantID string) ([]Dashboard, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, name, description, default_earliest, default_latest, created_by, created_at, updated_at
		FROM dashboards WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Dashboard
	for rows.Next() {
		var d Dashboard
		if err := rows.Scan(&d.ID, &d.TenantID, &d.Name, &d.Description, &d.DefaultEarliest, &d.DefaultLatest, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDashboard(ctx context.Context, tenantID, id string) (*Dashboard, error) {
	var d Dashboard
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, description, default_earliest, default_latest, created_by, created_at, updated_at
		FROM dashboards WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	if err := row.Scan(&d.ID, &d.TenantID, &d.Name, &d.Description, &d.DefaultEarliest, &d.DefaultLatest, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	panels, err := s.listPanels(ctx, id)
	if err != nil {
		return nil, err
	}
	d.Panels = panels
	return &d, nil
}

// listPanels doesn't itself take a tenantID -- every call site first
// resolves the owning dashboard via a tenant-scoped query (GetDashboard
// above, or dashboardTenantMatches below), so by the time this runs,
// dashboardID is already known to belong to the caller's tenant.
func (s *Store) listPanels(ctx context.Context, dashboardID string) ([]Panel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, dashboard_id, title, query, query_language, viz_type, viz_config,
		       position_x, position_y, width, height, earliest_override, latest_override,
		       sort_order, created_at, updated_at
		FROM dashboard_panels WHERE dashboard_id = $1 ORDER BY sort_order, created_at`, dashboardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Panel
	for rows.Next() {
		var p Panel
		if err := rows.Scan(&p.ID, &p.DashboardID, &p.Title, &p.Query, &p.QueryLanguage, &p.VizType, &p.VizConfig,
			&p.PositionX, &p.PositionY, &p.Width, &p.Height, &p.EarliestOverride, &p.LatestOverride,
			&p.SortOrder, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// dashboardTenantMatches is the join every panel-mutating method below
// uses in place of a tenant_id column dashboard_panels doesn't have --
// "does this dashboard exist AND belong to this tenant." A plain
// EXISTS query, not a full row fetch: the panel methods that call this
// only need a yes/no gate, not the dashboard's data.
func (s *Store) dashboardTenantMatches(ctx context.Context, tenantID, dashboardID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM dashboards WHERE id = $1 AND tenant_id = $2)`,
		dashboardID, tenantID,
	).Scan(&exists)
	return exists, err
}

func (s *Store) UpdateDashboard(ctx context.Context, tenantID string, d *Dashboard) error {
	if d.DefaultEarliest == "" {
		d.DefaultEarliest = "-1h"
	}
	if d.DefaultLatest == "" {
		d.DefaultLatest = "now"
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE dashboards SET name = $1, description = $2, default_earliest = $3, default_latest = $4, updated_at = now()
		WHERE id = $5 AND tenant_id = $6
		RETURNING tenant_id, created_by, created_at, updated_at`,
		d.Name, d.Description, d.DefaultEarliest, d.DefaultLatest, d.ID, tenantID)
	if err := row.Scan(&d.TenantID, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (s *Store) DeleteDashboard(ctx context.Context, tenantID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dashboards WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) AddPanel(ctx context.Context, tenantID, dashboardID string, p *Panel) error {
	if err := validatePanel(p); err != nil {
		return err
	}
	ok, err := s.dashboardTenantMatches(ctx, tenantID, dashboardID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	p.ID = uuid.NewString()
	p.DashboardID = dashboardID
	row := s.pool.QueryRow(ctx, `
		INSERT INTO dashboard_panels (id, dashboard_id, title, query, query_language, viz_type, viz_config,
		                               position_x, position_y, width, height, earliest_override, latest_override, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING created_at, updated_at`,
		p.ID, p.DashboardID, p.Title, p.Query, p.QueryLanguage, p.VizType, p.VizConfig,
		p.PositionX, p.PositionY, p.Width, p.Height, p.EarliestOverride, p.LatestOverride, p.SortOrder)
	return row.Scan(&p.CreatedAt, &p.UpdatedAt)
}

func (s *Store) UpdatePanel(ctx context.Context, tenantID string, p *Panel) error {
	if err := validatePanel(p); err != nil {
		return err
	}
	ok, err := s.dashboardTenantMatches(ctx, tenantID, p.DashboardID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE dashboard_panels SET
			title = $1, query = $2, query_language = $3, viz_type = $4, viz_config = $5,
			position_x = $6, position_y = $7, width = $8, height = $9,
			earliest_override = $10, latest_override = $11, sort_order = $12, updated_at = now()
		WHERE id = $13 AND dashboard_id = $14`,
		p.Title, p.Query, p.QueryLanguage, p.VizType, p.VizConfig,
		p.PositionX, p.PositionY, p.Width, p.Height,
		p.EarliestOverride, p.LatestOverride, p.SortOrder, p.ID, p.DashboardID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeletePanel(ctx context.Context, tenantID, dashboardID, panelID string) error {
	ok, err := s.dashboardTenantMatches(ctx, tenantID, dashboardID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM dashboard_panels WHERE id = $1 AND dashboard_id = $2`, panelID, dashboardID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ImportDashboard creates a new dashboard and all its panels from an
// exported Dashboard document, assigning fresh IDs throughout -- so
// importing an exported dashboard into a different environment (or
// re-importing into the same one) never collides with the source IDs.
// Runs in one transaction: either the whole dashboard lands, or none of
// it does. tenantID comes from the caller (the authenticated identity),
// never from d.TenantID -- an exported dashboard JSON file carries
// whatever tenant_id it was exported from, and importing it must not
// let that value silently re-assign the dashboard to a different
// tenant than the importing user's own.
func (s *Store) ImportDashboard(ctx context.Context, tenantID string, d *Dashboard) (*Dashboard, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	id := uuid.NewString()
	createdBy := d.CreatedBy
	if createdBy == "" {
		createdBy = "anonymous"
	}
	earliest := d.DefaultEarliest
	if earliest == "" {
		earliest = "-1h"
	}
	latest := d.DefaultLatest
	if latest == "" {
		latest = "now"
	}

	var out Dashboard
	out.ID, out.TenantID, out.Name, out.Description = id, tenantID, d.Name, d.Description
	out.DefaultEarliest, out.DefaultLatest, out.CreatedBy = earliest, latest, createdBy

	row := tx.QueryRow(ctx, `
		INSERT INTO dashboards (id, tenant_id, name, description, default_earliest, default_latest, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, updated_at`,
		out.ID, out.TenantID, out.Name, out.Description, out.DefaultEarliest, out.DefaultLatest, out.CreatedBy)
	if err := row.Scan(&out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, err
	}

	for _, p := range d.Panels {
		if err := validatePanel(&p); err != nil {
			return nil, fmt.Errorf("panel %q: %w", p.Title, err)
		}
		p.ID = uuid.NewString()
		p.DashboardID = out.ID
		prow := tx.QueryRow(ctx, `
			INSERT INTO dashboard_panels (id, dashboard_id, title, query, query_language, viz_type, viz_config,
			                               position_x, position_y, width, height, earliest_override, latest_override, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			RETURNING created_at, updated_at`,
			p.ID, p.DashboardID, p.Title, p.Query, p.QueryLanguage, p.VizType, p.VizConfig,
			p.PositionX, p.PositionY, p.Width, p.Height, p.EarliestOverride, p.LatestOverride, p.SortOrder)
		if err := prow.Scan(&p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out.Panels = append(out.Panels, p)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

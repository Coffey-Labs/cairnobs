// Package notifystore is pgx-backed CRUD for notification_targets.
package notifystore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

type Kind string

const (
	KindWebhook   Kind = "webhook"
	KindSlack     Kind = "slack"
	KindPagerDuty Kind = "pagerduty"
)

func ValidKind(k Kind) bool {
	switch k {
	case KindWebhook, KindSlack, KindPagerDuty:
		return true
	default:
		return false
	}
}

type Target struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenant_id"`
	Name            string          `json:"name"`
	Kind            Kind            `json:"kind"`
	WebhookURL      string          `json:"webhook_url"`
	PayloadTemplate *string         `json:"payload_template,omitempty"`
	Headers         json.RawMessage `json:"headers,omitempty"`
	Secret          *string         `json:"secret,omitempty"`
	CreatedBy       string          `json:"created_by"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Create(ctx context.Context, t *Target) error {
	t.ID = uuid.NewString()
	if t.TenantID == "" {
		t.TenantID = "default"
	}
	if t.CreatedBy == "" {
		t.CreatedBy = "anonymous"
	}
	if len(t.Headers) == 0 {
		t.Headers = json.RawMessage(`{}`)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_targets (id, tenant_id, name, kind, webhook_url, payload_template, headers, secret, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.ID, t.TenantID, t.Name, t.Kind, t.WebhookURL, t.PayloadTemplate, t.Headers, t.Secret, t.CreatedBy)
	return err
}

func (s *Store) List(ctx context.Context) ([]Target, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, name, kind, webhook_url, payload_template, headers, secret, created_by
		FROM notification_targets ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Target
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.ID, &t.TenantID, &t.Name, &t.Kind, &t.WebhookURL, &t.PayloadTemplate, &t.Headers, &t.Secret, &t.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (*Target, error) {
	var t Target
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, kind, webhook_url, payload_template, headers, secret, created_by
		FROM notification_targets WHERE id = $1`, id)
	if err := row.Scan(&t.ID, &t.TenantID, &t.Name, &t.Kind, &t.WebhookURL, &t.PayloadTemplate, &t.Headers, &t.Secret, &t.CreatedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM notification_targets WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

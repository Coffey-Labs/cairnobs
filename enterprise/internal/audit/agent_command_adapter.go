// Adapts *Store to api/agents.CommandLogger -- same shape as
// ai_interaction_adapter.go's AIInteractionLogger, wired in by
// enterprise/cmd/enterprise-api alongside it.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sentry/sentry/api/agents"
	"github.com/sentry/sentry/api/authz"
)

// AgentCommandLogger implements agents.CommandLogger by translating its
// CommandLogEntry into this package's Entry, reading tenant/user
// identity from ctx -- same "read identity from ctx rather than the
// interface growing tenant-awareness" shape as every other adapter in
// this package.
type AgentCommandLogger struct {
	store  *Store
	source Source
}

func NewAgentCommandLogger(store *Store, source Source) *AgentCommandLogger {
	return &AgentCommandLogger{store: store, source: source}
}

type agentCommandDetail struct {
	Host    string `json:"host"`
	Command string `json:"command"`
}

func (l *AgentCommandLogger) LogCommand(ctx context.Context, entry agents.CommandLogEntry) error {
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok || identity.TenantID == "" {
		return fmt.Errorf("audit: no tenant identity in context, refusing to write an unattributable audit entry")
	}

	var userID *string
	if identity.UserID != "" {
		userID = &identity.UserID
	}

	detail, err := json.Marshal(agentCommandDetail{Host: entry.Host, Command: entry.Command})
	if err != nil {
		return fmt.Errorf("audit: marshaling agent command detail: %w", err)
	}

	_, err = l.store.Append(ctx, Entry{
		TenantID:  identity.TenantID,
		UserID:    userID,
		Source:    l.source,
		EventType: EventAgentCommand,
		Status:    StatusSuccess,
		Detail:    detail,
	})
	return err
}

// Adapts *Store to api/ai/aiapi.InteractionLogger -- same shape as
// queryapi_adapter.go's QueryAPILogger, wired in by
// enterprise/cmd/enterprise-api alongside it (Phase 7 task 12).
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cairnobs/cairnobs/api/ai/aiapi"
	"github.com/cairnobs/cairnobs/api/authz"
)

// AIInteractionLogger implements aiapi.InteractionLogger by translating
// its InteractionEntry into this package's Entry, reading tenant/user
// identity from ctx -- same "read identity from ctx rather than the
// interface growing tenant-awareness" shape as QueryAPILogger.
type AIInteractionLogger struct {
	store  *Store
	source Source
}

func NewAIInteractionLogger(store *Store, source Source) *AIInteractionLogger {
	return &AIInteractionLogger{store: store, source: source}
}

// aiInteractionDetail is what Detail carries -- Operation/Confidence/
// Accepted/Edited don't have dedicated audit_log columns (same reasoning
// as role_change/grant_change already using Detail instead of
// query_text/row_count/duration_ms), only QueryText (FinalQuery) does.
type aiInteractionDetail struct {
	Operation  string `json:"operation"`
	Input      string `json:"input"`
	Output     string `json:"output"`
	Confidence string `json:"confidence,omitempty"`
	Accepted   bool   `json:"accepted"`
	Edited     bool   `json:"edited"`
}

func (l *AIInteractionLogger) LogInteraction(ctx context.Context, entry aiapi.InteractionEntry) error {
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok || identity.TenantID == "" {
		return fmt.Errorf("audit: no tenant identity in context, refusing to write an unattributable audit entry")
	}

	var userID *string
	if identity.UserID != "" {
		userID = &identity.UserID
	}

	detail, err := json.Marshal(aiInteractionDetail{
		Operation:  entry.Operation,
		Input:      entry.Input,
		Output:     entry.Output,
		Confidence: entry.Confidence,
		Accepted:   entry.Accepted,
		Edited:     entry.Edited,
	})
	if err != nil {
		return fmt.Errorf("audit: marshaling ai interaction detail: %w", err)
	}

	var queryText *string
	if entry.FinalQuery != "" {
		queryText = &entry.FinalQuery
	}

	_, err = l.store.Append(ctx, Entry{
		TenantID:  identity.TenantID,
		UserID:    userID,
		Source:    l.source,
		EventType: EventAIInteraction,
		QueryText: queryText,
		Status:    StatusSuccess,
		Detail:    detail,
	})
	return err
}

// Adapts *Store to api/queryapi.AuditLogger -- the interface core
// defines and has carried as a nil-by-default field
// (api/queryapi.Handler.audit) since Phase 4 task 4, waiting on exactly
// this: a real implementation, wired in by enterprise/cmd/enterprise-api
// (the one binary allowed to import both packages -- see chrunner's doc
// comment on the enterprise->api import direction).
package audit

import (
	"context"
	"fmt"

	"github.com/sentry/sentry/api/authz"
	"github.com/sentry/sentry/api/queryapi"
)

// QueryAPILogger implements queryapi.AuditLogger by translating its
// tenant-agnostic QueryAuditEntry into this package's Entry, reading
// tenant/user identity from ctx -- exactly the shape
// queryapi.AuditLogger's doc comment describes: "an enterprise-side
// implementation reads identity from ctx rather than this interface
// growing tenant-awareness."
type QueryAPILogger struct {
	store  *Store
	source Source
}

// NewQueryAPILogger wraps store for use as a specific Source -- api's
// queryapi.Handler and /alerting's evaluations go through different
// enterprise-api-fronted paths today (SourceAPI is the only one
// actually wired to a real HTTP handler; SourceAlerting is named for
// when alerting's queries get audited the same way, not yet built).
func NewQueryAPILogger(store *Store, source Source) *QueryAPILogger {
	return &QueryAPILogger{store: store, source: source}
}

func (l *QueryAPILogger) LogQuery(ctx context.Context, entry queryapi.QueryAuditEntry) error {
	identity, ok := authz.IdentityFromContext(ctx)
	if !ok || identity.TenantID == "" {
		// Fail open at the queryapi.Handler call site already covers
		// "don't take down the query path" -- this specific error tells
		// that fail-open path *why* the write didn't happen, distinct
		// from a real audit-storage failure, since chrunner.RunSQL
		// would already have refused the query itself in this case (see
		// that package's RunSQL) -- this branch mostly protects against
		// a future caller that skips chrunner's own check.
		return fmt.Errorf("audit: no tenant identity in context, refusing to write an unattributable audit entry")
	}

	status := StatusSuccess
	var errMsg *string
	if !entry.Success {
		status = StatusError
		errMsg = &entry.Error
	}
	var userID *string
	if identity.UserID != "" {
		userID = &identity.UserID
	}
	queryText := entry.Query
	rowCount := entry.RowCount
	durationMS := int(entry.Duration.Milliseconds())

	_, err := l.store.Append(ctx, Entry{
		TenantID:     identity.TenantID,
		UserID:       userID,
		Source:       l.source,
		EventType:    EventQuery,
		QueryText:    &queryText,
		RowCount:     &rowCount,
		DurationMS:   &durationMS,
		Status:       status,
		ErrorMessage: errMsg,
	})
	return err
}

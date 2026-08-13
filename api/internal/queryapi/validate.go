package queryapi

import (
	"errors"
	"regexp"
	"strings"
)

// disallowedKeyword is defense-in-depth on top of the SELECT-only gate: it
// catches mutating/administrative statements appearing anywhere in the
// query (e.g. smuggled into a subquery), not just at the start. This is
// word-boundary matching, not a real SQL parser.
var disallowedKeyword = regexp.MustCompile(`(?i)\b(insert|update|delete|alter|drop|truncate|create|grant|revoke|attach|detach|rename|kill|optimize|system|set|exchange|watch)\b`)

// validateSelectOnly enforces the Phase 0 query API contract: exactly one
// SELECT statement and nothing else. This is "basic injection guarding" as
// specced, not a SQL parser: it will reject some unusual-but-valid SELECTs
// (e.g. one that references a column literally named "delete") and will
// not catch every possible abuse (e.g. a syntactically pure SELECT that's
// simply expensive to run). Both are acceptable for a Phase 0 placeholder
// that's explicitly superseded by a real query layer in Phase 2 — see
// /docs/architecture.md.
func validateSelectOnly(sql string) error {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return errors.New("query must not be empty")
	}

	trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, ";"))
	if trimmed == "" {
		return errors.New("query must not be empty")
	}
	if strings.Contains(trimmed, ";") {
		return errors.New("only a single statement is allowed")
	}

	firstWord := strings.ToUpper(strings.Fields(trimmed)[0])
	if firstWord != "SELECT" {
		return errors.New("only SELECT queries are allowed")
	}

	if disallowedKeyword.MatchString(trimmed) {
		return errors.New("query contains a disallowed keyword")
	}

	return nil
}

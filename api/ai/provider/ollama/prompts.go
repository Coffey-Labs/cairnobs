package ollama

import (
	"fmt"
	"strings"

	"github.com/cairnobs/cairnobs/api/ai/provider"
)

// grammarReference is a condensed version of
// /docs/query-language-reference.md -- every operation's system prompt
// includes this so the model is grounded in Cairn OBS's actual pipe syntax,
// not whatever generic log-query DSL it may have seen in training.
// Trimmed to the parts that matter for generation/explanation (the full
// doc's prose and examples aren't needed here); kept in sync with that
// doc by hand -- if the grammar changes, this needs updating too, same
// as any other place the language is described outside its own parser.
const grammarReference = `Cairn OBS query language (pipe syntax):

<base search> | <stage> | <stage> | ...

Base search: filter terms and/or free-text search, combined with implicit "and".
  field=value, field!=value, field>value, field>=value, field<value, field<=value
  bare word or "quoted phrase" -- free-text search on the message field
  message:"phrase" -- explicit free-text search
  earliest=-1h, latest=-5m -- relative time (s/m/h/d/w units), or earliest="2026-08-14T00:00:00Z" (RFC 3339 absolute)
  "or" only works between free-text terms, never between structured filters

Pipe stages, in the order they may appear:
  | where <filter terms>              additional filtering, same syntax as base search filters
  | stats <func>(<field>) as <alias>, ... by <field>, ...
        functions: count (no field needed), sum, avg, min, max (all need a field)
  | sort -field, +field, ...          "-" descending (default if no sign), "+" ascending
  | fields field, field, ...          choose output columns
  | head N                            first N results (default 100)
  | tail N                            last N results, chronologically

Structured columns: timestamp, host, service, severity, message, record_id.
Anything else is looked up in per-record attributes (always text; compared
numerically when the right-hand side looks like a number).

Raw SQL (SELECT ...) is also accepted but pipe syntax is strongly preferred
for anything generated rather than hand-written -- narrower, safer surface.`

func renderSchema(s provider.SchemaContext) string {
	if len(s.Services) == 0 && len(s.Fields) == 0 {
		return "(no schema grounding data available yet)"
	}
	var sb strings.Builder
	if len(s.Services) > 0 {
		fmt.Fprintf(&sb, "Known services: %s\n", strings.Join(s.Services, ", "))
	}
	if len(s.Fields) > 0 {
		sb.WriteString("Known fields:\n")
		for _, f := range s.Fields {
			if len(f.Examples) > 0 {
				fmt.Fprintf(&sb, "  - %s (examples: %s)\n", f.Name, strings.Join(f.Examples, ", "))
			} else {
				fmt.Fprintf(&sb, "  - %s\n", f.Name)
			}
		}
	}
	return sb.String()
}

func translateSystemPrompt(schema provider.SchemaContext) string {
	return fmt.Sprintf(`You translate a plain-English question into a Cairn OBS pipe-syntax query. You never explain, never execute anything, never write raw SQL unless the pipe syntax genuinely cannot express the request.

%s

%s

Respond with ONLY a JSON object, no other text, no markdown fences:
{"query": "<the pipe-syntax query>", "confidence": "high"|"medium"|"low", "reason": "<empty unless confidence is low, in which case explain what's ambiguous or unsupported>"}

If you cannot produce a query you're reasonably confident in, set confidence to "low", leave query empty, and explain why in reason. Never guess with false confidence.`, grammarReference, renderSchema(schema))
}

func completeSystemPrompt(schema provider.SchemaContext) string {
	return fmt.Sprintf(`You suggest how to continue a partially-typed Cairn OBS query. You are given everything typed so far; respond with ONLY the suggested continuation text (what should appear after the cursor), not the text already typed, not an explanation.

%s

%s

Respond with ONLY a JSON object, no other text, no markdown fences:
{"suggestion": "<continuation text, or empty string if you have no good suggestion>"}`, grammarReference, renderSchema(schema))
}

// explainSystemPrompt covers all three contexts provider.ExplainRequest
// supports: a plain hand-written-query explanation (both empty), a
// post-translation review (hasIntent), or Optimize's "phrase these
// findings" mode (hasFindings) -- mutually exclusive in practice, see
// ExplainRequest.RuleFindings' doc comment.
func explainSystemPrompt(hasIntent, hasFindings bool) string {
	if hasFindings {
		return fmt.Sprintf(`A rule-based check already found one or more real issues with a Cairn OBS query's efficiency (e.g. a missing time range). Your only job is to phrase those findings as a short, clear, actionable suggestion for the person who wrote the query -- do not invent additional issues, do not restate the query's own syntax back at them, do not hedge with "might" or "could" about something the check already confirmed. One or two sentences.

%s`, grammarReference)
	}

	base := fmt.Sprintf(`You explain what a Cairn OBS query does in plain English, for someone who may not know the query language. Be concise -- two or three sentences, not a line-by-line breakdown unless the query is unusually complex.

%s`, grammarReference)
	if hasIntent {
		base += "\n\nYou are explaining a query that was just generated from a natural-language request. Focus on how the request became this query -- call out any interpretation choices (e.g. how a vague time phrase or field reference was resolved), not just what the query does in isolation."
	}
	return base
}

func fixSystemPrompt(schema provider.SchemaContext) string {
	return fmt.Sprintf(`You fix a broken Cairn OBS query given its error message. Produce a corrected query and a short explanation of what was wrong.

%s

%s

Respond with ONLY a JSON object, no other text, no markdown fences:
{"suggested_query": "<corrected query>", "explanation": "<short explanation of what was wrong and what changed>", "confidence": "high"|"medium"|"low"}

If you cannot determine a fix, set confidence to "low" and suggested_query to an empty string.`, grammarReference, renderSchema(schema))
}

// Package normalize maps the wire-format LogRecord (as agents send it)
// into the ClickHouse row shape defined in /storage. This is the "OTel-log-
// like schema" normalization step called for in the ingest design — Phase
// 0 keeps it to the minimal column set; full OTel field mapping (separate
// SeverityNumber/SeverityText, resource attributes, etc.) is deferred, see
// the open questions in /docs/architecture.md.
package normalize

import (
	"time"

	logsv1 "github.com/sentry/sentry/proto/sentry/logs/v1"
)

type Row struct {
	Timestamp  time.Time
	Host       string
	Service    string
	Severity   string
	Message    string
	Attributes map[string]string
}

func ToRow(rec *logsv1.LogRecord) Row {
	attrs := rec.GetAttributes()
	if attrs == nil {
		attrs = map[string]string{}
	}
	return Row{
		Timestamp:  time.Unix(0, rec.GetTimestampUnixNano()).UTC(),
		Host:       rec.GetHost(),
		Service:    rec.GetService(),
		Severity:   severityText(rec.GetSeverity()),
		Message:    rec.GetMessage(),
		Attributes: attrs,
	}
}

// severityText maps the proto Severity enum to short OTel-style severity
// names, stored as the `severity` column's value.
func severityText(sev logsv1.Severity) string {
	switch sev {
	case logsv1.Severity_SEVERITY_TRACE:
		return "TRACE"
	case logsv1.Severity_SEVERITY_DEBUG:
		return "DEBUG"
	case logsv1.Severity_SEVERITY_INFO:
		return "INFO"
	case logsv1.Severity_SEVERITY_WARN:
		return "WARN"
	case logsv1.Severity_SEVERITY_ERROR:
		return "ERROR"
	case logsv1.Severity_SEVERITY_FATAL:
		return "FATAL"
	default:
		return "UNSPECIFIED"
	}
}

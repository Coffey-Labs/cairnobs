package normalize

import (
	"testing"
	"time"

	"github.com/google/uuid"

	logsv1 "github.com/cairnobs/cairnobs/proto/sentry/logs/v1"
)

func TestToRowMapsFieldsAndSeverity(t *testing.T) {
	id := uuid.New()
	rec := &logsv1.LogRecord{
		TimestampUnixNano: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano(),
		Host:               "host-1",
		Service:            "svc-a",
		Severity:           logsv1.Severity_SEVERITY_ERROR,
		Message:            "boom",
		Attributes:         map[string]string{"k": "v"},
		RecordId:           id.String(),
	}

	row := ToRow(rec)

	if row.Host != "host-1" || row.Service != "svc-a" || row.Message != "boom" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.Severity != "ERROR" {
		t.Fatalf("expected severity ERROR, got %s", row.Severity)
	}
	if row.Attributes["k"] != "v" {
		t.Fatalf("expected attribute k=v, got %+v", row.Attributes)
	}
	if !row.Timestamp.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("unexpected timestamp: %v", row.Timestamp)
	}
	if row.RecordID != id {
		t.Fatalf("RecordID = %v, want %v", row.RecordID, id)
	}
}

func TestToRowInvalidRecordIDFallsBackToNilUUID(t *testing.T) {
	rec := &logsv1.LogRecord{Host: "h", Service: "s", Message: "m", RecordId: "not-a-uuid"}
	row := ToRow(rec)
	if row.RecordID != uuid.Nil {
		t.Fatalf("expected nil UUID fallback for an invalid record_id, got %v", row.RecordID)
	}
}

func TestToRowNilAttributesBecomesEmptyMap(t *testing.T) {
	rec := &logsv1.LogRecord{Host: "h", Service: "s", Message: "m"}
	row := ToRow(rec)
	if row.Attributes == nil {
		t.Fatal("expected non-nil empty map, got nil")
	}
	if len(row.Attributes) != 0 {
		t.Fatalf("expected empty map, got %+v", row.Attributes)
	}
}

func TestSeverityTextCoversAllEnumValues(t *testing.T) {
	cases := map[logsv1.Severity]string{
		logsv1.Severity_SEVERITY_UNSPECIFIED: "UNSPECIFIED",
		logsv1.Severity_SEVERITY_TRACE:       "TRACE",
		logsv1.Severity_SEVERITY_DEBUG:       "DEBUG",
		logsv1.Severity_SEVERITY_INFO:        "INFO",
		logsv1.Severity_SEVERITY_WARN:        "WARN",
		logsv1.Severity_SEVERITY_ERROR:       "ERROR",
		logsv1.Severity_SEVERITY_FATAL:       "FATAL",
	}
	for sev, want := range cases {
		if got := severityText(sev); got != want {
			t.Errorf("severityText(%v) = %q, want %q", sev, got, want)
		}
	}
}

func TestSeverityTextUnknownValueFallsBackToUnspecified(t *testing.T) {
	if got := severityText(logsv1.Severity(99)); got != "UNSPECIFIED" {
		t.Fatalf("expected UNSPECIFIED for unknown severity, got %q", got)
	}
}

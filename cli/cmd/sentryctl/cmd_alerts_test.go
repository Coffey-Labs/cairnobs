package main

import (
	"bytes"
	"reflect"
	"testing"
)

func TestExtractAlertingAPIFlagDefault(t *testing.T) {
	alertingURL, rest := extractAlertingAPIFlag([]string{"rule-1"}, func(string) string { return "" })
	if alertingURL != defaultAlertingURL {
		t.Fatalf("alertingURL = %q, want default %q", alertingURL, defaultAlertingURL)
	}
	if !reflect.DeepEqual(rest, []string{"rule-1"}) {
		t.Fatalf("rest = %v", rest)
	}
}

func TestExtractAlertingAPIFlagOverride(t *testing.T) {
	alertingURL, rest := extractAlertingAPIFlag([]string{"--alerting-api", "http://custom:9091", "rule-1"}, func(string) string { return "" })
	if alertingURL != "http://custom:9091" {
		t.Fatalf("alertingURL = %q", alertingURL)
	}
	if !reflect.DeepEqual(rest, []string{"rule-1"}) {
		t.Fatalf("rest = %v", rest)
	}
}

func TestExtractAlertingAPIFlagFromEnv(t *testing.T) {
	alertingURL, _ := extractAlertingAPIFlag(nil, func(k string) string {
		if k == "SENTRYCTL_ALERTING_API_URL" {
			return "http://env-alerting:8081"
		}
		return ""
	})
	if alertingURL != "http://env-alerting:8081" {
		t.Fatalf("alertingURL = %q", alertingURL)
	}
}

func TestCmdAlertsMissingSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAlerts(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdAlertsApplyMissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAlerts([]string{"apply"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdAlertsUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdAlerts([]string{"bogus"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

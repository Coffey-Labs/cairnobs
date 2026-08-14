package main

import (
	"bytes"
	"reflect"
	"testing"
)

func TestExtractAPIFlagDefault(t *testing.T) {
	apiURL, rest := extractAPIFlag([]string{"abc123"}, func(string) string { return "" })
	if apiURL != defaultAPIURL {
		t.Fatalf("apiURL = %q, want default %q", apiURL, defaultAPIURL)
	}
	if !reflect.DeepEqual(rest, []string{"abc123"}) {
		t.Fatalf("rest = %v", rest)
	}
}

func TestExtractAPIFlagOverride(t *testing.T) {
	apiURL, rest := extractAPIFlag([]string{"--api", "http://custom:9090", "abc123"}, func(string) string { return "" })
	if apiURL != "http://custom:9090" {
		t.Fatalf("apiURL = %q", apiURL)
	}
	if !reflect.DeepEqual(rest, []string{"abc123"}) {
		t.Fatalf("rest = %v, want [abc123] (flag pair stripped)", rest)
	}
}

func TestCmdDashboardsMissingSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestCmdDashboardsGetMissingID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdDashboards([]string{"get"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

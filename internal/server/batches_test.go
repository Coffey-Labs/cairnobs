package server

import (
	"strings"
	"testing"
)

func batched(key, sev, item string) Condition {
	return Condition{Key: key, Severity: sev, Title: key + " ends soon", Href: "/" + key,
		Batch: &Batch{Key: "certends/vault-prod/pki_int", Title: "{n} certificates from pki_int end soon", Item: item, Href: "/workloads"}}
}

// Three or more alike are one condition, named once each; fewer stay as
// they are; one that has paged on its own stays on its own (#33).
func TestFoldBatches(t *testing.T) {
	none := func(string) bool { return false }
	other := Condition{Key: "host/node1", Severity: "critical", Title: "node1 is down"}
	conds := []Condition{
		batched("c", "warning", "c (6 days)"), other,
		batched("a", "warning", "a (5 days)"),
		batched("b", "critical", "b (30 hours)"),
	}
	out := foldBatches(conds, none)
	if len(out) != 2 || out[0].Key != "batch/certends/vault-prod/pki_int" || out[1].Key != "host/node1" {
		t.Fatalf("%+v", out)
	}
	b := out[0]
	if b.Severity != "critical" || b.Title != "3 certificates from pki_int end soon" || b.Detail != "b (30 hours), a (5 days), c (6 days)." || b.Href != "/workloads" {
		t.Errorf("batch: %+v", b)
	}

	if out := foldBatches(conds[:3], none); len(out) != 3 {
		t.Errorf("two alike aren't a batch: %+v", out)
	}

	// b paged on its own before the others came: it stays, and the rest
	// are too few.
	paged := func(k string) bool { return k == "b" }
	if out := foldBatches(conds, paged); len(out) != 4 {
		t.Errorf("a paged member stays on its own: %+v", out)
	}

	// A batch that paged keeps its incident while one member is left.
	batchPaged := func(k string) bool { return k == "batch/certends/vault-prod/pki_int" }
	out = foldBatches([]Condition{batched("a", "warning", "a (5 days)")}, batchPaged)
	if len(out) != 1 || out[0].Key != "batch/certends/vault-prod/pki_int" || out[0].Title != "a ends soon" {
		t.Errorf("what's left of a paged batch: %+v", out)
	}

	// More than batchListed members: the rest are counted.
	var many []Condition
	for i := range 11 {
		many = append(many, batched(string(rune('a'+i)), "warning", string(rune('a'+i))))
	}
	if out := foldBatches(many, none); len(out) != 1 || !strings.HasSuffix(out[0].Detail, "h and 3 more.") {
		t.Errorf("many: %+v", out)
	}
}

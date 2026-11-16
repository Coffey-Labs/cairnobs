package server

import (
	"cmp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/cairnobs/cairnobs/internal/notify"
)

// Batches (#33): several problems of one kind from one place at once (the
// certificates of one Vault mount ending in the same week, a vCenter's
// VMs all failing the same way) are one incident that lists them, not one
// page each. Root-cause grouping (grouping.go) folds what depends on a
// failure; this folds what's alike.
//
// The safety rules are grouping's: a member that has already paged on
// its own stays on its own, so it neither vanishes nor reads as resolved;
// the batch takes its worst member's severity, so one that gets worse
// escalates it; and a batch that has paged keeps its incident while any
// member is left, rather than resolving and opening again as one alert.

// Batch says which batch a condition belongs to.
type Batch struct {
	Key   string // what's alike and where: "certends/vault-prod/pki_int"
	Title string // the batch's title, {n} for how many: "{n} certificates from pki_int in vault-prod end soon"
	Item  string // this one in the batch's list: "svc-102.prod.internal (ends in 5 days)"
	Href  string // where the batch's link goes (the first member's link without it)
}

// batchMin is how many alike make a batch.
const batchMin = 3

// batchListed is how many members a batch's detail names.
const batchListed = 8

// foldBatches replaces each batch's members with one condition, keyed
// batch/<key>. firing says whether an incident has paged.
func foldBatches(conds []Condition, firing func(key string) bool) []Condition {
	members := map[string][]int{}
	for i, c := range conds {
		if c.Batch != nil && !firing(c.Key) {
			members[c.Batch.Key] = append(members[c.Batch.Key], i)
		}
	}
	if len(members) == 0 {
		return conds
	}
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	folded := make([]bool, len(conds))
	batch := map[int]Condition{} // by its first member's place in conds, so it takes that place
	for _, k := range keys {
		idx := members[k]
		if len(idx) < batchMin && !firing("batch/"+k) {
			continue
		}
		at := slices.Min(idx) // where the batch goes in the list
		// Worst first, then by key: the same order from one evaluation to
		// the next, so the detail doesn't shuffle.
		sort.SliceStable(idx, func(a, b int) bool {
			ca, cb := conds[idx[a]], conds[idx[b]]
			if ra, rb := notify.Rank(ca.Severity), notify.Rank(cb.Severity); ra != rb {
				return ra > rb
			}
			return ca.Key < cb.Key
		})
		first := conds[idx[0]]
		b := Condition{Key: "batch/" + k, Severity: first.Severity, Href: cmp.Or(first.Batch.Href, first.Href), Map: first.Map, Host: first.Host, Ends: first.Ends,
			Title: strings.ReplaceAll(first.Batch.Title, "{n}", strconv.Itoa(len(idx)))}
		var items []string
		for _, i := range idx {
			c := conds[i]
			folded[i] = true
			if c.Host != b.Host {
				b.Host = "" // a maintenance window holds it only when it's all one host's
			}
			if !c.Ends.IsZero() && (b.Ends.IsZero() || c.Ends.Before(b.Ends)) {
				b.Ends = c.Ends
			}
			if len(items) < batchListed {
				items = append(items, c.Batch.Item)
			}
		}
		b.Detail = strings.Join(items, ", ")
		if more := len(idx) - len(items); more > 0 {
			b.Detail += " and " + strconv.Itoa(more) + " more"
		}
		b.Detail += "."
		if len(idx) == 1 { // what's left of a batch that paged: still its incident
			b.Title, b.Detail = first.Title, first.Detail
		}
		batch[at] = b
	}
	out := make([]Condition, 0, len(conds))
	for i, c := range conds {
		if b, ok := batch[i]; ok {
			out = append(out, b)
		}
		if !folded[i] {
			out = append(out, c)
		}
	}
	return out
}

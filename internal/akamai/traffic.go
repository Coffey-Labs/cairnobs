package akamai

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"time"
)

// Traffic and Akamai's status (docs/akamai.md step 5): each CP code's edge
// hits and how many were answered 5xx, from the Reporting API v2's
// delivery/traffic/current report (v1's hits-by-cpcode was discontinued
// on 2026-04-01). Its data is only queried with a POST; with no time
// range it is the latest complete 5-minute window, about ten minutes
// behind. Field names from Akamai's Reporting API v2 reference.

// trafficPath is the report's data: the one POST ever sent.
const trafficPath = "/reporting-api/v2/reports/delivery/traffic/current/data"

// CPTraffic is one CP code's edge traffic in the window.
type CPTraffic struct {
	CPCode int64 `json:"cpcode"`
	Hits   int64 `json:"hits"`
	Errors int64 `json:"errors"` // answered 5xx
}

// Pct is the share of hits answered 5xx.
func (t CPTraffic) Pct() float64 {
	if t.Hits == 0 {
		return 0
	}
	return 100 * float64(t.Errors) / float64(t.Hits)
}

// trafficQuery is the report query: hits by CP code and response class.
var trafficQuery = []byte(`{"dimensions":["cpcode","responseClass"],"metrics":["edgeHitsSum"],"limit":50000}`)

type trafficReport struct {
	Metadata struct {
		Start       time.Time `json:"start"`
		End         time.Time `json:"end"`
		HasMoreRows bool      `json:"hasMoreRows"`
	} `json:"metadata"`
	Data []struct {
		CPCode        json.Number `json:"cpcode"`
		ResponseClass string      `json:"responseClass"`
		EdgeHitsSum   json.Number `json:"edgeHitsSum"`
	} `json:"data"`
}

func (w *Watcher) readTraffic(ctx context.Context, c Creds) ([]CPTraffic, time.Time, time.Time, error) {
	var r trafficReport
	if err := w.do(ctx, c, http.MethodPost, trafficPath, "application/json", trafficQuery, &r); err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	by := map[int64]*CPTraffic{}
	for _, row := range r.Data {
		cp, err := row.CPCode.Int64()
		if err != nil {
			continue
		}
		n, _ := row.EdgeHitsSum.Int64()
		t := by[cp]
		if t == nil {
			t = &CPTraffic{CPCode: cp}
			by[cp] = t
		}
		t.Hits += n
		if row.ResponseClass == "5xx" {
			t.Errors += n
		}
	}
	out := make([]CPTraffic, 0, len(by))
	for _, t := range by {
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b CPTraffic) int { // busiest first
		return cmp.Or(cmp.Compare(b.Hits, a.Hits), cmp.Compare(a.CPCode, b.CPCode))
	})
	return out, r.Metadata.Start, r.Metadata.End, nil
}

// Status is Akamai's status page summary (a Statuspage
// /api/v2/status.json): "none", "minor", "major", "critical", or
// "unknown" when it couldn't be read.
type Status struct {
	Indicator   string `json:"indicator"`
	Description string `json:"description"`
}

// status reads Akamai's status page; a stand-in's (a loopback .edgerc
// host) when that's what the client speaks to.
func (w *Watcher) status(ctx context.Context, c Creds) Status {
	u := w.cfg.StatusURL
	if c.Plain {
		u = "http://" + c.Host + "/status.json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Status{Indicator: "unknown", Description: err.Error()}
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return Status{Indicator: "unknown", Description: "status page unreachable"}
	}
	defer resp.Body.Close()
	var v struct {
		Status Status `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil || v.Status.Indicator == "" {
		return Status{Indicator: "unknown", Description: "status page unreadable"}
	}
	return v.Status
}

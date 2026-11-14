package akamai

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Property Manager (docs/akamai.md step 3): each property's versions on
// staging and production, and the newest activation on each network, so
// a failed activation, or one stuck pending, is seen. Field names from
// Akamai's Property Manager API (PAPI v1) reference. Activations' notify
// addresses are never taken.

// PMProperty is a Property Manager property (a delivery configuration;
// GTM's properties are Property).
type PMProperty struct {
	ID         string `json:"id"` // prp_…
	Name       string `json:"name"`
	Group      string `json:"group"` // its group's name
	Latest     int    `json:"latest"`
	Staging    int    `json:"staging"`    // the version active there; 0: none
	Production int    `json:"production"` // the version active there; 0: none
	// The newest activation on each network; nil: never activated there.
	LastStaging    *Activation `json:"last_staging,omitempty"`
	LastProduction *Activation `json:"last_production,omitempty"`
	Read           time.Time   `json:"read"` // when its activations were last read
	Error          string      `json:"error,omitempty"`

	contract, group string // for the activations' query
}

// Activation is one activation (or deactivation) of a property version.
type Activation struct {
	ID        string    `json:"id"`
	Version   int       `json:"version"`
	Type      string    `json:"type"`   // ACTIVATE, DEACTIVATE
	Status    string    `json:"status"` // ACTIVE, FAILED, PENDING, ZONE_1…3, ABORTED, INACTIVE, …
	Submitted time.Time `json:"submitted"`
	Updated   time.Time `json:"updated"`
	Note      string    `json:"note,omitempty"`
}

// Pending says whether the activation hasn't finished.
func (a *Activation) Pending() bool {
	switch a.Status {
	case "NEW", "PENDING", "ZONE_1", "ZONE_2", "ZONE_3", "PENDING_DEACTIVATION", "PENDING_CANCELLATION":
		return true
	}
	return false
}

type papiGroups struct {
	Groups struct {
		Items []struct {
			GroupID     string   `json:"groupId"`
			GroupName   string   `json:"groupName"`
			ContractIDs []string `json:"contractIds"`
		} `json:"items"`
	} `json:"groups"`
}

type papiProperties struct {
	Properties struct {
		Items []struct {
			PropertyID        string `json:"propertyId"`
			PropertyName      string `json:"propertyName"`
			ContractID        string `json:"contractId"`
			GroupID           string `json:"groupId"`
			LatestVersion     int    `json:"latestVersion"`
			StagingVersion    *int   `json:"stagingVersion"`
			ProductionVersion *int   `json:"productionVersion"`
		} `json:"items"`
	} `json:"properties"`
}

type papiActivations struct {
	Activations struct {
		Items []struct {
			ActivationID    string    `json:"activationId"`
			PropertyVersion int       `json:"propertyVersion"`
			Network         string    `json:"network"`
			ActivationType  string    `json:"activationType"`
			Status          string    `json:"status"`
			SubmitDate      time.Time `json:"submitDate"`
			UpdateDate      time.Time `json:"updateDate"`
			Note            string    `json:"note"`
		} `json:"items"`
	} `json:"activations"`
}

// activationsRefresh is how long a settled property's activations are
// trusted before they're read again: a property whose versions haven't
// moved, and whose newest activations had finished well, has nothing new
// to say, and an enterprise has hundreds (docs/akamai.md, Scale).
const activationsRefresh = 6 * time.Hour

// propertiesAtOnce is how many properties' activations are read at a time.
const propertiesAtOnce = 4

// readPAPI reads every property the client can see, in each group's
// contracts. Activations are read for a property when its versions moved,
// its newest activation hadn't settled well, or activationsRefresh passed.
func (w *Watcher) readPAPI(ctx context.Context, c Creds, now time.Time, prev []PMProperty) ([]PMProperty, error) {
	var gs papiGroups
	if err := w.get(ctx, c, "/papi/v1/groups", &gs); err != nil {
		return nil, err
	}
	was := map[string]PMProperty{}
	for _, p := range prev {
		was[p.ID] = p
	}
	var out []PMProperty
	seen := map[string]bool{}
	for _, g := range gs.Groups.Items {
		for _, ct := range g.ContractIDs {
			var ps papiProperties
			q := url.Values{"contractId": {ct}, "groupId": {g.GroupID}}
			if err := w.get(ctx, c, "/papi/v1/properties?"+q.Encode(), &ps); err != nil {
				return nil, err
			}
			for _, it := range ps.Properties.Items {
				if seen[it.PropertyID] {
					continue
				}
				seen[it.PropertyID] = true
				p := PMProperty{ID: it.PropertyID, Name: it.PropertyName, Group: g.GroupName, Latest: it.LatestVersion, contract: ct, group: g.GroupID}
				if it.StagingVersion != nil {
					p.Staging = *it.StagingVersion
				}
				if it.ProductionVersion != nil {
					p.Production = *it.ProductionVersion
				}
				out = append(out, p)
			}
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, propertiesAtOnce)
	for i := range out {
		p := &out[i]
		if o, ok := was[p.ID]; ok && settled(o, *p, now) {
			p.LastStaging, p.LastProduction, p.Read = o.LastStaging, o.LastProduction, o.Read
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			w.activations(ctx, c, now, p)
		}()
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b PMProperty) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// settled says whether a property's last reading of its activations still
// stands: read without error, within activationsRefresh, its versions
// unchanged, and its newest activations finished and not failed.
func settled(o, p PMProperty, now time.Time) bool {
	if o.Error != "" || o.Read.IsZero() || now.Sub(o.Read) >= activationsRefresh ||
		o.Latest != p.Latest || o.Staging != p.Staging || o.Production != p.Production {
		return false
	}
	for _, a := range []*Activation{o.LastStaging, o.LastProduction} {
		if a != nil && (a.Pending() || a.Status == "FAILED") {
			return false
		}
	}
	return true
}

// activations reads a property's activations and keeps the newest on
// each network.
func (w *Watcher) activations(ctx context.Context, c Creds, now time.Time, p *PMProperty) {
	var as papiActivations
	q := url.Values{"contractId": {p.contract}, "groupId": {p.group}}
	if err := w.get(ctx, c, "/papi/v1/properties/"+url.PathEscape(p.ID)+"/activations?"+q.Encode(), &as); err != nil {
		p.Error = err.Error()
		return
	}
	for _, it := range as.Activations.Items {
		a := &Activation{ID: it.ActivationID, Version: it.PropertyVersion, Type: it.ActivationType, Status: it.Status,
			Submitted: it.SubmitDate, Updated: it.UpdateDate, Note: it.Note}
		last := &p.LastStaging
		if it.Network == "PRODUCTION" {
			last = &p.LastProduction
		}
		if *last == nil || a.Submitted.After((*last).Submitted) {
			*last = a
		}
	}
	p.Read = now
}

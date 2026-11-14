package akamai

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/cairnobs/cairnobs/internal/certinfo"
	"github.com/cairnobs/cairnobs/internal/report"
)

// CPS (Certificate Provisioning System, docs/akamai.md step 2): each
// enrollment's certificates as deployed on the production network, and
// whether a change is waiting on someone. Field names from Akamai's CPS
// API reference; its media types are versioned and must match, so each
// call names the versions it understands, newest first.

// Enrollment is one CPS enrollment.
type Enrollment struct {
	ID         int           `json:"id"`
	CN         string        `json:"cn"`
	SANs       []string      `json:"sans,omitempty"`
	Validation string        `json:"validation,omitempty"` // dv, ov, ev, third-party
	Type       string        `json:"type,omitempty"`       // san, single, wildcard, …
	Pending    int           `json:"pending"`              // changes in progress
	Waiting    string        `json:"waiting,omitempty"`    // what a change needs from someone: "third-party-csr", "change-management-info"
	Deployed   []report.Cert `json:"deployed"`             // on production: RSA and ECDSA when both
	Error      string        `json:"error,omitempty"`
}

var (
	enrollmentsTypes = []string{"application/vnd.akamai.cps.enrollments.v11+json", "application/vnd.akamai.cps.enrollments.v10+json"}
	deploymentTypes  = []string{"application/vnd.akamai.cps.deployment.v8+json", "application/vnd.akamai.cps.deployment.v7+json"}
	changeTypes      = []string{"application/vnd.akamai.cps.change.v2+json"}
)

type cpsEnrollments struct {
	Enrollments []struct {
		ID  int `json:"id"`
		CSR struct {
			CN   string   `json:"cn"`
			SANs []string `json:"sans"`
		} `json:"csr"`
		ValidationType  string          `json:"validationType"`
		CertificateType string          `json:"certificateType"`
		PendingChanges  json.RawMessage `json:"pendingChanges"` // objects ({location, changeType}) or, in older versions, strings
	} `json:"enrollments"`
}

type cpsCert struct {
	Certificate string `json:"certificate"`
}

type cpsDeployment struct {
	PrimaryCertificate       *cpsCert  `json:"primaryCertificate"`
	MultiStackedCertificates []cpsCert `json:"multiStackedCertificates"`
}

type cpsChange struct {
	AllowedInput []struct {
		Type              string `json:"type"`
		RequiredToProceed bool   `json:"requiredToProceed"`
	} `json:"allowedInput"`
}

type papiContracts struct {
	Contracts struct {
		Items []struct {
			ContractID string `json:"contractId"`
		} `json:"items"`
	} `json:"contracts"`
}

// enrollmentsAtOnce is how many enrollments' deployments are read at a time.
const enrollmentsAtOnce = 4

// readCPS reads every enrollment the client can see: in each contract
// configured, else each Property Manager lists, else without one.
func (w *Watcher) readCPS(ctx context.Context, c Creds) ([]Enrollment, error) {
	contracts := w.cfg.Contracts
	if len(contracts) == 0 {
		var pc papiContracts
		if err := w.get(ctx, c, "/papi/v1/contracts", &pc); err == nil {
			for _, it := range pc.Contracts.Items {
				contracts = append(contracts, strings.TrimPrefix(it.ContractID, "ctr_"))
			}
		}
	}
	if len(contracts) == 0 {
		contracts = []string{""}
	}
	var out []Enrollment
	seen := map[int]bool{}
	for _, ct := range contracts {
		path := "/cps/v2/enrollments"
		if ct != "" {
			path += "?contractId=" + strings.TrimPrefix(ct, "ctr_")
		}
		var es cpsEnrollments
		if err := w.getTyped(ctx, c, path, enrollmentsTypes, &es); err != nil {
			return nil, err
		}
		for _, e := range es.Enrollments {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			en := Enrollment{ID: e.ID, CN: e.CSR.CN, SANs: e.CSR.SANs, Validation: e.ValidationType, Type: e.CertificateType, Deployed: []report.Cert{}}
			var changes []json.RawMessage
			_ = json.Unmarshal(e.PendingChanges, &changes)
			en.Pending = len(changes)
			if len(changes) > 0 {
				en.Waiting = w.waiting(ctx, c, changes[0])
			}
			out = append(out, en)
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, enrollmentsAtOnce)
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var d cpsDeployment
			e := &out[i]
			switch err := w.getTyped(ctx, c, "/cps/v2/enrollments/"+strconv.Itoa(e.ID)+"/deployments/production", deploymentTypes, &d); {
			case errors.Is(err, errAbsent): // not deployed yet
			case err != nil:
				e.Error = err.Error()
			default:
				var pems []string
				if d.PrimaryCertificate != nil {
					pems = append(pems, d.PrimaryCertificate.Certificate)
				}
				for _, m := range d.MultiStackedCertificates {
					pems = append(pems, m.Certificate)
				}
				for _, p := range pems {
					if b, _ := pem.Decode([]byte(p)); b != nil {
						if x, err := x509.ParseCertificate(b.Bytes); err == nil {
							e.Deployed = append(e.Deployed, *certinfo.Of(x))
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b Enrollment) int { return strings.Compare(a.CN, b.CN) })
	return out, nil
}

// waiting says what a pending change needs from someone, or "": its
// location is "/cps/v2/enrollments/{id}/changes/{changeId}".
func (w *Watcher) waiting(ctx context.Context, c Creds, raw json.RawMessage) string {
	var loc string
	var obj struct {
		Location string `json:"location"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Location != "" {
		loc = obj.Location
	} else {
		_ = json.Unmarshal(raw, &loc)
	}
	if !strings.HasPrefix(loc, "/cps/v2/enrollments/") {
		return ""
	}
	var ch cpsChange
	if err := w.getTyped(ctx, c, loc, changeTypes, &ch); err != nil {
		return ""
	}
	for _, in := range ch.AllowedInput {
		if in.RequiredToProceed {
			return in.Type
		}
	}
	return ""
}

// getTyped is get with a versioned media type: the first the API accepts.
func (w *Watcher) getTyped(ctx context.Context, c Creds, path string, types []string, v any) error {
	var err error
	for _, t := range types {
		if err = w.getAccept(ctx, c, path, t, v); !errors.Is(err, errNotAcceptable) {
			return err
		}
	}
	return fmt.Errorf("%s: none of %s accepted", path, strings.Join(types, ", "))
}

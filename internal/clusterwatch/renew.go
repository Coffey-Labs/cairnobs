package clusterwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/cairnobs/cairnobs/internal/kube"
)

// Renew asks cert-manager to renew a Certificate now, as `cmctl renew`
// does: it sets the Certificate's Issuing condition, and cert-manager
// issues a new one (docs/cert-rotation.md, step 3). Nothing else is
// changed. It needs patch on certificates/status, which only the optional
// cert-renew.yaml grants.
func Renew(ctx context.Context, kc *kube.Client, namespace, name string, now time.Time) error {
	if !dnsLabel.MatchString(namespace) || !dnsName.MatchString(name) {
		return errors.New("not a Certificate's namespace and name")
	}
	path := fmt.Sprintf("/apis/cert-manager.io/v1/namespaces/%s/certificates/%s", namespace, name)
	var c struct {
		Metadata struct {
			Generation      int64  `json:"generation"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Status struct {
			// Kept as they are, but for Issuing: a merge patch replaces
			// the whole list.
			Conditions []map[string]any `json:"conditions"`
		} `json:"status"`
	}
	if err := kc.Get(ctx, path, &c); err != nil {
		switch kube.Code(err) {
		case http.StatusNotFound:
			return fmt.Errorf("no Certificate %s/%s (or cert-manager isn't installed)", namespace, name)
		case http.StatusForbidden:
			return errors.New("the cluster agent may not read cert-manager's Certificates: apply the current cluster.yaml")
		}
		return err
	}
	conds := c.Status.Conditions[:0:0]
	for _, cd := range c.Status.Conditions {
		if cd["type"] == "Issuing" {
			if cd["status"] == "True" {
				return fmt.Errorf("cert-manager is already renewing %s/%s", namespace, name)
			}
			continue
		}
		conds = append(conds, cd)
	}
	conds = append(conds, map[string]any{
		"type": "Issuing", "status": "True", "reason": "ManuallyTriggered",
		"message":            "Certificate re-issuance manually triggered (Cairn OBS)",
		"lastTransitionTime": now.UTC().Format(time.RFC3339),
		"observedGeneration": c.Metadata.Generation,
	})
	// The resourceVersion makes it fail rather than overwrite a change
	// made since the read.
	patch := map[string]any{
		"metadata": map[string]any{"resourceVersion": c.Metadata.ResourceVersion},
		"status":   map[string]any{"conditions": conds},
	}
	if err := kc.MergePatch(ctx, path+"/status", patch); err != nil {
		switch kube.Code(err) {
		case http.StatusForbidden:
			return errors.New("the cluster agent may not patch certificates/status: apply cert-renew.yaml (docs/cert-rotation.md)")
		case http.StatusConflict:
			return fmt.Errorf("%s/%s changed while asking; try again", namespace, name)
		}
		return err
	}
	return nil
}

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsName  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// RenewArgs are a renew action's arguments.
type RenewArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ParseRenew reads them.
func ParseRenew(raw json.RawMessage) (RenewArgs, error) {
	var a RenewArgs
	if err := json.Unmarshal(raw, &a); err != nil || a.Namespace == "" || a.Name == "" {
		return a, errors.New("bad renew request")
	}
	return a, nil
}

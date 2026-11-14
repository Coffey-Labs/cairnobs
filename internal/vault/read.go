package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Health is sys/health: what the server that answered says about itself
// and its cluster.
type Health struct {
	Initialized bool   `json:"initialized"`
	Sealed      bool   `json:"sealed"`
	Standby     bool   `json:"standby"`
	PerfStandby bool   `json:"performance_standby"`
	ReplPerf    string `json:"replication_performance_mode"`
	ReplDR      string `json:"replication_dr_mode"`
	Version     string `json:"version"`
	ClusterName string `json:"cluster_name"`
	ClusterID   string `json:"cluster_id"`
	Enterprise  bool   `json:"enterprise"`
}

// healthCodes are sys/health's answers that aren't errors: standby,
// a DR secondary, a performance standby, not initialized, sealed.
var healthCodes = []int{429, 472, 473, 501, 503}

// Seal is sys/seal-status, read when sealed: how far unsealing has got.
type Seal struct {
	Type     string `json:"type"` // shamir, awskms, transit…
	T        int    `json:"t"`    // shares needed
	Progress int    `json:"progress"`
}

// Leader is sys/leader.
type Leader struct {
	HAEnabled     bool   `json:"ha_enabled"`
	IsSelf        bool   `json:"is_self"`
	LeaderAddress string `json:"leader_address"`
}

// Server is one Raft server in autopilot's state.
type Server struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	NodeStatus  string `json:"node_status"` // alive, left, failed
	Healthy     bool   `json:"healthy"`
	Status      string `json:"status"` // leader, voter, non-voter
	LastContact string `json:"last_contact"`
	Version     string `json:"version"`
	StableSince string `json:"stable_since"`
}

// Autopilot is sys/storage/raft/autopilot/state.
type Autopilot struct {
	Healthy          bool              `json:"healthy"`
	FailureTolerance int               `json:"failure_tolerance"`
	Leader           string            `json:"leader"`
	Voters           []string          `json:"voters"`
	Servers          map[string]Server `json:"servers"`
}

// Token is what auth/token/lookup-self says about the monitoring token.
// Its id (the token itself) isn't kept.
type Token struct {
	DisplayName string   `json:"display_name"`
	Policies    []string `json:"policies"`
	TTL         int64    `json:"ttl"`          // seconds left; 0: it never ends
	CreationTTL int64    `json:"creation_ttl"` // seconds it was made for
	ExpireTime  *string  `json:"expire_time"`
	Renewable   bool     `json:"renewable"`
	Type        string   `json:"type"`
}

// Snapshot is one read of a Vault cluster.
type Snapshot struct {
	Name        string
	Health      Health
	HealthCode  int
	Seal        *Seal
	Leader      *Leader
	Raft        *Autopilot // nil when not Raft, or not readable: RaftErr says which
	RaftErr     string
	Token       *Token
	TokenErr    string
	Renewed     bool   // the token renewed itself this read
	RenewErr    string // why it couldn't
	Problems    map[string]string
	PKI         []Mount                 // every PKI mount, with its CAs and leaf certificates
	Metrics     *Metrics                // nil when sys/metrics couldn't be read (Problems says why)
	AuditFailed time.Time               // when audit failures last went up
	AuditNew    float64                 // failures since then, while auditHold lasts
	Repl        map[string]*Replication // "dr", "performance": the kinds that are on (Enterprise)
	License     *License                // Enterprise's
	MinLife     time.Duration           // the leaf certificates judged are meant to last at least this
	At          time.Time
}

// unwrap is a response's data: Vault wraps most answers in "data", and
// some sys/ ones not.
func unwrap(raw json.RawMessage, v any) error {
	var w struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &w) == nil && len(w.Data) > 0 && string(w.Data) != "null" {
		return json.Unmarshal(w.Data, v)
	}
	return json.Unmarshal(raw, v)
}

// Read reads the cluster once. sys/health is what it's for: without it
// the read failed. A sealed cluster answers nothing else, so nothing
// else is asked.
func (c *Client) Read(ctx context.Context, now time.Time) (*Snapshot, error) {
	s := &Snapshot{Name: c.Name(), Problems: map[string]string{}, MinLife: c.cfg.MinLifetime, At: now}
	code, err := c.call(ctx, http.MethodGet, "sys/health", false, nil, &s.Health, healthCodes...)
	if err != nil {
		return nil, err
	}
	s.HealthCode = code
	if !s.Health.Initialized {
		return s, nil
	}
	if s.Health.Sealed {
		var seal Seal
		if _, err := c.call(ctx, http.MethodGet, "sys/seal-status", false, nil, &seal); err == nil {
			s.Seal = &seal
		}
		return s, nil
	}
	var l Leader
	if _, err := c.call(ctx, http.MethodGet, "sys/leader", false, nil, &l); err != nil {
		s.Problems["leader"] = err.Error()
	} else {
		s.Leader = &l
	}
	var raw json.RawMessage
	switch err := c.get(ctx, "sys/storage/raft/autopilot/state", &raw); {
	case err == nil:
		var a Autopilot
		if err := unwrap(raw, &a); err != nil {
			s.RaftErr = err.Error()
		} else if len(a.Servers) > 0 {
			s.Raft = &a
		}
	case notRaft(err):
		// Consul, a file or memory (dev mode): no Raft servers to show.
	default:
		s.RaftErr = err.Error()
	}
	c.lookupToken(ctx, s)
	c.readMetrics(ctx, s)
	c.readEnterprise(ctx, s)
	c.readPKI(ctx, s)
	return s, nil
}

// notRaft says whether autopilot's refusal means storage isn't Raft.
func notRaft(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	m := strings.ToLower(se.Msg)
	return se.Code == http.StatusNotFound || strings.Contains(m, "raft") && (strings.Contains(m, "not in use") || strings.Contains(m, "not configured")) ||
		strings.Contains(m, "unsupported path")
}

// lookupToken looks the monitoring token up, and renews it once half of
// its time is gone, when it can be (docs/vault.md, decision 2).
func (c *Client) lookupToken(ctx context.Context, s *Snapshot) {
	var raw json.RawMessage
	if err := c.get(ctx, "auth/token/lookup-self", &raw); err != nil {
		s.TokenErr = err.Error()
		return
	}
	var t Token
	if err := unwrap(raw, &t); err != nil {
		s.TokenErr = err.Error()
		return
	}
	// Renewed once half its time is gone, or with three weeks left, so a
	// long-lived token never comes near the warning.
	if t.Renewable && t.TTL > 0 && t.CreationTTL > 0 && (t.TTL < t.CreationTTL/2 || time.Duration(t.TTL)*time.Second < warnBefore+24*time.Hour) &&
		s.At.Sub(c.renewed) >= renewGap(t) {
		c.renewed = s.At
		var r struct {
			Auth struct {
				LeaseDuration int64 `json:"lease_duration"`
			} `json:"auth"`
		}
		if _, err := c.call(ctx, http.MethodPost, "auth/token/renew-self", true, map[string]any{}, &r); err != nil {
			s.RenewErr = err.Error()
		} else if r.Auth.LeaseDuration > 0 {
			s.Renewed, t.TTL = true, r.Auth.LeaseDuration
			t.ExpireTime = nil
		}
	}
	s.Token = &t
}

// renewGap is the least time between renewals: a token near its max TTL
// comes back short every time, and isn't asked again each minute.
func renewGap(t Token) time.Duration {
	return min(time.Hour, time.Duration(t.CreationTTL)*time.Second/4)
}

// TokenEnds is when the token ends; zero when it never does.
func (s *Snapshot) TokenEnds() time.Time {
	if s.Token == nil || s.Token.TTL <= 0 {
		return time.Time{}
	}
	return s.At.Add(time.Duration(s.Token.TTL) * time.Second)
}

// Product is "Vault" or "OpenBao", from the version: OpenBao's are 2.x
// and later, Vault's 1.x; Vault Enterprise's end in +ent.
func (h Health) Product() string {
	if v := strings.TrimPrefix(h.Version, "v"); v != "" && !strings.HasPrefix(v, "1.") && !strings.HasPrefix(v, "0.") {
		return "OpenBao"
	}
	if h.Enterprise || strings.Contains(h.Version, "+ent") {
		return "Vault Enterprise"
	}
	return "Vault"
}

func (s *Snapshot) String() string {
	return fmt.Sprintf("%s: %s %s, code %d", s.Name, s.Health.Product(), s.Health.Version, s.HealthCode)
}

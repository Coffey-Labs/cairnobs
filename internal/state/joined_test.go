package state

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestJoinTokens(t *testing.T) {
	db, _ := open(t)
	now := time.Unix(1_800_000_000, 0)
	none := func(string) bool { return false }
	add := func(hash, agent string) {
		t.Helper()
		if err := db.AddJoinToken(JoinToken{TokenSHA256: hash, Agent: agent, Created: now, CreatedBy: "admin", Expires: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	add("aaaaaaaaaaaa1", "web1")
	if _, err := db.UseJoinToken("aaaaaaaaaaaa1", now.Add(time.Hour), none); !errors.Is(err, ErrJoinToken) {
		t.Fatalf("an expired token: %v", err)
	}
	add("bbbbbbbbbbbb2", "web1")
	a, err := db.UseJoinToken("bbbbbbbbbbbb2", now.Add(time.Minute), none)
	if err != nil || a.Name != "web1" || a.AddedBy != "admin" {
		t.Fatalf("use: %+v %v", a, err)
	}
	if _, err := db.UseJoinToken("bbbbbbbbbbbb2", now.Add(time.Minute), none); !errors.Is(err, ErrJoinToken) {
		t.Fatalf("used twice: %v", err)
	}
	// A second token for a name that joined meanwhile is spent and refused.
	add("cccccccccccc3", "web1")
	if _, err := db.UseJoinToken("cccccccccccc3", now.Add(time.Minute), none); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("joined a name twice: %v", err)
	}
	add("dddddddddddd4", "host1")
	if _, err := db.UseJoinToken("dddddddddddd4", now.Add(time.Minute), func(n string) bool { return n == "host1" }); err == nil {
		t.Fatal("joined a name the config file has")
	}
	add("eeeeeeeeeeee5", "web2")
	p, err := db.PendingJoinTokens(now.Add(time.Minute))
	if err != nil || len(p) != 2 || p[0].Agent != "web1" || p[1].Agent != "web2" { // aaaa…1 is still good at this time
		t.Fatalf("pending: %+v %v", p, err)
	}
	if ok, _ := db.CancelJoinToken("eeee"); ok {
		t.Fatal("cancelled by a too-short id")
	}
	if ok, err := db.CancelJoinToken("eeeeeeeeeeee"); !ok || err != nil {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	if _, err := db.UseJoinToken("eeeeeeeeeeee5", now.Add(time.Minute), none); !errors.Is(err, ErrJoinToken) {
		t.Fatalf("a cancelled token: %v", err)
	}
	// Tokens that ran out over a day ago are forgotten.
	if _, err := db.PendingJoinTokens(now.Add(26 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.db.QueryRow(`SELECT count(*) FROM join_tokens`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d old tokens kept", n)
	}

	if err := db.IssueAgentCert(AgentCert{Serial: "01", Agent: "web1", Issued: now, NotAfter: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.RemoveJoinedAgent("web1", now); !ok || err != nil {
		t.Fatalf("remove: %v %v", ok, err)
	}
	if live, _ := db.AgentCertLive("01", "web1"); live {
		t.Fatal("a removed agent's certificate is live")
	}
	if list, _ := db.JoinedAgents(); len(list) != 0 {
		t.Fatalf("still joined: %+v", list)
	}
	if ok, _ := db.RemoveJoinedAgent("web1", now); ok {
		t.Fatal("removed twice")
	}
}

// A cluster's token makes a cluster agent, on both back ends.
func TestJoinTokenForACluster(t *testing.T) {
	db, _ := open(t)
	now := time.Unix(1_800_000_000, 0)
	if err := db.AddJoinToken(JoinToken{TokenSHA256: "ffffffffffff6", Agent: "prod", Cluster: true, Created: now, CreatedBy: "admin", Expires: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if p, err := db.PendingJoinTokens(now); err != nil || len(p) != 1 || !p[0].Cluster {
		t.Fatalf("pending: %+v %v", p, err)
	}
	a, err := db.UseJoinToken("ffffffffffff6", now, func(string) bool { return false })
	if err != nil || !a.Cluster {
		t.Fatalf("use: %+v %v", a, err)
	}
	list, err := db.JoinedAgents()
	if err != nil || len(list) != 1 || !list[0].Cluster || list[0].Workstation {
		t.Fatalf("joined: %+v %v", list, err)
	}
}

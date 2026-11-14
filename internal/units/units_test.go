package units

import (
	"context"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	out := []byte("Id=patroni.service\nLoadState=loaded\nActiveState=active\nSubState=running\nNRestarts=2\nActiveEnterTimestamp=@1759372323\n\n" +
		"Id=nosuch.service\nLoadState=not-found\nActiveState=inactive\nSubState=dead\nNRestarts=0\nActiveEnterTimestamp=\n")
	u := parse(out)
	if len(u) != 2 || u[0].Name != "patroni.service" || u[0].Restarts != 2 || u[0].Since.Unix() != 1759372323 || u[1].Load != "not-found" || !u[1].Since.IsZero() {
		t.Fatalf("%+v", u)
	}
	if Name("inbuxa") != "inbuxa.service" || Name("backup.timer") != "backup.timer" {
		t.Fatal("names")
	}
}

func TestReadLive(t *testing.T) {
	u, err := Read(context.Background(), []string{"beacon-test-no-such-unit"})
	if err != nil {
		t.Skip("no systemd here:", err)
	}
	if len(u) != 1 || u[0].Load != "not-found" {
		t.Fatalf("%+v", u)
	}
}

func TestParseRunning(t *testing.T) {
	out := "cron.service loaded active running Regular background program processing daemon\n" +
		"docker.service loaded active running Docker Application Container Engine\n" +
		"user@1000.service loaded active running User Manager for UID 1000\n" +
		"session-3.scope loaded active running Session 3 of User john\n\n"
	got := parseRunning([]byte(out))
	if strings.Join(got, ",") != "cron.service,docker.service" {
		t.Fatalf("got %v", got)
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/cairnobs/cairnobs/internal/report"
)

func TestNameContainers(t *testing.T) {
	id := "3321fbc7d7a6" + strings.Repeat("0", 52)
	m := &report.Map{
		Listen: []report.Listener{{Owner: "container:3321fbc7d7a6", Proto: "tcp", Addr: "*", Port: 5432}, {Owner: "container:ffffffffffff", Proto: "tcp", Addr: "*", Port: 80}},
		Conns:  []report.Conn{{Owner: "container:3321fbc7d7a6", Dir: "in", Remote: "10.0.0.3", Port: 5432, Count: 2}, {Owner: "unit:x.service", Dir: "out", Remote: "10.0.0.4", Port: 5432, Count: 1}},
	}
	nameContainers(m, []report.Container{{ID: id, Name: "db"}})
	if m.Listen[0].Owner != "container:db" || m.Conns[0].Owner != "container:db" {
		t.Errorf("not renamed: %+v", m)
	}
	// One it doesn't know (a Kubernetes container on this node) keeps its ID.
	if m.Listen[1].Owner != "container:ffffffffffff" || m.Conns[1].Owner != "unit:x.service" {
		t.Errorf("renamed what it shouldn't: %+v", m)
	}
}

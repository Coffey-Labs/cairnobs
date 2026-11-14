package mapgraph

import (
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A cluster's controllers and VMs are parts, grouped by their labels; its
// Ingresses make one way in with declared edges; a host reaching a
// Service's address needs what's behind it.
func TestKubernetesOnTheServiceMap(t *testing.T) {
	ctrl := func(ns, name string, labels map[string]string) report.Workload {
		return report.Workload{Kind: report.KindController, ID: ns + name, Name: name, Namespace: ns, State: report.StateOK, Labels: labels}
	}
	shop := map[string]string{"app.kubernetes.io/part-of": "shop"}
	c := Cluster{Name: "prod", Records: []report.Workload{
		ctrl("shop", "deployment/web", shop),
		ctrl("shop", "deployment/cart", shop),
		ctrl("shop", "statefulset/db", map[string]string{"app.kubernetes.io/name": "postgresql"}),
		ctrl("shop", "cronjob/backup", nil),
		ctrl("openshift-ingress", "deployment/router-default", nil),
		{Kind: report.KindVM, ID: "vm1", Name: "legacy", Namespace: "vms", State: report.StateOK},
		{Kind: report.KindService, ID: "s1", Name: "web", Namespace: "shop", State: report.StateOK,
			Facts: map[string]any{"ports": "80,443", "cluster_ips": "10.96.0.10", "external": "203.0.113.40,lb.example.net", "backends": "deployment/web"}},
		{Kind: report.KindService, ID: "s2", Name: "db", Namespace: "shop", State: report.StateOK, Facts: map[string]any{"ports": "5432", "backends": "statefulset/db"}},
		{Kind: report.KindService, ID: "s3", Name: "legacy", Namespace: "vms", State: report.StateOK, Facts: map[string]any{"ports": "22", "cluster_ips": "10.96.0.12", "backends": "vm/legacy"}},
		{Kind: report.KindIngress, ID: "i1", Name: "ingress/shop", Namespace: "shop", State: report.StateOK, Facts: map[string]any{"tls": true, "backends": "web:80"}},
		{Kind: report.KindIngress, ID: "r1", Name: "route/console", Namespace: "openshift-console", State: report.StateOK, Facts: map[string]any{"tls": true, "backends": "console:443"}},
	}}
	// bastion's haproxy sends to web's load balancer address.
	bastion := Host{Name: "bastion", Map: &report.Map{
		Interfaces: []report.Interface{{Name: "eth0", Kind: "physical", Addrs: []string{"10.0.0.5/24"}, Up: true}},
		Listen:     []report.Listener{{Owner: "unit:haproxy.service", Proto: "tcp", Addr: "*", Port: 443}},
		Conns:      []report.Conn{{Owner: "unit:haproxy.service", Dir: "out", Remote: "203.0.113.40", Port: 443, Count: 4}},
	}}
	g := Build([]Host{bastion}, Corrections{}, time.Now(), c)

	svcs := map[string]Service{}
	for _, s := range g.Services {
		svcs[s.ID] = s
	}
	shopSvc, ok := svcs["k8s:prod/shop/shop"]
	if !ok || shopSvc.Name != "shop" || shopSvc.Source != "kubernetes" || len(shopSvc.Parts) != 2 {
		t.Fatalf("shop: %+v", shopSvc)
	}
	if p := shopSvc.Parts[0]; p.Host != "prod" || (p.Owner != "k8s:shop/deployment/web" && p.Owner != "k8s:shop/deployment/cart") {
		t.Errorf("part: %+v", p)
	}
	if s := svcs["k8s:prod/shop/postgresql"]; s.Name != "postgresql" || len(s.Ports) != 1 || s.Ports[0] != 5432 {
		t.Errorf("postgresql: %+v", s)
	}
	if s := svcs["k8s:prod/vms/legacy"]; len(s.Parts) != 1 || s.Parts[0].Owner != "k8s:vms/vm/legacy" {
		t.Errorf("VM: %+v", s)
	}
	for id := range svcs {
		if id == "k8s:prod/shop/backup" || id == "k8s:prod/openshift-ingress/router-default" {
			t.Errorf("%s is on the map", id)
		}
	}
	in := svcs[IngressID("prod")]
	if in.Name != "prod ingress" || in.Tier != "edge" || len(in.Internet) != 1 || in.Internet[0] != 443 {
		t.Errorf("ingress: %+v", in)
	}
	want := map[Edge]bool{
		{From: IngressID("prod"), To: "k8s:prod/shop/shop", Port: 80, Source: "declared"}:        true,
		{From: "unit:haproxy", To: "k8s:prod/shop/shop", Port: 443, Count: 4, Source: "learned"}: true,
	}
	for _, e := range g.Edges {
		if !want[e] {
			t.Errorf("unexpected edge %+v", e)
		}
		delete(want, e)
	}
	for e := range want {
		t.Errorf("missing edge %+v", e)
	}

	// People move a part out; the move holds.
	cor := Corrections{Moved: map[string]string{PartKey("prod", "k8s:shop/deployment/cart"): "k8s:prod/shop/cart"}}
	g = Build([]Host{bastion}, cor, time.Now(), c)
	for _, s := range g.Services {
		if s.ID == "k8s:prod/shop/shop" && len(s.Parts) != 1 {
			t.Errorf("after the move: %+v", s)
		}
	}
}

// What only the edge sends to is an app, unless its kind holds data; what
// an app needs, with nothing below it, is data.
func TestTiersBehindTheEdge(t *testing.T) {
	svcs := []Service{
		{ID: "in", Internet: []int{443}},
		{ID: "shop"},
		{ID: "cache", Kind: "redis"},
		{ID: "api"},
		{ID: "db"},
		{ID: "alone"},
	}
	edges := []Edge{{From: "in", To: "shop"}, {From: "in", To: "cache"}, {From: "in", To: "api"}, {From: "api", To: "db"}}
	tierAndLayer(svcs, edges, nil, nil)
	want := map[string]string{"in": "edge", "shop": "apps", "cache": "data", "api": "apps", "db": "data", "alone": "apps"}
	for _, s := range svcs {
		if s.Tier != want[s.ID] {
			t.Errorf("%s: tier %s, want %s", s.ID, s.Tier, want[s.ID])
		}
	}
}

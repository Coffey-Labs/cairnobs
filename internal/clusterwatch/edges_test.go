package clusterwatch

import (
	"testing"
)

// Services, from their EndpointSlices, say which controllers and VMs are
// behind them; Ingresses and Routes say which Services they send to.
func TestEdgeRecords(t *testing.T) {
	v := view{}
	pod := func(uid, name, ownerKind, owner string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `","namespace":"shop","ownerReferences":[{"kind":"` + ownerKind + `","name":"` + owner + `","controller":true}]},"status":{"phase":"Running"}}`
	}
	v.add(t, "replicasets", `{"metadata":{"uid":"rs1","name":"web-6d4f","namespace":"shop","ownerReferences":[{"kind":"Deployment","name":"web","controller":true}]}}`)
	v.add(t, "pods", pod("p1", "web-6d4f-a", "ReplicaSet", "web-6d4f"))
	v.add(t, "pods", pod("p2", "web-6d4f-b", "ReplicaSet", "web-6d4f"))
	v.add(t, "pods", pod("p3", "db-0", "StatefulSet", "db"))
	v.add(t, "pods", pod("p4", "virt-launcher-legacy-x", "VirtualMachineInstance", "legacy"))

	svc := func(uid, name, spec, status string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `","namespace":"shop","creationTimestamp":"` + t0 + `"},"spec":` + spec + `,"status":` + status + `}`
	}
	v.add(t, "services", svc("s1", "web", `{"type":"LoadBalancer","clusterIPs":["10.96.0.10"],"selector":{"app":"web"},"ports":[{"port":80,"protocol":"TCP","nodePort":30080},{"port":443,"protocol":"TCP","nodePort":30443}]}`,
		`{"loadBalancer":{"ingress":[{"ip":"203.0.113.40"}]}}`))
	v.add(t, "services", svc("s2", "db", `{"type":"ClusterIP","clusterIPs":["None"],"selector":{"app":"db"},"ports":[{"port":5432,"protocol":"TCP"}]}`, `{}`))
	v.add(t, "services", svc("s3", "legacy", `{"type":"ClusterIP","clusterIPs":["10.96.0.12"],"selector":{"vm":"legacy"},"ports":[{"port":22,"protocol":"TCP"},{"port":53,"protocol":"UDP"}]}`, `{}`))
	v.add(t, "services", svc("s4", "idle", `{"type":"ClusterIP","clusterIPs":["10.96.0.13"],"selector":{"app":"idle"},"ports":[{"port":8080}]}`, `{}`))
	v.add(t, "services", svc("s5", "upstream", `{"type":"ExternalName","ports":[]}`, `{}`))

	slice := func(uid, svc, eps string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + svc + `-x","namespace":"shop","labels":{"kubernetes.io/service-name":"` + svc + `"}},"endpoints":[` + eps + `]}`
	}
	ep := func(pod string, ready bool) string {
		r := "true"
		if !ready {
			r = "false"
		}
		return `{"conditions":{"ready":` + r + `},"targetRef":{"kind":"Pod","name":"` + pod + `"}}`
	}
	v.add(t, "endpointslices", slice("e1", "web", ep("web-6d4f-a", true)+","+ep("web-6d4f-b", false)))
	v.add(t, "endpointslices", slice("e2", "db", ep("db-0", true)))
	v.add(t, "endpointslices", slice("e3", "legacy", ep("virt-launcher-legacy-x", true)))

	v.add(t, "ingresses", `{"metadata":{"uid":"i1","name":"shop","namespace":"shop"},"spec":{"ingressClassName":"nginx","tls":[{}],
		"rules":[{"host":"shop.example.org","http":{"paths":[{"backend":{"service":{"name":"web","port":{"number":80}}}}]}},
		         {"host":"api.example.org","http":{"paths":[{"backend":{"service":{"name":"api","port":{"name":"http"}}}}]}}]}}`)
	v.add(t, "routes", `{"metadata":{"uid":"r1","name":"admin","namespace":"shop"},"spec":{"host":"admin.apps.example.org","to":{"kind":"Service","name":"web"},"port":{"targetPort":8443},"tls":{}}}`)

	got := byName(build(v))
	for _, c := range []struct {
		key, state, word string
		facts            map[string]any
	}{
		{"service:web", "ok", "1 ready endpoint", map[string]any{"type": "LoadBalancer", "cluster_ips": "10.96.0.10", "external": "203.0.113.40",
			"ports": "80,443", "node_ports": "30080,30443", "backends": "deployment/web"}},
		{"service:db", "ok", "1 ready endpoint", map[string]any{"backends": "statefulset/db", "ports": "5432"}},
		{"service:legacy", "ok", "1 ready endpoint", map[string]any{"backends": "vm/legacy", "ports": "22,53/udp", "cluster_ips": "10.96.0.12"}},
		{"service:idle", "stopped", "No ready endpoints", nil},
		{"service:upstream", "ok", "A name for something outside the cluster", nil},
		{"ingress:ingress/shop", "ok", "2 hosts → 2 Services", map[string]any{"hosts": "api.example.org,shop.example.org", "backends": "api:http,web:80", "tls": true, "class": "nginx"}},
		{"ingress:route/admin", "ok", "admin.apps.example.org → web", map[string]any{"backends": "web:8443", "tls": true}},
	} {
		r, ok := got[c.key]
		if !ok || r.State != c.state || r.Word != c.word {
			t.Errorf("%s: %q %q, want %q %q", c.key, r.State, r.Word, c.state, c.word)
			continue
		}
		for k, w := range c.facts {
			if r.Facts[k] != w {
				t.Errorf("%s %s = %#v, want %#v", c.key, k, r.Facts[k], w)
			}
		}
	}
	if _, ok := got["service:db"].Facts["cluster_ips"]; ok {
		t.Error("a headless Service has no cluster address")
	}
}

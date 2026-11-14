package clusterwatch

import (
	"testing"

	"github.com/cairnobs/cairnobs/internal/report"
)

// OpenShift's parts, as OKD 4.x / 5.x serves them, become cluster records.
func TestOpenShiftParts(t *testing.T) {
	v := view{}
	// The version, mid-update and then failing; the operators; the pools.
	v.add(t, "clusterversions", `{"metadata":{"uid":"cv","name":"version"},"spec":{"channel":"stable-5"},
		"status":{"desired":{"version":"5.0.0-okd-scos.4"},
		"history":[{"state":"Partial","version":"5.0.0-okd-scos.4"},{"state":"Completed","version":"5.0.0-okd-scos.3"}],
		"conditions":[{"type":"Available","status":"True"},{"type":"Failing","status":"False"},
		{"type":"Progressing","status":"True","message":"Working towards 5.0.0-okd-scos.4: 512 of 873 done (58% complete)","lastTransitionTime":"`+t0+`"}]}}`)
	op := func(uid, name, conds string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `"},"status":{"versions":[{"name":"operator","version":"5.0.0-okd-scos.3"}],"conditions":[` + conds + `]}}`
	}
	v.add(t, "clusteroperators", op("o1", "dns", `{"type":"Available","status":"True"},{"type":"Degraded","status":"False"},{"type":"Progressing","status":"False"}`))
	v.add(t, "clusteroperators", op("o2", "ingress", `{"type":"Available","status":"True"},{"type":"Degraded","status":"True","reason":"IngressDegraded","message":"The \"default\" ingress controller reports Degraded=True"}`))
	v.add(t, "clusteroperators", op("o3", "authentication", `{"type":"Available","status":"False","reason":"OAuthServerRouteEndpointAccessibleController_EndpointUnavailable","message":"Get \"https://oauth-openshift.apps.example/healthz\": EOF"}`))
	v.add(t, "clusteroperators", op("o4", "network", `{"type":"Available","status":"True"},{"type":"Progressing","status":"True","message":"DaemonSet \"/openshift-ovn-kubernetes/ovnkube-node\" is not available (awaiting 1 nodes)"}`))
	pool := func(uid, name, spec, st string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `"},"spec":` + spec + `,"status":` + st + `}`
	}
	v.add(t, "machineconfigpools", pool("m1", "master", `{}`, `{"machineCount":3,"updatedMachineCount":3,"readyMachineCount":3,"conditions":[{"type":"Updated","status":"True"},{"type":"Updating","status":"False"},{"type":"Degraded","status":"False"}]}`))
	v.add(t, "machineconfigpools", pool("m2", "worker", `{}`, `{"machineCount":3,"updatedMachineCount":1,"readyMachineCount":1,"conditions":[{"type":"Updated","status":"False"},{"type":"Updating","status":"True"}]}`))
	v.add(t, "machineconfigpools", pool("m3", "infra", `{"paused":true}`, `{"machineCount":2,"updatedMachineCount":0,"conditions":[{"type":"Updated","status":"False"}]}`))
	v.add(t, "machineconfigpools", pool("m4", "gpu", `{}`, `{"machineCount":1,"degradedMachineCount":1,"conditions":[{"type":"Degraded","status":"True","reason":"1 nodes are reporting degraded status on sync","message":"Node gpu-0 is reporting: \"unexpected on-disk state\""}]}`))
	// Nodes the machine config operator is working on: one rebooting, one
	// not cordoned yet; and one it couldn't update.
	mnode := func(uid, name, ready, ann string) string {
		return `{"metadata":{"uid":"` + uid + `","name":"` + name + `","annotations":{` + ann + `}},"status":{"conditions":[{"type":"Ready","status":"` + ready + `"}]}}`
	}
	v.add(t, "nodes", mnode("n1", "worker-0", "False", `"machineconfiguration.openshift.io/state":"Working","machineconfiguration.openshift.io/currentConfig":"rendered-worker-a","machineconfiguration.openshift.io/desiredConfig":"rendered-worker-b"`))
	v.add(t, "nodes", mnode("n2", "worker-1", "True", `"machineconfiguration.openshift.io/state":"Done","machineconfiguration.openshift.io/currentConfig":"rendered-worker-a","machineconfiguration.openshift.io/desiredConfig":"rendered-worker-b"`))
	v.add(t, "nodes", mnode("n3", "gpu-0", "True", `"machineconfiguration.openshift.io/state":"Degraded","machineconfiguration.openshift.io/reason":"unexpected on-disk state"`))
	v.add(t, "nodes", mnode("n4", "worker-2", "True", `"machineconfiguration.openshift.io/state":"Done","machineconfiguration.openshift.io/currentConfig":"rendered-worker-b","machineconfiguration.openshift.io/desiredConfig":"rendered-worker-b"`))

	got := byName(build(v))
	for _, c := range []struct{ key, state, word string }{
		{"cluster:clusterversion/version", "updating", "Updating to 5.0.0-okd-scos.4: Working towards 5.0.0-okd-scos.4: 512 of 873 done (58% complete)"},
		{"cluster:clusteroperator/dns", "ok", "Available"},
		{"cluster:clusteroperator/ingress", "degraded", `Degraded: IngressDegraded: The "default" ingress controller reports Degraded=True`},
		{"cluster:clusteroperator/authentication", "down", `Not available: OAuthServerRouteEndpointAccessibleController_EndpointUnavailable: Get "https://oauth-openshift.apps.example/healthz": EOF`},
		{"cluster:clusteroperator/network", "updating", `Progressing: DaemonSet "/openshift-ovn-kubernetes/ovnkube-node" is not available (awaiting 1 nodes)`},
		{"cluster:machineconfigpool/master", "ok", "Up to date (3 nodes)"},
		{"cluster:machineconfigpool/worker", "updating", "Updating: 1 of 3 nodes done"},
		{"cluster:machineconfigpool/infra", "ok", "Paused, with an update waiting (0 of 2 nodes updated)"},
		{"cluster:machineconfigpool/gpu", "degraded", `Degraded: 1 nodes are reporting degraded status on sync: Node gpu-0 is reporting: "unexpected on-disk state"`},
		{"node:worker-0", "updating", "Updating its machine config, not ready (rebooting)"},
		{"node:worker-1", "updating", "Updating its machine config"},
		{"node:gpu-0", "degraded", "Machine config degraded: unexpected on-disk state"},
		{"node:worker-2", "ok", "Ready"},
	} {
		r, ok := got[c.key]
		if !ok || r.State != c.state || r.Word != c.word {
			t.Errorf("%s: %q %q, want %q %q", c.key, r.State, r.Word, c.state, c.word)
		}
	}
	cv := got["cluster:clusterversion/version"]
	if cv.Facts["version"] != "5.0.0-okd-scos.3" || cv.Facts["desired"] != "5.0.0-okd-scos.4" || cv.Facts["distribution"] != "OKD" || cv.Facts["channel"] != "stable-5" ||
		cv.Namespace != "" || cv.Since.IsZero() {
		t.Errorf("version record: %+v", cv)
	}

	// The update finishes; then a later one fails.
	v = view{}
	v.add(t, "clusterversions", `{"metadata":{"uid":"cv","name":"version"},"status":{"desired":{"version":"4.20.3"},
		"history":[{"state":"Completed","version":"4.20.3"}],"conditions":[{"type":"Progressing","status":"False"}]}}`)
	if r := byName(build(v))["cluster:clusterversion/version"]; r.State != report.StateOK || r.Word != "OpenShift 4.20.3" || r.Facts["desired"] != nil {
		t.Errorf("done: %+v", r)
	}
	v = view{}
	v.add(t, "clusterversions", `{"metadata":{"uid":"cv","name":"version"},"status":{"desired":{"version":"4.20.4"},
		"history":[{"state":"Partial","version":"4.20.4"},{"state":"Completed","version":"4.20.3"}],
		"conditions":[{"type":"Progressing","status":"True"},{"type":"Failing","status":"True","message":"Cluster operator machine-config is degraded"}]}}`)
	if r := byName(build(v))["cluster:clusterversion/version"]; r.State != report.StateDegraded || r.Word != "Update to 4.20.4 failing: Cluster operator machine-config is degraded" {
		t.Errorf("failing: %+v", r)
	}
}

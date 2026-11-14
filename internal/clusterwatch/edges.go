package clusterwatch

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Services, Ingresses and Routes become records the maps draw edges from
// (docs/maps.md, step 7): a Service says which controllers and VMs are
// behind it, from its EndpointSlices; an Ingress or Route says which
// Services it sends to. They aren't judged: the controllers are.

// edgeRecords is a record for every Service, Ingress and Route.
func (b *builder) edgeRecords(v view) []report.Workload {
	svcs := each[k8sService](v, "services")
	ings := each[ingress](v, "ingresses")
	routes := each[route](v, "routes")
	if len(svcs) == 0 && len(ings) == 0 && len(routes) == 0 {
		return nil
	}
	// Who runs each pod: "deployment/web", or "vm/db" for a VM's launcher.
	ownerOf := map[string]string{}
	for _, p := range each[pod](v, "pods") {
		owner, _ := b.podOwner(p.Metadata)
		if name, ok := strings.CutPrefix(owner, "virtualmachineinstance/"); ok {
			owner = "vm/" + name
		}
		if owner == "" {
			owner = "pod/" + p.Metadata.Name
		}
		ownerOf[nsName(p.Metadata)] = owner
	}
	type behind struct {
		owners []string
		ready  int
	}
	bySvc := map[string]*behind{}
	for _, s := range each[endpointSlice](v, "endpointslices") {
		name := s.Metadata.Labels["kubernetes.io/service-name"]
		if name == "" {
			continue
		}
		k := s.Metadata.Namespace + "/" + name
		bh := bySvc[k]
		if bh == nil {
			bh = &behind{}
			bySvc[k] = bh
		}
		for _, e := range s.Endpoints {
			if e.Conditions.Ready == nil || *e.Conditions.Ready {
				bh.ready++
			}
			if e.TargetRef != nil && e.TargetRef.Kind == "Pod" {
				if o := ownerOf[s.Metadata.Namespace+"/"+e.TargetRef.Name]; o != "" && !slices.Contains(bh.owners, o) {
					bh.owners = append(bh.owners, o)
				}
			}
		}
	}
	var out []report.Workload
	for _, s := range svcs {
		bh := bySvc[nsName(s.Metadata)]
		if bh == nil {
			bh = &behind{}
		}
		out = append(out, b.service(s, bh.owners, bh.ready))
	}
	for _, i := range ings {
		out = append(out, b.ingress(i))
	}
	for _, r := range routes {
		out = append(out, b.route(r))
	}
	return out
}

func joinSorted(s []string) string {
	s = slices.Clone(s)
	slices.Sort(s)
	return strings.Join(slices.Compact(s), ",")
}

func (b *builder) service(s *k8sService, owners []string, ready int) report.Workload {
	r := b.rec(report.KindService, s.Metadata.Name, s.Metadata)
	sp := s.Spec
	r.Facts = map[string]any{"type": sp.Type}
	var ips []string
	for _, ip := range sp.ClusterIPs {
		if ip != "" && ip != "None" {
			ips = append(ips, ip)
		}
	}
	if len(ips) > 0 {
		r.Facts["cluster_ips"] = strings.Join(ips, ",")
	}
	ext := slices.Clone(sp.ExternalIPs)
	for _, in := range s.Status.LoadBalancer.Ingress {
		ext = append(ext, cmpOr(in.IP, in.Hostname))
	}
	if len(ext) > 0 {
		r.Facts["external"] = joinSorted(ext)
	}
	var ports, nodePorts []string
	for _, p := range sp.Ports {
		w := strconv.Itoa(p.Port)
		if p.Protocol != "" && p.Protocol != "TCP" {
			w += "/" + strings.ToLower(p.Protocol)
		}
		ports = append(ports, w)
		if p.NodePort != 0 {
			nodePorts = append(nodePorts, strconv.Itoa(p.NodePort))
		}
	}
	if len(ports) > 0 {
		r.Facts["ports"] = strings.Join(ports, ",")
	}
	if len(nodePorts) > 0 {
		r.Facts["node_ports"] = strings.Join(nodePorts, ",")
	}
	if len(owners) > 0 {
		r.Facts["backends"] = joinSorted(owners)
	}
	r.State, r.Since = report.StateOK, s.Metadata.CreationTimestamp
	switch {
	case sp.Type == "ExternalName":
		r.Word = "A name for something outside the cluster"
	case len(sp.Selector) == 0 && ready == 0:
		r.Word = "No selector: its endpoints are set by hand"
	case ready == 0:
		r.State, r.Word = report.StateStopped, "No ready endpoints"
	default:
		r.Word = fmt.Sprintf("%d ready %s", ready, plural(ready, "endpoint", "endpoints"))
	}
	return r
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func backendOf(bk *ingressBackend) string {
	if bk == nil || bk.Service == nil || bk.Service.Name == "" {
		return ""
	}
	p := bk.Service.Port.Name
	if bk.Service.Port.Number != 0 {
		p = strconv.Itoa(bk.Service.Port.Number)
	}
	if p == "" {
		return bk.Service.Name
	}
	return bk.Service.Name + ":" + p
}

func (b *builder) ingress(i *ingress) report.Workload {
	r := b.rec(report.KindIngress, "ingress/"+i.Metadata.Name, i.Metadata)
	var hosts, backends []string
	if bk := backendOf(i.Spec.DefaultBackend); bk != "" {
		backends = append(backends, bk)
	}
	for _, rule := range i.Spec.Rules {
		if rule.Host != "" {
			hosts = append(hosts, rule.Host)
		}
		if rule.HTTP == nil {
			continue
		}
		for _, p := range rule.HTTP.Paths {
			if bk := backendOf(&p.Backend); bk != "" {
				backends = append(backends, bk)
			}
		}
	}
	r.Facts = map[string]any{"tls": len(i.Spec.TLS) > 0}
	if c := i.Spec.IngressClassName; c != nil && *c != "" {
		r.Facts["class"] = *c
	}
	return ingressRecord(r, hosts, backends, i.Metadata)
}

func (b *builder) route(rt *route) report.Workload {
	r := b.rec(report.KindIngress, "route/"+rt.Metadata.Name, rt.Metadata)
	var backends []string
	port := ""
	if p := rt.Spec.Port; p != nil && p.TargetPort != nil {
		port = ":" + fmt.Sprint(p.TargetPort)
		if f, ok := p.TargetPort.(float64); ok {
			port = ":" + strconv.Itoa(int(f))
		}
	}
	if rt.Spec.To.Kind == "Service" && rt.Spec.To.Name != "" {
		backends = append(backends, rt.Spec.To.Name+port)
	}
	for _, a := range rt.Spec.AlternateBackends {
		if a.Kind == "Service" && a.Name != "" {
			backends = append(backends, a.Name+port)
		}
	}
	var hosts []string
	if rt.Spec.Host != "" {
		hosts = append(hosts, rt.Spec.Host)
	}
	r.Facts = map[string]any{"tls": rt.Spec.TLS != nil, "class": "openshift-router"}
	return ingressRecord(r, hosts, backends, rt.Metadata)
}

func ingressRecord(r report.Workload, hosts, backends []string, m meta) report.Workload {
	if len(hosts) > 0 {
		r.Facts["hosts"] = joinSorted(hosts)
	}
	if len(backends) > 0 {
		r.Facts["backends"] = joinSorted(backends)
	}
	r.State, r.Since = report.StateOK, m.CreationTimestamp
	svcs := map[string]bool{}
	for _, bk := range backends {
		name, _, _ := strings.Cut(bk, ":")
		svcs[name] = true
	}
	from := "Any host"
	switch len(hosts) {
	case 0:
	case 1:
		from = hosts[0]
	default:
		from = fmt.Sprintf("%d hosts", len(hosts))
	}
	switch len(svcs) {
	case 0:
		r.Word = "Sends to no Service"
	case 1:
		name, _, _ := strings.Cut(backends[0], ":")
		r.Word = from + " → " + name
	default:
		r.Word = fmt.Sprintf("%s → %d Services", from, len(svcs))
	}
	return r
}

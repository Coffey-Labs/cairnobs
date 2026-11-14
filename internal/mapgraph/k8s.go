package mapgraph

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// Kubernetes on the service map (docs/maps.md, step 7). A cluster's
// controllers and VMs are parts; those with the same
// app.kubernetes.io/part-of label are one service, else the same name or
// instance label, else each is its own. A Service says which parts are
// behind it and which addresses reach them; an Ingress or Route sends the
// Internet to Services. Those edges are declared: the API says so.

// Cluster is one cluster agent's records: its controllers, VMs,
// Services, Ingresses and Routes (the rest are ignored here).
type Cluster struct {
	Name    string
	Records []report.Workload
}

// PlatformNamespace is where Kubernetes or OpenShift runs itself; it's
// left off the map, as it's left to its operators when judging.
func PlatformNamespace(ns string) bool {
	return strings.HasPrefix(ns, "kube-") || strings.HasPrefix(ns, "openshift-") || ns == "openshift"
}

// K8sOwner is a part's owner for a controller or VM: "k8s:shop/deployment/web",
// "k8s:vms/vm/db".
func K8sOwner(ns, name string) string { return "k8s:" + ns + "/" + name }

// IngressID is the service a cluster's Ingresses and Routes make: one box
// for the way in, however many there are.
func IngressID(cluster string) string { return "k8s:" + cluster + "/ingress" }

func fact(r report.Workload, k string) string {
	s, _ := r.Facts[k].(string)
	return s
}

func list(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func (b *builder) k8s(clusters []Cluster) {
	b.k8sAddr = map[netip.AddrPort][]string{}
	for _, c := range clusters {
		b.k8sCluster(c)
	}
}

func (b *builder) k8sCluster(c Cluster) {
	// Parts: controllers (not CronJobs: their Jobs serve nothing) and VMs.
	byOwner := map[string]string{} // ns/"deployment/web" or ns/"vm/db" → map service id
	for _, r := range c.Records {
		if PlatformNamespace(r.Namespace) {
			continue
		}
		var name string
		switch {
		case r.Kind == report.KindController && !strings.HasPrefix(r.Name, "cronjob/"):
			name = r.Name
		case r.Kind == report.KindVM:
			name = "vm/" + r.Name
		default:
			continue
		}
		short := name[strings.Index(name, "/")+1:]
		group := r.Labels["app.kubernetes.io/part-of"]
		if group == "" {
			group = r.Labels["app.kubernetes.io/name"]
		}
		if group == "" {
			group = r.Labels["app.kubernetes.io/instance"]
		}
		if group == "" {
			group = short
		}
		id := "k8s:" + c.Name + "/" + r.Namespace + "/" + group
		s := b.place(c.Name, K8sOwner(r.Namespace, name), id, group, "kubernetes")
		byOwner[r.Namespace+"/"+name] = s.ID
	}
	// Services: their ports go on what's behind them, and their addresses
	// lead there.
	behind := map[string][]string{} // ns/service → map service ids
	portOf := map[string]int{}      // ns/service → its first TCP port
	for _, r := range c.Records {
		if r.Kind != report.KindService || PlatformNamespace(r.Namespace) {
			continue
		}
		var ids []string
		for _, o := range list(fact(r, "backends")) {
			if id := byOwner[r.Namespace+"/"+o]; id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		key := r.Namespace + "/" + r.Name
		behind[key] = ids
		var ports []int
		for _, p := range list(fact(r, "ports")) {
			if n, err := strconv.Atoi(p); err == nil { // TCP; "53/udp" isn't
				ports = append(ports, n)
			}
		}
		if len(ports) > 0 {
			portOf[key] = ports[0]
		}
		for _, id := range ids {
			s := b.svc[id]
			for _, p := range ports {
				if !slices.Contains(s.Ports, p) {
					s.Ports = append(s.Ports, p)
				}
			}
		}
		for _, a := range append(list(fact(r, "cluster_ips")), list(fact(r, "external"))...) {
			ip, err := netip.ParseAddr(a)
			if err != nil {
				continue // a load balancer's hostname
			}
			for _, p := range ports {
				ap := netip.AddrPortFrom(ip, uint16(p))
				for _, id := range ids {
					if !slices.Contains(b.k8sAddr[ap], id) {
						b.k8sAddr[ap] = append(b.k8sAddr[ap], id)
					}
				}
			}
		}
	}
	// Ingresses and Routes: one way in for the cluster, with declared
	// edges to the services they send to.
	var in *Service
	for _, r := range c.Records {
		if r.Kind != report.KindIngress || PlatformNamespace(r.Namespace) {
			continue
		}
		if in == nil {
			in = b.place(c.Name, "k8s:ingress", IngressID(c.Name), c.Name+" ingress", "kubernetes")
			in.Kind = "ingress"
		}
		port := 80
		if t, _ := r.Facts["tls"].(bool); t {
			port = 443
		}
		if !slices.Contains(in.Internet, port) {
			in.Internet = append(in.Internet, port)
		}
		if !slices.Contains(in.Ports, port) {
			in.Ports = append(in.Ports, port)
		}
		for _, bk := range list(fact(r, "backends")) {
			name, p, _ := strings.Cut(bk, ":")
			key := r.Namespace + "/" + name
			n, err := strconv.Atoi(p)
			if err != nil {
				n = portOf[key]
			}
			for _, id := range behind[key] {
				b.declare(in.ID, id, n)
			}
		}
	}
}

// declare adds an edge the API states: a Kubernetes object, not a guess.
func (b *builder) declare(from, to string, port int) {
	if from == to {
		return
	}
	k := edgeKey{from, to, port}
	if e := b.edges[k]; e != nil {
		e.Source = "declared"
	} else {
		b.edges[k] = &Edge{From: from, To: to, Port: port, Source: "declared"}
	}
}

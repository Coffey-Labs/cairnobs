package server

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/cairnobs/cairnobs/internal/report"
)

// A PVC on a Pure array is one volume seen from two sides (docs/pure.md,
// step 3): the cluster agent sends the PersistentVolume a PVC is bound to
// ("pvc-<uid>", its `volume` fact), and Pure's CSI driver names the array
// volume after it ("k8s-pvc-<uid>", or "<pod>::k8s-pvc-<uid>" in a pod).
// Linked, each page shows the other side.

// VolumeLink is an array volume a PVC uses.
type VolumeLink struct {
	Source string `json:"source"` // the array's source key
	Array  string `json:"array"`  // its name for people: "fa-prod-01 (mgmt1)"
	Name   string `json:"name"`
	State  string `json:"state"`
	Word   string `json:"word"`
	Href   string `json:"href"`
}

// PVCLink is a PVC that uses an array volume.
type PVCLink struct {
	Source    string `json:"source"` // the cluster
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Href      string `json:"href"`
}

// pvName is a PersistentVolume's name as CSI drivers make it, at the end
// of an array volume's name.
var pvName = regexp.MustCompile(`pvc-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// volumeLinks matches PVCs' volumes with arrays' volumes, by
// PersistentVolume name. A volume in an ActiveCluster pod is on both
// arrays: a PVC links to each. Both are keyed source/namespace/name, as
// their conditions are (an array volume has no namespace).
func volumeLinks(snaps []wsnap) (byPVC map[string][]VolumeLink, byVolume map[string][]PVCLink) {
	type vol struct {
		s *wsnap
		r *report.Workload
	}
	onArrays := map[string][]vol{}
	for i := range snaps {
		s := &snaps[i]
		if !strings.HasPrefix(s.distribution(), "Purity") {
			continue
		}
		for j := range s.recs {
			r := &s.recs[j]
			if r.Kind != report.KindPVC {
				continue
			}
			if pv := pvName.FindString(strings.ToLower(r.Name)); pv != "" {
				onArrays[pv] = append(onArrays[pv], vol{s, r})
			}
		}
	}
	byPVC, byVolume = map[string][]VolumeLink{}, map[string][]PVCLink{}
	if len(onArrays) == 0 {
		return byPVC, byVolume
	}
	for i := range snaps {
		s := &snaps[i]
		for _, r := range s.recs {
			if r.Kind != report.KindPVC {
				continue
			}
			pv, _ := r.Facts["volume"].(string)
			vs := onArrays[strings.ToLower(pv)]
			if pv == "" || len(vs) == 0 {
				continue
			}
			pk := s.name + "/" + r.Namespace + "/" + r.Name
			for _, v := range vs {
				byPVC[pk] = append(byPVC[pk], VolumeLink{Source: v.s.name, Array: v.s.title(), Name: v.r.Name, State: v.r.State, Word: v.r.Word,
					Href: workloadHref(v.s.name, "", v.r.Name)})
				vk := v.s.name + "//" + v.r.Name
				byVolume[vk] = append(byVolume[vk], PVCLink{Source: s.name, Namespace: r.Namespace, Name: r.Name, State: r.State,
					Href: workloadHref(s.name, r.Namespace, r.Name)})
			}
		}
	}
	return byPVC, byVolume
}

// linkConditions tells each side of a link about the other: a PVC's
// condition names its array volume, an array volume's the PVCs it
// serves, and an array's own (in trouble, or silent) how many PVCs its
// volumes hold, and in which clusters.
func linkConditions(conds []Condition, byPVC map[string][]VolumeLink, byVolume map[string][]PVCLink) {
	if len(byPVC) == 0 {
		return
	}
	perArray := map[string][]PVCLink{}
	for k, ps := range byVolume {
		src, _, _ := strings.Cut(k, "//")
		perArray[src] = append(perArray[src], ps...)
	}
	for i := range conds {
		c := &conds[i]
		kind, rest, _ := strings.Cut(c.Key, "/")
		var add string
		switch {
		case kind == "pvc" && len(byPVC[rest]) > 0:
			var on []string
			for _, v := range byPVC[rest] {
				on = append(on, fmt.Sprintf("volume %s on %s: %s", v.Name, v.Array, lowerFirst(v.Word)))
			}
			add = "Its array side: " + strings.Join(on, "; ") + "."
		case kind == "pvc" && len(byVolume[rest]) > 0:
			var by []string
			for _, p := range byVolume[rest] {
				by = append(by, where(p.Name, p.Namespace)+" ("+p.Source+")")
			}
			add = "Used by PVC " + strings.Join(by, ", ") + "."
		case kind == "cluster":
			src, what, _ := strings.Cut(rest, "/")
			ps := perArray[src]
			if (what != "cluster" && what != "silent") || len(ps) == 0 {
				break
			}
			var clusters []string
			for _, p := range ps {
				if !slices.Contains(clusters, p.Source) {
					clusters = append(clusters, p.Source)
				}
			}
			slices.Sort(clusters)
			add = fmt.Sprintf("Its volumes hold %s, in %s.", plural(len(ps), "PVC", "PVCs"), strings.Join(clusters, ", "))
		}
		if add != "" {
			c.Detail = strings.TrimSpace(c.Detail + " " + add)
		}
	}
}

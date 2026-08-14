// Exercises the hand-written DeepCopy methods in zz_generated.deepcopy.go
// -- see that file's doc comment for why these aren't controller-gen
// output here. A DeepCopy that accidentally shares a slice/map with the
// original is a real, easy-to-introduce bug (client-go relies on
// DeepCopyObject returning something safe to mutate independently), so
// these tests mutate the copy and assert the original is unaffected.
package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTenantDeepCopyIsIndependent(t *testing.T) {
	orig := &Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Labels: map[string]string{"a": "1"}},
		Spec:       TenantSpec{DisplayName: "Acme", Suspended: false},
		Status: TenantStatus{
			Phase: PhaseActive,
			Conditions: []metav1.Condition{
				{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "x"},
			},
		},
	}

	cp := orig.DeepCopy()
	cp.Spec.DisplayName = "Changed"
	cp.Status.Conditions[0].Reason = "changed"
	cp.Labels["a"] = "changed"

	if orig.Spec.DisplayName != "Acme" {
		t.Fatalf("mutating the copy's Spec affected the original: %q", orig.Spec.DisplayName)
	}
	if orig.Status.Conditions[0].Reason != "x" {
		t.Fatalf("mutating the copy's Conditions affected the original: %q", orig.Status.Conditions[0].Reason)
	}
	// Labels comes from metav1.ObjectMeta.DeepCopyInto, which this
	// package doesn't implement itself -- this assertion is really
	// checking that Tenant.DeepCopyInto actually calls
	// ObjectMeta.DeepCopyInto rather than doing a shallow `out.ObjectMeta
	// = in.ObjectMeta`.
	if orig.Labels["a"] != "1" {
		t.Fatalf("mutating the copy's Labels affected the original: %q", orig.Labels["a"])
	}
}

func TestTenantDeepCopyObjectPreservesData(t *testing.T) {
	orig := &Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: TenantSpec{DisplayName: "Acme"}}
	obj := orig.DeepCopyObject()
	cp, ok := obj.(*Tenant)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *Tenant", obj)
	}
	if cp.Name != "acme" || cp.Spec.DisplayName != "Acme" {
		t.Fatalf("unexpected copy: %+v", cp)
	}
}

func TestTenantListDeepCopyIsIndependent(t *testing.T) {
	orig := &TenantList{Items: []Tenant{
		{ObjectMeta: metav1.ObjectMeta{Name: "acme"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "globex"}},
	}}
	cp := orig.DeepCopy()
	cp.Items[0].Name = "changed"

	if orig.Items[0].Name != "acme" {
		t.Fatalf("mutating the copy's Items affected the original: %q", orig.Items[0].Name)
	}
}

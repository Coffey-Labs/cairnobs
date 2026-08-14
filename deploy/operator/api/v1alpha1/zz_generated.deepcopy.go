// Hand-written, not `controller-gen object:headerFile=...` generated --
// no controller-gen binary available in this environment (see
// groupversion_info.go's doc comment). Kept under the conventional
// zz_generated.deepcopy.go name so its purpose is recognizable, and
// covered by api_test.go's round-trip tests since a hand-written
// DeepCopy is exactly the kind of code a typo silently breaks.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *TenantSpec) DeepCopy() *TenantSpec {
	if in == nil {
		return nil
	}
	out := new(TenantSpec)
	*out = *in
	return out
}

func (in *TenantStatus) DeepCopyInto(out *TenantStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *TenantStatus) DeepCopy() *TenantStatus {
	if in == nil {
		return nil
	}
	out := new(TenantStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *Tenant) DeepCopyInto(out *Tenant) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Tenant) DeepCopy() *Tenant {
	if in == nil {
		return nil
	}
	out := new(Tenant)
	in.DeepCopyInto(out)
	return out
}

func (in *Tenant) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *TenantList) DeepCopyInto(out *TenantList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Tenant, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *TenantList) DeepCopy() *TenantList {
	if in == nil {
		return nil
	}
	out := new(TenantList)
	in.DeepCopyInto(out)
	return out
}

func (in *TenantList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

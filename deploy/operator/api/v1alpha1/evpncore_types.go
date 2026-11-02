package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=evpncores,scope=Namespaced,shortName=ecore,kind=EvpnCore

type EvpnCoreSpec struct {
	Enabled      bool   `json:"enabled,omitempty"`
	AppNamespace string `json:"appNamespace,omitempty"`
	OvnNamespace string `json:"ovnNamespace,omitempty"`
	AppImage     string `json:"appImage,omitempty"`
	GobgpImage   string `json:"gobgpImage,omitempty"`
	OvnImage     string `json:"ovnImage,omitempty"`
}

type EvpnCoreStatus struct {
	Phase  string `json:"phase,omitempty"`
	Ready  bool   `json:"ready,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// +kubebuilder:object:root=true
// EvpnCore is the CR that configures the EVPN core components in the lab.
type EvpnCore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EvpnCoreSpec   `json:"spec,omitempty"`
	Status            EvpnCoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// EvpnCoreList contains a list of EvpnCore.
type EvpnCoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EvpnCore `json:"items"`
}

var (
	SchemeGroupVersion = schema.GroupVersion{Group: "evpn.core.example.com", Version: "v1alpha1"}
	SchemeBuilder      = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme        = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion, &EvpnCore{}, &EvpnCoreList{})
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

func (in *EvpnCore) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(EvpnCore)
	in.DeepCopyInto(out)
	return out
}

func (in *EvpnCore) DeepCopyInto(out *EvpnCore) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

func (in *EvpnCoreList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(EvpnCoreList)
	in.DeepCopyInto(out)
	return out
}

func (in *EvpnCoreList) DeepCopyInto(out *EvpnCoreList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]EvpnCore, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

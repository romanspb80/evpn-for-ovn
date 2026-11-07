package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=evpncores,scope=Namespaced,shortName=ecore,kind=EvpnCore

type EvpnCoreSpec struct {
	Enabled       bool          `json:"enabled"`
	AppNamespace  string        `json:"appNamespace,omitempty"`
	OvnNamespace  string        `json:"ovnNamespace,omitempty"`
	AppImage      string        `json:"appImage,omitempty"`
	GobgpImage    string        `json:"gobgpImage,omitempty"`
	OvnImage      string        `json:"ovnImage,omitempty"`
	DataplaneNode string        `json:"dataplaneNode,omitempty"`
	NodeAgent     NodeAgentSpec `json:"nodeAgent,omitempty"`
	BGP           BGPSpec       `json:"bgp,omitempty"`
}

type BGPSpec struct {
	// Provider selects the independent speaker deployment provider.
	// +kubebuilder:default=k8gobgp
	// +kubebuilder:validation:Enum=k8gobgp
	Provider string `json:"provider,omitempty"`
}

type NodeAgentSpec struct {
	Enabled                 bool                `json:"enabled,omitempty"`
	Image                   string              `json:"image,omitempty"`
	NodeSelector            map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations             []corev1.Toleration `json:"tolerations,omitempty"`
	Mode                    string              `json:"mode,omitempty"`
	NetworkBackend          string              `json:"networkBackend,omitempty"`
	Socket                  string              `json:"socket,omitempty"`
	OVNNBEndpoint           string              `json:"ovnNBEndpoint,omitempty"`
	OVSDBEndpoint           string              `json:"ovsdbEndpoint,omitempty"`
	OVNSBEndpoint           string              `json:"ovnSBEndpoint,omitempty"`
	ExportConfigPath        string              `json:"exportConfigPath,omitempty"`
	ExportStatusPath        string              `json:"exportStatusPath,omitempty"`
	ExportInterval          string              `json:"exportInterval,omitempty"`
	ExportTimeout           string              `json:"exportTimeout,omitempty"`
	RemoteConfigPath        string              `json:"remoteConfigPath,omitempty"`
	RemoteStatusPath        string              `json:"remoteStatusPath,omitempty"`
	RemoteInterval          string              `json:"remoteInterval,omitempty"`
	RemoteTimeout           string              `json:"remoteTimeout,omitempty"`
	GoBGPHostPath           string              `json:"gobgpHostPath,omitempty"`
	CNIContinuityConfigPath string              `json:"cniContinuityConfigPath,omitempty"`
	// ProviderAttestation enables local, read-only Kubernetes/containerd
	// inspection. The host mounts remain an administrator privilege boundary.
	ProviderAttestation bool   `json:"providerAttestation,omitempty"`
	ProviderCTRHostPath string `json:"providerCTRHostPath,omitempty"`
}

type ManagedResource struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	// State is CreationPending until creation is confirmed, then Observed.
	State    string `json:"state,omitempty"`
	Conflict string `json:"conflict,omitempty"`
}

type EvpnCoreStatus struct {
	Phase              string `json:"phase,omitempty"`
	Ready              bool   `json:"ready,omitempty"`
	Reason             string `json:"reason,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	ManagedResources []ManagedResource  `json:"managedResources,omitempty"`
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
	SchemeGroupVersion = schema.GroupVersion{Group: "evpn.romanspb80.github.io", Version: "v1alpha1"}
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
	if in.Spec.NodeAgent.NodeSelector != nil {
		out.Spec.NodeAgent.NodeSelector = make(map[string]string, len(in.Spec.NodeAgent.NodeSelector))
		for k, v := range in.Spec.NodeAgent.NodeSelector {
			out.Spec.NodeAgent.NodeSelector[k] = v
		}
	}
	if in.Spec.NodeAgent.Tolerations != nil {
		out.Spec.NodeAgent.Tolerations = make([]corev1.Toleration, len(in.Spec.NodeAgent.Tolerations))
		for i := range in.Spec.NodeAgent.Tolerations {
			in.Spec.NodeAgent.Tolerations[i].DeepCopyInto(&out.Spec.NodeAgent.Tolerations[i])
		}
	}
	out.Status = in.Status
	out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
	out.Status.ManagedResources = append([]ManagedResource(nil), in.Status.ManagedResources...)
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

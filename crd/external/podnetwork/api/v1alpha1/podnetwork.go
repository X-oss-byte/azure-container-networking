//go:build !ignore_uncovered
// +build !ignore_uncovered

package v1alpha

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Important: Run "make" to regenerate code after modifying this file

// +kubebuilder:object:root=true

// PodNetwork is the Schema for the PodNetworks API
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:resource:shortName=pn
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type=string,priority=1,JSONPath=`.status.status`
// +kubebuilder:printcolumn:name="Error Message",type=string,priority=1,JSONPath=`.status.errorMessage`
// +kubebuilder:printcolumn:name="Address Prefixes",type=string,priority=1,JSONPath=`.status.addressPrefixes`
// +kubebuilder:printcolumn:name="Network",type=string,priority=1,JSONPath=`.spec.network`
// +kubebuilder:printcolumn:name="Subnet",type=string,priority=1,JSONPath=`.spec.subnet`
type PodNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodNetworkSpec   `json:"spec,omitempty"`
	Status PodNetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodNetworkList contains a list of PodNetwork
type PodNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodNetwork `json:"items"`
}

// PodNetworkSpec defines the desired state of PodNetwork
type PodNetworkSpec struct {
	// +kubebuilder:validation:Optional
	// customer vnet guid
	network string `json:"network,omitempty"`
	// customer subnet name
	subnet string `json:"subnet,omitempty"`
}

// Status indicates the status of PN
// +kubebuilder:default=Nil
// +kubebuilder:validation:Enum=Nil;Ready;InUse;SubnetNotDelegated
type Status string

const (
	Nil                Status = "Nil"
	Ready              Status = "Ready"
	InUse              Status = "InUse"
	SubnetNotDelegated Status = "SubnetNotDelegated"
)

// PodNetworkStatus defines the observed state of PodNetwork
type PodNetworkStatus struct {
	// +kubebuilder:validation:Optional
	Status          Status   `json:"status,omitempty"`
	ErrorMessage    string   `json:"errorMessage,omitempty"`
	AddressPrefixes []string `json:"addressPrefixes,omitempty"`
}

func init() {
	SchemeBuilder.Register(&PodNetwork{}, &PodNetworkList{})
}

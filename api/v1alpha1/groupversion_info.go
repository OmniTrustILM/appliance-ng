// Package v1alpha1 contains the API types of the appliance.otilm.com group.
// +kubebuilder:object:generate=true
// +groupName=appliance.otilm.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of the appliance API.
	GroupVersion = schema.GroupVersion{Group: "appliance.otilm.com", Version: "v1alpha1"}

	// SchemeBuilder registers the API types with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the API types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &Appliance{}, &ApplianceList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

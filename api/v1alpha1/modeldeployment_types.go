/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ModelSource 描述模型檔從哪裡來、存成什麼名字。
type ModelSource struct {
	// URL 是 GGUF 檔的下載位址。只接受 https。
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`

	// File 是存進容器 /models 底下的檔名。
	// 只允許英數、點、底線、連字號，且必須以 .gguf 結尾 —— 擋掉路徑穿越。
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+\.gguf$`
	File string `json:"file"`
}

// ModelDeploymentSpec 是使用者宣告的期望狀態。
type ModelDeploymentSpec struct {
	// Runtime 是推論引擎。目前只支援 llamacpp。
	// +kubebuilder:validation:Enum=llamacpp
	// +kubebuilder:default=llamacpp
	// +optional
	Runtime string `json:"runtime,omitempty"`

	// Model 是要載入的模型。
	Model ModelSource `json:"model"`

	// Replicas 是推論 pod 數量。可以是 0（停用但保留設定）。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image 覆蓋預設的推論 image。
	// +optional
	Image string `json:"image,omitempty"`

	// Resources 覆蓋推論容器預設的 requests/limits。
	// +kubebuilder:validation:XValidation:rule="!has(self.requests) || !has(self.limits) || ['cpu', 'memory'].all(r, !(r in self.requests) || !(r in self.limits) || quantity(string(self.requests[r])).compareTo(quantity(string(self.limits[r]))) <= 0)",message="resources.requests must not exceed resources.limits (cpu, memory)"
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ModelDeploymentStatus 是 controller 觀察到的實際狀態。
type ModelDeploymentStatus struct {
	// ObservedGeneration 是 controller 最後處理過的 metadata.generation。
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ReadyReplicas 是通過 readiness probe 的 pod 數量。
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Endpoint 是叢集內呼叫推論服務的網址。
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Conditions 依 Kubernetes 慣例表達狀態，目前只有 Available。
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=md
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtime`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelDeployment is the Schema for the modeldeployments API
type ModelDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ModelDeployment
	// +required
	Spec ModelDeploymentSpec `json:"spec"`

	// status defines the observed state of ModelDeployment
	// +optional
	Status ModelDeploymentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ModelDeploymentList contains a list of ModelDeployment
type ModelDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ModelDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ModelDeployment{}, &ModelDeploymentList{})
		return nil
	})
}

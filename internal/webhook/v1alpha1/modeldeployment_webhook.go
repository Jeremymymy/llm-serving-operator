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
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

// SetupModelDeploymentWebhookWithManager registers the webhook for ModelDeployment in the manager.
func SetupModelDeploymentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &servingv1alpha1.ModelDeployment{}).
		// mgr.GetClient() 讀的是 controller 本來就有的 informer cache：不用新的 RBAC，也不會每個請求都打一次 API server。
		WithValidator(&ModelDeploymentValidator{Reader: mgr.GetClient()}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-serving-llmlab-dev-v1alpha1-modeldeployment,mutating=false,failurePolicy=fail,sideEffects=None,groups=serving.llmlab.dev,resources=modeldeployments,verbs=create,versions=v1alpha1,name=vmodeldeployment-v1alpha1.kb.io,admissionReviewVersions=v1

// ModelDeploymentValidator 在建立 md 時檢查：controller 要用的子資源名稱，是不是已經被別人用掉了。
// 這需要讀叢集裡的其他物件，CEL 做不到（CEL 只看得到被寫入的這一個物件）。
type ModelDeploymentValidator struct {
	Reader client.Reader
}

// ValidateCreate 拒絕名稱撞到既有、且不屬於同名 md 的 Deployment／Service／PVC。
// 不擋的話，controller 的 SSA（ForceOwnership）會接手那個物件，md 被刪時 GC 會連它一起刪掉（docs/05 實驗 1）。
func (v *ModelDeploymentValidator) ValidateCreate(ctx context.Context, md *servingv1alpha1.ModelDeployment) (admission.Warnings, error) {
	children := []struct {
		kind string
		obj  client.Object
		name string
	}{
		{"Deployment", &appsv1.Deployment{}, md.Name},
		{"Service", &corev1.Service{}, md.Name},
		// 要跟 internal/controller 的 pvcName() 一致。
		{"PersistentVolumeClaim", &corev1.PersistentVolumeClaim{}, md.Name + "-models"},
	}
	for _, c := range children {
		err := v.Reader.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: c.name}, c.obj)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("check %s %q: %w", c.kind, c.name, err)
		}
		if !controlledByModelDeployment(c.obj, md.Name) {
			return nil, fmt.Errorf("%s %q already exists in namespace %q and is not managed by ModelDeployment %q; "+
				"choose another name", c.kind, c.name, md.Namespace, md.Name)
		}
	}
	return nil, nil
}

// ValidateUpdate 不檢查：名稱不能改，子資源在建立時已經確認過。
func (v *ModelDeploymentValidator) ValidateUpdate(_ context.Context, _, _ *servingv1alpha1.ModelDeployment) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete 不檢查：marker 的 verbs 只有 create，API server 不會為了刪除呼叫這個 webhook。
func (v *ModelDeploymentValidator) ValidateDelete(_ context.Context, _ *servingv1alpha1.ModelDeployment) (admission.Warnings, error) {
	return nil, nil
}

// controlledByModelDeployment：同名 md 被刪、GC 還沒清完時，子資源的 controller 仍是（舊的）同名 md，這種情況放行。
func controlledByModelDeployment(obj client.Object, mdName string) bool {
	ref := metav1.GetControllerOf(obj)
	return ref != nil &&
		ref.APIVersion == servingv1alpha1.GroupVersion.String() &&
		ref.Kind == "ModelDeployment" &&
		ref.Name == mdName
}

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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

var _ = Describe("ModelDeployment Webhook", func() {
	const (
		name = "chat"
		ns   = "default"
	)

	newMD := func() *servingv1alpha1.ModelDeployment {
		return &servingv1alpha1.ModelDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: servingv1alpha1.ModelDeploymentSpec{
				Model: servingv1alpha1.ModelSource{URL: "https://example.com/m.gguf", File: "m.gguf"},
			},
		}
	}
	// validatorWith 用 fake client 扮演 informer cache，裡面放著叢集「已經有」的物件。
	validatorWith := func(objs ...client.Object) *ModelDeploymentValidator {
		return &ModelDeploymentValidator{Reader: fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()}
	}
	ownedBy := func(mdName string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{
			APIVersion: servingv1alpha1.GroupVersion.String(), Kind: "ModelDeployment",
			Name: mdName, UID: "old-uid", Controller: new(true),
		}}
	}

	It("admits a ModelDeployment whose children do not exist yet", func() {
		Expect(validatorWith().ValidateCreate(ctx, newMD())).Error().NotTo(HaveOccurred())
	})

	DescribeTable("denies a name that collides with an object it does not own",
		func(existing client.Object, wantMsg string) {
			_, err := validatorWith(existing).ValidateCreate(ctx, newMD())
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
		},
		Entry("Service created by someone else",
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
			`Service "chat" already exists`),
		Entry("Deployment created by someone else",
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
			`Deployment "chat" already exists`),
		Entry("model cache PVC created by someone else",
			&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name + "-models", Namespace: ns}},
			`PersistentVolumeClaim "chat-models" already exists`),
		Entry("Service owned by a different ModelDeployment",
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: ownedBy("other")}},
			`Service "chat" already exists`),
	)

	It("admits a name whose children still belong to a deleted ModelDeployment of the same name", func() {
		leftover := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: ownedBy(name)}}
		Expect(validatorWith(leftover).ValidateCreate(ctx, newMD())).Error().NotTo(HaveOccurred())
	})

	It("ignores objects with the same name in another namespace", func() {
		elsewhere := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "other"}}
		Expect(validatorWith(elsewhere).ValidateCreate(ctx, newMD())).Error().NotTo(HaveOccurred())
	})

	It("is wired into the API server: a colliding create is rejected end to end", func() {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		}
		Expect(k8sClient.Create(ctx, svc)).To(Succeed())
		DeferCleanup(k8sClient.Delete, ctx, svc)

		// webhook 讀的是 cache，Service 剛建立時 cache 可能還沒看到，所以用 Eventually。
		Eventually(func() error { return k8sClient.Create(ctx, newMD()) }).
			Should(MatchError(ContainSubstring(`denied the request: Service "chat" already exists`)))
	})
})

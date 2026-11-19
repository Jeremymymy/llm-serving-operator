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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

var _ = Describe("ModelDeployment Controller", func() {
	const name = "smollm"
	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: "default"}

	newMD := func() *servingv1alpha1.ModelDeployment {
		return &servingv1alpha1.ModelDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: servingv1alpha1.ModelDeploymentSpec{
				Model: servingv1alpha1.ModelSource{
					URL:  "https://example.com/model.gguf",
					File: "model.gguf",
				},
			},
		}
	}

	reconcileOnce := func() {
		r := &ModelDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	AfterEach(func() {
		md := &servingv1alpha1.ModelDeployment{}
		if err := k8sClient.Get(ctx, key, md); err == nil {
			Expect(k8sClient.Delete(ctx, md)).To(Succeed())
		}
		// envtest 沒有 garbage collector：刪了 owner，子資源不會跟著消失，要手動清。
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
	})

	It("creates an owned Deployment and Service with defaults applied", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(md.Spec.Runtime).To(Equal("llamacpp"))        // CRD default
		Expect(md.Spec.Replicas).To(Equal(ptr.To[int32](1))) // CRD default

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(defaultLlamaCppImage))
		Expect(dep.Spec.Template.Spec.Containers[0].Args).To(ContainElement("/models/model.gguf"))

		owner := metav1.GetControllerOf(dep)
		Expect(owner).NotTo(BeNil())
		Expect(owner.Kind).To(Equal("ModelDeployment"))
		Expect(owner.UID).To(Equal(md.UID))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(servingPort)))
		Expect(metav1.GetControllerOf(svc).UID).To(Equal(md.UID))
	})

	It("writes status, and reports Progressing because envtest has no deployment controller", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(md.Status.Endpoint).To(Equal("http://smollm.default.svc:8080"))
		Expect(md.Status.ObservedGeneration).To(Equal(md.Generation))

		c := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("Progressing"))
	})

	It("propagates spec.replicas changes to the Deployment", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		md.Spec.Replicas = ptr.To[int32](3)
		Expect(k8sClient.Update(ctx, md)).To(Succeed())
		reconcileOnce()

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
	})

	It("reports ScaledToZero when replicas is 0", func() {
		md := newMD()
		md.Spec.Replicas = ptr.To[int32](0)
		Expect(k8sClient.Create(ctx, md)).To(Succeed())
		reconcileOnce()

		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		c := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(c).NotTo(BeNil())
		Expect(c.Reason).To(Equal("ScaledToZero"))
	})

	DescribeTable("rejects invalid specs at the API server",
		func(mutate func(*servingv1alpha1.ModelDeployment)) {
			md := newMD()
			mutate(md)
			err := k8sClient.Create(ctx, md)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got: %v", err)
		},
		Entry("path traversal in file", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Model.File = "../x.gguf" }),
		Entry("non-gguf file", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Model.File = "model.bin" }),
		Entry("plain http url", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Model.URL = "http://example.com/m.gguf" }),
		Entry("unknown runtime", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Runtime = "vllm" }),
		Entry("negative replicas", func(md *servingv1alpha1.ModelDeployment) { md.Spec.Replicas = ptr.To[int32](-1) }),
	)
})

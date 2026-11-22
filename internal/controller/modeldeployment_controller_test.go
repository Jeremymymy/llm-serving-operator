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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/Jeremymymy/llm-serving-operator/api/v1alpha1"
)

var _ = Describe("ModelDeployment Controller", func() {
	const (
		name = "smollm"
		ns   = "default"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: ns}
	pvcKey := types.NamespacedName{Name: name + "-models", Namespace: ns}

	var recorder *events.FakeRecorder

	newMD := func() *servingv1alpha1.ModelDeployment {
		return &servingv1alpha1.ModelDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: servingv1alpha1.ModelDeploymentSpec{
				Model: servingv1alpha1.ModelSource{
					URL:  "https://example.com/model.gguf",
					File: "model.gguf",
				},
			},
		}
	}

	reconcileOnce := func() {
		r := &ModelDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		recorder = events.NewFakeRecorder(10)
	})

	AfterEach(func() {
		md := &servingv1alpha1.ModelDeployment{}
		if err := k8sClient.Get(ctx, key, md); err == nil {
			Expect(k8sClient.Delete(ctx, md)).To(Succeed())
		}
		// envtest 沒有 garbage collector：刪了 owner，子資源不會跟著消失，要手動清。
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		_ = k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(ns), client.GracePeriodSeconds(0))
		// API server 會替 PVC 加上 kubernetes.io/pvc-protection finalizer，平常由 kube-controller-manager 拿掉。
		// envtest 沒有它，不手動拿掉的話 PVC 會永遠卡在 Terminating。
		pvc := &corev1.PersistentVolumeClaim{}
		if err := k8sClient.Get(ctx, pvcKey, pvc); err == nil {
			pvc.Finalizers = nil
			Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
			_ = k8sClient.Delete(ctx, pvc)
		}
	})

	It("creates owned PVC, Deployment and Service with defaults applied", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(md.Spec.Runtime).To(Equal("llamacpp"))
		Expect(md.Spec.Replicas).To(Equal(ptr.To[int32](1)))

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(defaultLlamaCppImage))
		Expect(dep.Spec.Template.Spec.Containers[0].Args).To(ContainElement("/models/model.gguf"))
		Expect(dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(pvcKey.Name))
		Expect(metav1.GetControllerOf(dep).UID).To(Equal(md.UID))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(servingPort)))
		Expect(metav1.GetControllerOf(svc).UID).To(Equal(md.UID))

		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
		Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))
		Expect(metav1.GetControllerOf(pvc).UID).To(Equal(md.UID))
	})

	It("writes status, and reports Progressing because envtest has no deployment controller", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(md.Status.Endpoint).To(Equal("http://smollm.default.svc:8080"))
		Expect(md.Status.ObservedGeneration).To(Equal(md.Generation))

		a := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(a).NotTo(BeNil())
		Expect(a.Status).To(Equal(metav1.ConditionFalse))
		Expect(a.Reason).To(Equal("Progressing"))
		d := meta.FindStatusCondition(md.Status.Conditions, conditionDegraded)
		Expect(d).NotTo(BeNil())
		Expect(d.Status).To(Equal(metav1.ConditionFalse))
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

	// Phase 1 實驗 7 的回歸測試：沒有變更時，子資源不應該被寫入。
	It("does not write children when nothing changed", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()
		// 實測：PVC 建立後的第一次 re-apply 會多寫一次（內容完全相同、只有 resourceVersion 變），之後就穩定。
		// 所以先多跑一輪讓它穩定，再量「沒有變更時有沒有寫入」。
		reconcileOnce()

		rv := func(obj client.Object, k types.NamespacedName) string {
			Expect(k8sClient.Get(ctx, k, obj)).To(Succeed())
			return obj.GetResourceVersion()
		}
		depRV, svcRV, pvcRV := rv(&appsv1.Deployment{}, key), rv(&corev1.Service{}, key), rv(&corev1.PersistentVolumeClaim{}, pvcKey)

		reconcileOnce()

		Expect(rv(&appsv1.Deployment{}, key)).To(Equal(depRV))
		Expect(rv(&corev1.Service{}, key)).To(Equal(svcRV))
		Expect(rv(&corev1.PersistentVolumeClaim{}, pvcKey)).To(Equal(pvcRV))
	})

	It("takes back fields it owns, and leaves other managers' fields alone", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		// 模擬另一個人：改 replicas（我們擁有的欄位）、加 annotation（我們沒宣告的欄位）。
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Spec.Replicas = ptr.To[int32](3)
		dep.Annotations = map[string]string{"team.example.com/owner": "alice"}
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())

		reconcileOnce()

		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
		Expect(dep.Annotations).To(HaveKeyWithValue("team.example.com/owner", "alice"))

		var managers []string
		for _, mf := range dep.ManagedFields {
			if mf.Operation == metav1.ManagedFieldsOperationApply {
				managers = append(managers, mf.Manager)
			}
		}
		Expect(managers).To(ConsistOf(string(fieldOwner)))
	})

	It("reports Available and emits a Normal event once replicas are ready", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		// envtest 沒有 deployment controller，自己扮演它：把 Deployment status 寫成「全部就緒」。
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		}
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(md.Status.Conditions, conditionAvailable)).To(BeTrue())
		Expect(recorder.Events).To(Receive(Equal("Normal Available 1/1 replicas ready")))

		// 再跑一次，狀態沒有轉變 → 不應該再發 Event。
		reconcileOnce()
		Expect(recorder.Events).NotTo(Receive())
	})

	// Phase 2 實驗 2 的回歸測試：別人改了 Deployment 的 annotation，Available 不應該閃一下。
	It("keeps Available when only the Deployment's annotations change", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		}
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		reconcileOnce()
		Expect(recorder.Events).To(Receive(Equal("Normal Available 1/1 replicas ready")))

		// 模擬 kubectl annotate deploy：Deployment 的 annotation 一變，generation 也會 +1。
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		genBefore := dep.Generation
		dep.Annotations = map[string]string{"lab/poke": "1"}
		Expect(k8sClient.Update(ctx, dep)).To(Succeed())
		Expect(dep.Generation).To(Equal(genBefore + 1))

		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(md.Status.Conditions, conditionAvailable)).To(BeTrue())
		Expect(recorder.Events).NotTo(Receive())
	})

	// Phase 2 實驗 4 的回歸測試：Deployment 被刪掉重建，不能沿用舊的 Available=True。
	It("reports Progressing when the Deployment is recreated", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce()

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		}
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		reconcileOnce()
		Expect(recorder.Events).To(Receive(Equal("Normal Available 1/1 replicas ready")))

		// 模擬 kubectl delete deploy（envtest 沒有 GC，刪除會立刻生效）。
		Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		reconcileOnce()

		md := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		a := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(a.Status).To(Equal(metav1.ConditionFalse))
		Expect(a.Reason).To(Equal("Progressing"))
		Expect(recorder.Events).To(Receive(HavePrefix("Warning Unavailable")))
	})

	// Phase 2 實驗 4、5 的回歸測試：cache 舊的時候，不能把同一個轉變再寫一次、再發一次 Event。
	It("does not repeat a transition when it reads a stale ModelDeployment", func() {
		Expect(k8sClient.Create(ctx, newMD())).To(Succeed())
		reconcileOnce() // Available=False（Progressing）

		// 先留一份「舊的」md：status 還是 False，resourceVersion 也是舊的。
		stale := &servingv1alpha1.ModelDeployment{}
		Expect(k8sClient.Get(ctx, key, stale)).To(Succeed())

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
		dep.Status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		}
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		reconcileOnce() // False → True，發一次 Event
		Expect(recorder.Events).To(Receive(Equal("Normal Available 1/1 replicas ready")))

		// 模擬 informer cache 還沒收到剛才的 status 寫入：Get md 時回傳舊快照。
		r := &ModelDeploymentReconciler{
			Client:   &staleReader{Client: k8sClient, stale: stale},
			Scheme:   k8sClient.Scheme(),
			Recorder: recorder,
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})

		Expect(apierrors.IsConflict(err)).To(BeTrue(), "got: %v", err) // 樂觀鎖擋下根據舊資料做的寫入
		Expect(recorder.Events).NotTo(Receive())                       // 沒有重複的 Event
	})

	It("reports Degraded when the model download fails", func() {
		md := newMD()
		Expect(k8sClient.Create(ctx, md)).To(Succeed())

		// 扮演 kubelet：做一個 init container 以 exit 22（curl -f 遇到 HTTP 4xx）結束的 pod。
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-abc", Namespace: ns, Labels: labelsFor(md)},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: fetchContainerName, Image: modelFetcherImage}},
				Containers:     []corev1.Container{{Name: "llama-server", Image: defaultLlamaCppImage}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:         fetchContainerName,
			Image:        modelFetcherImage,
			RestartCount: 2,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 22, Reason: "Error"},
			},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		reconcileOnce()

		Expect(k8sClient.Get(ctx, key, md)).To(Succeed())
		d := meta.FindStatusCondition(md.Status.Conditions, conditionDegraded)
		Expect(d).NotTo(BeNil())
		Expect(d.Status).To(Equal(metav1.ConditionTrue))
		Expect(d.Reason).To(Equal("ModelDownloadFailed"))
		Expect(d.Message).To(ContainSubstring("exited with code 22"))
		a := meta.FindStatusCondition(md.Status.Conditions, conditionAvailable)
		Expect(a.Reason).To(Equal("ModelDownloadFailed"))
		Expect(recorder.Events).To(Receive(HavePrefix("Warning ModelDownloadFailed")))
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
		Entry("memory request above limit", func(md *servingv1alpha1.ModelDeployment) {
			md.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			}
		}),
		Entry("cpu request above limit, in different units", func(md *servingv1alpha1.ModelDeployment) {
			md.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
			}
		}),
	)

	It("accepts a request equal to its limit even when the units differ", func() {
		md := newMD()
		md.Spec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1024Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		}
		Expect(k8sClient.Create(ctx, md)).To(Succeed())
	})
})

// staleReader 模擬 informer cache 落後：Get ModelDeployment 時回傳一份舊的快照，其他照常。
type staleReader struct {
	client.Client
	stale *servingv1alpha1.ModelDeployment
}

func (s *staleReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if md, ok := obj.(*servingv1alpha1.ModelDeployment); ok {
		s.stale.DeepCopyInto(md)
		return nil
	}
	return s.Client.Get(ctx, key, obj, opts...)
}

// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

// The agents.x-k8s.io/sandbox-uid Pod label selects one Sandbox's Pod without
// the collision risk of the 32-bit name hash. These tests cover how the
// controller owns it: stamped at Pod creation, never taken from the
// PodTemplate, restored when changed or removed, backfilled on Pods that
// predate it, stable across warm pool adoption, and kept out of the Service
// selector and the Sandbox spec.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

const (
	uidLabelNamespace = "uid-ns"
	uidLabelSandbox   = "uid-sandbox"
	// spoofedSandboxUID stands in for another tenant's Sandbox UID.
	spoofedSandboxUID = "11111111-2222-3333-4444-555555555555"
)

// uidLabelPodWrites counts Pod writes, so a test can tell a metadata patch
// from a Pod that was deleted and created again.
type uidLabelPodWrites struct {
	creates, patches, deletes int
}

func newUIDLabelReconciler(writes *uidLabelPodWrites, objs ...runtime.Object) *SandboxReconciler {
	cl := fake.NewClientBuilder().
		WithScheme(Scheme).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithIndex(&corev1.Pod{}, podSandboxNameHashIndex, podSandboxNameHashIndexer).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					writes.creates++
				}
				return c.Create(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					writes.patches++
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					writes.deletes++
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		WithRuntimeObjects(objs...).
		Build()
	return &SandboxReconciler{
		Client:        cl,
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}
}

func uidLabelSandboxFixture(templateLabels map[string]string) *sandboxv1beta1.Sandbox {
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       uidLabelSandbox,
			Namespace:  uidLabelNamespace,
			UID:        sandboxUID,
			Generation: 1,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				Service: new(true),
				PodTemplate: sandboxv1beta1.PodTemplate{
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: templateLabels},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name:  "workspace",
						Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
					}}},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
}

// uidLabelExistingPod is a running Pod owned by the fixture Sandbox, as a
// controller that predates the UID label left it.
func uidLabelExistingPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      uidLabelSandbox,
			Namespace: uidLabelNamespace,
			Labels: map[string]string{
				sandboxLabel: NameHash(uidLabelSandbox),
				"app":        "workspace",
			},
			Annotations: map[string]string{
				sandboxv1beta1.SandboxPropagatedLabelsAnnotation:      "app",
				sandboxv1beta1.SandboxPropagatedAnnotationsAnnotation: "",
			},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(uidLabelSandbox)},
		},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "workspace"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIPs:     []corev1.PodIP{{IP: "10.0.0.7"}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func reconcileUIDLabelSandbox(t *testing.T, r *SandboxReconciler) {
	t.Helper()
	_, err := r.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: uidLabelSandbox, Namespace: uidLabelNamespace},
	})
	require.NoError(t, err)
}

func getUIDLabelPod(t *testing.T, r *SandboxReconciler) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: uidLabelSandbox, Namespace: uidLabelNamespace}, pod))
	return pod
}

func getUIDLabelSandbox(t *testing.T, r *SandboxReconciler) *sandboxv1beta1.Sandbox {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: uidLabelSandbox, Namespace: uidLabelNamespace}, sb))
	return sb
}

func TestSandboxUIDLabelStampedOnCreatedPod(t *testing.T) {
	// Consumers select on the literal key, so it must not drift.
	require.Equal(t, "agents.x-k8s.io/sandbox-uid", sandboxv1beta1.SandboxUIDLabel)

	sandbox := uidLabelSandboxFixture(map[string]string{"app": "workspace"})
	writes := &uidLabelPodWrites{}
	r := newUIDLabelReconciler(writes, sandbox.DeepCopy())

	reconcileUIDLabelSandbox(t, r)

	pod := getUIDLabelPod(t, r)
	require.Equal(t, 1, writes.creates)
	require.Zero(t, writes.patches, "the label is part of the created Pod, not a follow-up patch")
	require.Equal(t, string(sandboxUID), pod.Labels[sandboxv1beta1.SandboxUIDLabel])
	require.Equal(t, "app", pod.Annotations[sandboxv1beta1.SandboxPropagatedLabelsAnnotation],
		"the UID label is controller-owned, not a propagated template label")

	// The Service and the Sandbox's scale selector keep selecting by name hash only.
	nameHash := NameHash(uidLabelSandbox)
	svc := &corev1.Service{}
	require.NoError(t, r.Get(t.Context(), types.NamespacedName{Name: uidLabelSandbox, Namespace: uidLabelNamespace}, svc))
	require.Equal(t, map[string]string{sandboxLabel: nameHash}, svc.Spec.Selector)
	require.NotContains(t, svc.Labels, sandboxv1beta1.SandboxUIDLabel)
	live := getUIDLabelSandbox(t, r)
	require.Equal(t, sandboxLabel+"="+nameHash, live.Status.LabelSelector)

	// The label lives only on the Pod. The Sandbox spec, which warm pools hash
	// to detect template revisions, is untouched.
	require.Equal(t, sandbox.Spec, live.Spec)
	require.NotContains(t, live.Labels, sandboxv1beta1.SandboxUIDLabel)

	// A second pass finds nothing to change.
	reconcileUIDLabelSandbox(t, r)
	require.Zero(t, writes.patches)
}

func TestSandboxUIDLabelIgnoresTemplateValue(t *testing.T) {
	spoofingTemplate := map[string]string{
		"app":                          "workspace",
		sandboxv1beta1.SandboxUIDLabel: spoofedSandboxUID,
	}

	t.Run("new Pod", func(t *testing.T) {
		writes := &uidLabelPodWrites{}
		r := newUIDLabelReconciler(writes, uidLabelSandboxFixture(spoofingTemplate))

		reconcileUIDLabelSandbox(t, r)

		pod := getUIDLabelPod(t, r)
		require.Equal(t, string(sandboxUID), pod.Labels[sandboxv1beta1.SandboxUIDLabel])
		require.Equal(t, "app", pod.Annotations[sandboxv1beta1.SandboxPropagatedLabelsAnnotation])
	})

	t.Run("existing Pod", func(t *testing.T) {
		pod := uidLabelExistingPod()
		pod.Labels[sandboxv1beta1.SandboxUIDLabel] = string(sandboxUID)
		writes := &uidLabelPodWrites{}
		r := newUIDLabelReconciler(writes, uidLabelSandboxFixture(spoofingTemplate), pod)

		reconcileUIDLabelSandbox(t, r)
		reconcileUIDLabelSandbox(t, r)

		require.Equal(t, string(sandboxUID), getUIDLabelPod(t, r).Labels[sandboxv1beta1.SandboxUIDLabel])
		require.Zero(t, writes.patches, "a template value must not make the controller rewrite the label")
	})
}

func TestSandboxUIDLabelRestoredWhenTampered(t *testing.T) {
	testCases := []struct {
		name   string
		tamper func(pod *corev1.Pod)
	}{
		{
			name: "changed to another Sandbox's UID",
			tamper: func(pod *corev1.Pod) {
				pod.Labels[sandboxv1beta1.SandboxUIDLabel] = spoofedSandboxUID
			},
		},
		{
			name: "emptied",
			tamper: func(pod *corev1.Pod) {
				pod.Labels[sandboxv1beta1.SandboxUIDLabel] = ""
			},
		},
		{
			name:   "removed",
			tamper: func(pod *corev1.Pod) { delete(pod.Labels, sandboxv1beta1.SandboxUIDLabel) },
		},
		{
			// An older controller that copied reserved template labels recorded
			// them as propagated, and cleanup scrubs such keys. It must not
			// scrub the controller's own UID label.
			name: "recorded as a propagated template label",
			tamper: func(pod *corev1.Pod) {
				pod.Labels[sandboxv1beta1.SandboxUIDLabel] = spoofedSandboxUID
				pod.Annotations[sandboxv1beta1.SandboxPropagatedLabelsAnnotation] = "agents.x-k8s.io/sandbox-uid,app"
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := uidLabelExistingPod()
			pod.Labels[sandboxv1beta1.SandboxUIDLabel] = string(sandboxUID)
			tc.tamper(pod)
			writes := &uidLabelPodWrites{}
			r := newUIDLabelReconciler(writes, uidLabelSandboxFixture(map[string]string{"app": "workspace"}), pod)

			reconcileUIDLabelSandbox(t, r)

			got := getUIDLabelPod(t, r)
			require.Equal(t, string(sandboxUID), got.Labels[sandboxv1beta1.SandboxUIDLabel])
			require.Equal(t, "app", got.Annotations[sandboxv1beta1.SandboxPropagatedLabelsAnnotation])
			require.Equal(t, uidLabelPodWrites{patches: 1}, *writes, "restored in place by one metadata patch")

			reconcileUIDLabelSandbox(t, r)
			require.Equal(t, uidLabelPodWrites{patches: 1}, *writes, "restored state is stable")
		})
	}
}

func TestSandboxUIDLabelBackfilledOnExistingPod(t *testing.T) {
	pod := uidLabelExistingPod()
	require.NotContains(t, pod.Labels, sandboxv1beta1.SandboxUIDLabel)
	writes := &uidLabelPodWrites{}
	r := newUIDLabelReconciler(writes, uidLabelSandboxFixture(map[string]string{"app": "workspace"}), pod)

	reconcileUIDLabelSandbox(t, r)

	got := getUIDLabelPod(t, r)
	require.Equal(t, string(sandboxUID), got.Labels[sandboxv1beta1.SandboxUIDLabel])
	require.Equal(t, uidLabelPodWrites{patches: 1}, *writes, "backfilled by a metadata patch; the Pod is not replaced")
	require.Equal(t, pod.Spec.NodeName, got.Spec.NodeName)
	require.Equal(t, pod.Status.PodIPs, got.Status.PodIPs)
	require.Equal(t, []string{"10.0.0.7"}, getUIDLabelSandbox(t, r).Status.PodIPs)
}

// TestSandboxUIDLabelStableAcrossWarmPoolAdoption follows a warm pool member
// through a claim's adoption. Adoption rewrites the existing Sandbox's owner,
// labels and PodTemplate but keeps the object, so its Pod keeps the UID label
// it was born with while the claim's metadata is patched onto it.
func TestSandboxUIDLabelStableAcrossWarmPoolAdoption(t *testing.T) {
	const (
		poolUID  = "pool-uid"
		claimUID = "claim-uid"
		envLabel = "sandbox.users.io/environment-id"
	)
	member := uidLabelSandboxFixture(map[string]string{
		"app": "workspace",
		// The warm pool also writes its tracking labels into the PodTemplate.
		sandboxv1beta1.SandboxWarmPoolLabel:     NameHash("pool"),
		sandboxv1beta1.SandboxTemplateHashLabel: "blueprint-hash",
	})
	member.Labels = map[string]string{
		sandboxv1beta1.SandboxWarmPoolLabel:     NameHash("pool"),
		sandboxv1beta1.SandboxTemplateHashLabel: "blueprint-hash",
		sandboxv1beta1.SandboxLaunchTypeLabel:   sandboxv1beta1.SandboxLaunchTypeWarm,
	}
	member.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: extensionsv1beta1.GroupVersion.String(),
		Kind:       extensionsv1beta1.SandboxWarmPoolKind,
		Name:       "pool",
		UID:        poolUID,
		Controller: new(true),
	}}
	writes := &uidLabelPodWrites{}
	r := newUIDLabelReconciler(writes, member)

	reconcileUIDLabelSandbox(t, r)

	warmPod := getUIDLabelPod(t, r)
	require.Equal(t, string(sandboxUID), warmPod.Labels[sandboxv1beta1.SandboxUIDLabel], "a warm member is born with the label")
	require.Equal(t, NameHash("pool"), warmPod.Labels[sandboxv1beta1.SandboxWarmPoolLabel])

	// Adopt the member as SandboxClaimReconciler.completeAdoption does.
	adopted := getUIDLabelSandbox(t, r)
	delete(adopted.Labels, sandboxv1beta1.SandboxWarmPoolLabel)
	delete(adopted.Labels, sandboxv1beta1.SandboxTemplateHashLabel)
	adopted.Labels[extensionsv1beta1.SandboxIDLabel] = claimUID
	adopted.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: extensionsv1beta1.GroupVersion.String(),
		Kind:       extensionsv1beta1.SandboxClaimKind,
		Name:       "claim",
		UID:        claimUID,
		Controller: new(true),
	}}
	adopted.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1beta1.SandboxIDLabel] = claimUID
	adopted.Spec.PodTemplate.ObjectMeta.Labels[envLabel] = "env-1"
	require.NoError(t, r.Update(t.Context(), adopted))

	reconcileUIDLabelSandbox(t, r)

	claimedPod := getUIDLabelPod(t, r)
	require.Equal(t, string(sandboxUID), claimedPod.Labels[sandboxv1beta1.SandboxUIDLabel], "adoption must not change the label")
	require.Equal(t, claimUID, claimedPod.Labels[extensionsv1beta1.SandboxIDLabel])
	require.Equal(t, "env-1", claimedPod.Labels[envLabel])
	require.NotContains(t, claimedPod.Labels, sandboxv1beta1.SandboxWarmPoolLabel)
	require.Equal(t, uidLabelPodWrites{creates: 1, patches: 1}, *writes, "the same Pod is patched, never replaced")
}

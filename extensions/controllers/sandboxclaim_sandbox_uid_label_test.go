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

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// TestWarmPoolAdoptionKeepsSandboxUIDPodLabel runs the Sandbox and
// SandboxClaim controllers over one warm pool member. A claim adopts the
// member Sandbox itself, so the member's Pod keeps the
// agents.x-k8s.io/sandbox-uid label it was born with, while the Sandbox
// controller patches the claim's metadata onto the same Pod.
func TestWarmPoolAdoptionKeepsSandboxUIDPodLabel(t *testing.T) {
	const (
		namespace = "default"
		envLabel  = "sandbox.users.io/environment-id"
	)
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "uid-template", Namespace: namespace},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"app": "workspace"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Image: "image-v1"}}},
		}}},
	}
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "uid-pool", Namespace: namespace, UID: "uid-pool-uid"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "uid-claim", Namespace: namespace, UID: "uid-claim-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef:           extensionsv1beta1.SandboxWarmPoolRef{Name: pool.Name},
			AdditionalPodMetadata: sandboxv1beta1.PodMetadata{Labels: map[string]string{envLabel: "env-1"}},
		},
	}

	// Build the member exactly as the warm pool controller does.
	podHash, err := computePodTemplateHash(template)
	require.NoError(t, err)
	blueprintHash, err := computeSandboxBlueprintHash(template)
	require.NoError(t, err)
	poolReconciler := &SandboxWarmPoolReconciler{Scheme: newTestScheme()}
	member, err := poolReconciler.buildSandboxCR(pool, sandboxcontrollers.NameHash(pool.Name), template, podHash, blueprintHash)
	require.NoError(t, err)
	member.GenerateName = ""
	member.Name = "uid-pool-member"
	member.UID = "uid-pool-member-uid"
	member.Generation = 1

	scheme := newScheme(t)
	// Mirrors the Pod index the Sandbox controller registers in SetupWithManager.
	podNameHashIndex := ".metadata.labels[" + sandboxcontrollers.SandboxNameHashLabel + "]"
	var podCreates, podDeletes int
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, pool, member, claim).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}, &extensionsv1beta1.SandboxClaim{}).
		WithIndex(&corev1.Pod{}, podNameHashIndex, func(obj client.Object) []string {
			if v, ok := obj.GetLabels()[sandboxcontrollers.SandboxNameHashLabel]; ok {
				return []string{v}
			}
			return nil
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					podCreates++
				}
				return c.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					podDeletes++
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	sandboxReconciler := &sandboxcontrollers.SandboxReconciler{
		Client:        c,
		Scheme:        scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}
	memberKey := types.NamespacedName{Namespace: namespace, Name: member.Name}
	reconcileMember := func(t *testing.T) {
		t.Helper()
		_, err := sandboxReconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: memberKey})
		require.NoError(t, err)
	}
	getPod := func(t *testing.T) *corev1.Pod {
		t.Helper()
		pod := &corev1.Pod{}
		require.NoError(t, c.Get(t.Context(), memberKey, pod))
		return pod
	}

	// The member's Pod is born with the label, then becomes Ready.
	reconcileMember(t)
	pod := getPod(t)
	require.Equal(t, string(member.UID), pod.Labels[sandboxv1beta1.SandboxUIDLabel])
	pod.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		PodIPs:     []corev1.PodIP{{IP: testNetworkedPodIP}},
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
	require.NoError(t, c.Status().Update(t.Context(), pod))
	reconcileMember(t)

	// The claim adopts the member.
	q := queue.NewSimpleSandboxQueue()
	q.Add(queue.GetNamespacedWarmPoolName(namespace, pool.Name), queue.SandboxKey{Namespace: namespace, Name: member.Name})
	claimReconciler := &SandboxClaimReconciler{
		Client:           c,
		Scheme:           scheme,
		Recorder:         events.NewFakeRecorder(10),
		WarmSandboxQueue: q,
		Tracer:           asmetrics.NewNoOp(),
	}
	_, err = claimReconciler.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: claim.Name}})
	require.NoError(t, err)
	adopted := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(t.Context(), memberKey, adopted))
	require.True(t, metav1.IsControlledBy(adopted, claim), "the claim adopted the member")
	require.Equal(t, member.UID, adopted.UID)

	// The Sandbox controller patches the claim's metadata onto the same Pod.
	reconcileMember(t)
	claimedPod := getPod(t)
	require.Equal(t, string(member.UID), claimedPod.Labels[sandboxv1beta1.SandboxUIDLabel], "adoption must not change the label")
	require.NotEqual(t, string(claim.UID), claimedPod.Labels[sandboxv1beta1.SandboxUIDLabel])
	require.Equal(t, string(claim.UID), claimedPod.Labels[extensionsv1beta1.SandboxIDLabel])
	require.Equal(t, "env-1", claimedPod.Labels[envLabel])
	require.NotContains(t, claimedPod.Labels, sandboxv1beta1.SandboxWarmPoolLabel)
	require.Equal(t, []int{1, 0}, []int{podCreates, podDeletes}, "the Pod was patched, not replaced")
	require.Equal(t, []corev1.PodIP{{IP: testNetworkedPodIP}}, claimedPod.Status.PodIPs)
}

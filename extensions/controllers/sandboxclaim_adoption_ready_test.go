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
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
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

const adoptionReadyNamespace = "default"

func adoptionReadyFixtures() (*extensionsv1beta1.SandboxTemplate, *extensionsv1beta1.SandboxWarmPool, *extensionsv1beta1.SandboxClaim) {
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-template", Namespace: adoptionReadyNamespace},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"app": "ready"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Image: "image-v1"}}},
		}}},
	}
	pool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-pool", Namespace: adoptionReadyNamespace, UID: "ready-pool-uid"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name}},
	}
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-claim", Namespace: adoptionReadyNamespace, UID: "ready-claim-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: pool.Name},
			AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{"sandbox.users.io/environment-id": "env-1"},
			},
		},
	}
	return template, pool, claim
}

// buildReadyWarmMember builds a Ready, networked member exactly as the warm
// pool controller does from template (secure defaults, revision labels,
// owner), at generation 1.
func buildReadyWarmMember(t *testing.T, pool *extensionsv1beta1.SandboxWarmPool, template *extensionsv1beta1.SandboxTemplate, name string) *sandboxv1beta1.Sandbox {
	t.Helper()
	podHash, err := computePodTemplateHash(template)
	require.NoError(t, err)
	blueprintHash, err := computeSandboxBlueprintHash(template)
	require.NoError(t, err)
	builder := &SandboxWarmPoolReconciler{Scheme: newTestScheme()}
	sb, err := builder.buildSandboxCR(pool, sandboxcontrollers.NameHash(pool.Name), template, podHash, blueprintHash)
	require.NoError(t, err)
	sb.GenerateName = ""
	sb.Name = name
	sb.UID = types.UID(name + "-uid")
	sb.Generation = 1
	sb.Status = sandboxv1beta1.SandboxStatus{
		PodIPs: []string{testNetworkedPodIP},
		Conditions: []metav1.Condition{{
			Type:               string(sandboxv1beta1.SandboxConditionReady),
			Status:             metav1.ConditionTrue,
			Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
			ObservedGeneration: 1,
		}},
	}
	return sb
}

// bumpGenerationOnSandboxSpecPatch emulates the API server: a patch that
// changes a Sandbox's spec increments metadata.generation. The fake client
// does not.
func bumpGenerationOnSandboxSpecPatch() interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			before, isSandbox := obj.(*sandboxv1beta1.Sandbox)
			if !isSandbox {
				return c.Patch(ctx, obj, patch, opts...)
			}
			stored := &sandboxv1beta1.Sandbox{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(before), stored); err != nil {
				return err
			}
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			after := obj.(*sandboxv1beta1.Sandbox)
			if equality.Semantic.DeepEqual(stored.Spec, after.Spec) {
				return nil
			}
			after.Generation = stored.Generation + 1
			return c.Update(ctx, after)
		},
	}
}

func claimReadyCondition(t *testing.T, claim *extensionsv1beta1.SandboxClaim) metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(claim.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	require.NotNil(t, cond)
	return *cond
}

// TestSandboxClaimReadyWaitsForAdoptedPodMetadata: adoption writes the
// claim's identity labels and additionalPodMetadata into the member's
// PodTemplate (a new generation). The warm member's Ready=True condition still
// describes the previous generation, whose Pod lacks that metadata, so the
// claim must not report Ready until the Sandbox controller reports Ready for
// the new generation, which it does only after patching the Pod.
func TestSandboxClaimReadyWaitsForAdoptedPodMetadata(t *testing.T) {
	template, pool, claim := adoptionReadyFixtures()
	member := buildReadyWarmMember(t, pool, template, "member")

	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(template, pool, member, claim).
		WithStatusSubresource(claim).
		WithInterceptorFuncs(bumpGenerationOnSandboxSpecPatch()).
		Build()
	q := queue.NewSimpleSandboxQueue()
	q.Add(queue.GetNamespacedWarmPoolName(adoptionReadyNamespace, pool.Name), queue.SandboxKey{Namespace: adoptionReadyNamespace, Name: member.Name})
	r := &SandboxClaimReconciler{
		Client:           c,
		Scheme:           newScheme(t),
		Recorder:         events.NewFakeRecorder(10),
		WarmSandboxQueue: q,
		Tracer:           asmetrics.NewNoOp(),
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}}
	reconcileClaim := func(t *testing.T) *extensionsv1beta1.SandboxClaim {
		t.Helper()
		_, err := r.Reconcile(t.Context(), req)
		require.NoError(t, err)
		got := &extensionsv1beta1.SandboxClaim{}
		require.NoError(t, c.Get(t.Context(), req.NamespacedName, got))
		return got
	}
	getSandbox := func(t *testing.T) *sandboxv1beta1.Sandbox {
		t.Helper()
		sb := &sandboxv1beta1.Sandbox{}
		require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: adoptionReadyNamespace, Name: member.Name}, sb))
		return sb
	}

	got := reconcileClaim(t)

	adopted := getSandbox(t)
	require.True(t, metav1.IsControlledBy(adopted, got))
	require.Equal(t, "env-1", adopted.Spec.PodTemplate.ObjectMeta.Labels["sandbox.users.io/environment-id"])
	require.Equal(t, string(got.UID), adopted.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1beta1.SandboxIDLabel])
	require.Equal(t, int64(2), adopted.Generation, "adoption changes the PodTemplate")
	ready := claimReadyCondition(t, got)
	require.Equal(t, metav1.ConditionFalse, ready.Status, "claim must not be Ready before the Pod carries its metadata")
	require.Equal(t, ClaimReasonSandboxUpdatePending, ready.Reason)
	require.Equal(t, member.Name, got.Status.SandboxStatus.Name, "the binding is still published while readiness is pending")

	// A pass before the Sandbox controller catches up stays pending.
	got = reconcileClaim(t)
	require.Equal(t, ClaimReasonSandboxUpdatePending, claimReadyCondition(t, got).Reason)

	// The Sandbox controller patches the Pod metadata, then reports Ready for
	// the new generation.
	adopted = getSandbox(t)
	meta.SetStatusCondition(&adopted.Status.Conditions, metav1.Condition{
		Type:               string(sandboxv1beta1.SandboxConditionReady),
		Status:             metav1.ConditionTrue,
		Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
		ObservedGeneration: adopted.Generation,
	})
	require.NoError(t, c.Update(t.Context(), adopted))

	got = reconcileClaim(t)
	ready = claimReadyCondition(t, got)
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	require.Equal(t, sandboxv1beta1.SandboxReasonDependenciesReady, ready.Reason)
	require.Equal(t, got.Generation, ready.ObservedGeneration, "a forwarded Ready condition carries the claim's generation")
}

func TestSandboxUpdatePendingCondition(t *testing.T) {
	claim := &extensionsv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: "c", Generation: 4}}
	sandbox := &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "s", Generation: 3}}
	readyAt := func(status metav1.ConditionStatus, observed int64) metav1.Condition {
		return metav1.Condition{Type: string(sandboxv1beta1.SandboxConditionReady), Status: status, Reason: "R", ObservedGeneration: observed}
	}

	pending, ok := sandboxUpdatePendingCondition(claim, sandbox, readyAt(metav1.ConditionTrue, 2))
	require.True(t, ok, "Ready for an older generation is pending")
	require.Equal(t, metav1.ConditionFalse, pending.Status)
	require.Equal(t, ClaimReasonSandboxUpdatePending, pending.Reason)
	require.Equal(t, int64(4), pending.ObservedGeneration)

	_, ok = sandboxUpdatePendingCondition(claim, sandbox, readyAt(metav1.ConditionTrue, 3))
	require.False(t, ok, "Ready for the current generation is forwarded")

	_, ok = sandboxUpdatePendingCondition(claim, sandbox, readyAt(metav1.ConditionFalse, 2))
	require.False(t, ok, "a not-Ready condition is forwarded unchanged")
}

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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

const revisionTestNamespace = "default"

func revisionTestTemplate(image string) *extensionsv1beta1.SandboxTemplate {
	return &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "rev-template", Namespace: revisionTestNamespace},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{"app": "rev"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "workspace", Image: image}}},
		}}},
	}
}

func revisionTestPool() *extensionsv1beta1.SandboxWarmPool {
	return &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "rev-pool", Namespace: revisionTestNamespace, UID: "rev-pool-uid"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "rev-template"}},
	}
}

func revisionTestClaim() *extensionsv1beta1.SandboxClaim {
	return &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "rev-claim", Namespace: revisionTestNamespace, UID: "rev-claim-uid"},
		Spec: extensionsv1beta1.SandboxClaimSpec{
			WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "rev-pool"},
			AdditionalPodMetadata: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{"sandbox.users.io/environment-id": "env-1"},
			},
		},
	}
}

// buildWarmMember builds a Ready, networked member exactly as the warm pool
// controller does from template (secure defaults, revision labels, owner).
func buildWarmMember(t *testing.T, pool *extensionsv1beta1.SandboxWarmPool, template *extensionsv1beta1.SandboxTemplate, name string) *sandboxv1beta1.Sandbox {
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

type revisionHarness struct {
	client     client.Client
	reconciler *SandboxClaimReconciler
	queue      queue.SandboxQueue
	req        reconcile.Request
}

func newRevisionHarness(t *testing.T, claim *extensionsv1beta1.SandboxClaim, objs []client.Object, queued []*sandboxv1beta1.Sandbox) *revisionHarness {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(append(objs, claim)...).WithStatusSubresource(claim).Build()
	q := queue.NewSimpleSandboxQueue()
	for _, sb := range queued {
		q.Add(queue.GetNamespacedWarmPoolName(sb.Namespace, claim.Spec.WarmPoolRef.Name), queue.SandboxKey{Namespace: sb.Namespace, Name: sb.Name})
	}
	return &revisionHarness{
		client: c,
		reconciler: &SandboxClaimReconciler{
			Client:           c,
			Scheme:           newScheme(t),
			Recorder:         events.NewFakeRecorder(10),
			WarmSandboxQueue: q,
			Tracer:           asmetrics.NewNoOp(),
		},
		queue: q,
		req:   reconcile.Request{NamespacedName: types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}},
	}
}

func (h *revisionHarness) reconcile(t *testing.T) *extensionsv1beta1.SandboxClaim {
	t.Helper()
	_, err := h.reconciler.Reconcile(t.Context(), h.req)
	require.NoError(t, err)
	claim := &extensionsv1beta1.SandboxClaim{}
	require.NoError(t, h.client.Get(t.Context(), h.req.NamespacedName, claim))
	return claim
}

func (h *revisionHarness) sandbox(t *testing.T, name string) *sandboxv1beta1.Sandbox {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	require.NoError(t, h.client.Get(t.Context(), types.NamespacedName{Namespace: revisionTestNamespace, Name: name}, sb))
	return sb
}

func (h *revisionHarness) queuedNames() []string {
	var names []string
	for {
		key, ok := h.queue.Get(queue.GetNamespacedWarmPoolName(revisionTestNamespace, "rev-pool"))
		if !ok {
			return names
		}
		names = append(names, key.Name)
	}
}

func requirePoolOwned(t *testing.T, sb *sandboxv1beta1.Sandbox, pool *extensionsv1beta1.SandboxWarmPool) {
	t.Helper()
	ref := metav1.GetControllerOf(sb)
	require.NotNil(t, ref)
	require.Equal(t, pool.UID, ref.UID, "member %s must stay with the pool", sb.Name)
	require.Contains(t, sb.Labels, warmPoolSandboxLabel)
}

// TestSandboxClaimNeverAdoptsStaleRevision: under the default OnReplenish
// strategy a pool can still hold members built from an older template. A
// claim must adopt only a member built from the current revision, drop stale
// members from its queue for good, and fall back to a cold start rather than
// hand one out.
func TestSandboxClaimNeverAdoptsStaleRevision(t *testing.T) {
	pool := revisionTestPool()
	oldTemplate := revisionTestTemplate("image-v1")
	current := revisionTestTemplate("image-v2")

	t.Run("adopts the current member and skips the older stale one", func(t *testing.T) {
		stale := buildWarmMember(t, pool, oldTemplate, "stale-member")
		fresh := buildWarmMember(t, pool, current, "fresh-member")
		h := newRevisionHarness(t, revisionTestClaim(), []client.Object{current, pool, stale, fresh}, []*sandboxv1beta1.Sandbox{stale, fresh})

		claim := h.reconcile(t)

		require.Equal(t, "fresh-member", claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
		require.True(t, metav1.IsControlledBy(h.sandbox(t, "fresh-member"), claim))
		requirePoolOwned(t, h.sandbox(t, "stale-member"), pool)
		require.Empty(t, h.queuedNames(), "the stale member must not be offered again")
	})

	t.Run("cold starts when every member is stale", func(t *testing.T) {
		stale := buildWarmMember(t, pool, oldTemplate, "stale-member")
		h := newRevisionHarness(t, revisionTestClaim(), []client.Object{current, pool, stale}, []*sandboxv1beta1.Sandbox{stale})

		claim := h.reconcile(t)

		require.Empty(t, claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
		cold := h.sandbox(t, claim.Name)
		require.True(t, metav1.IsControlledBy(cold, claim))
		require.Equal(t, "image-v2", cold.Spec.PodTemplate.Spec.Containers[0].Image)
		requirePoolOwned(t, h.sandbox(t, "stale-member"), pool)
	})

	t.Run("a member of a template the pool no longer references is stale", func(t *testing.T) {
		// Identical blueprint, but built while the pool referenced another
		// template: only the template reference differs.
		otherTemplate := revisionTestTemplate("image-v2")
		otherTemplate.Name = "other-template"
		previousPool := pool.DeepCopy()
		previousPool.Spec.TemplateRef.Name = otherTemplate.Name
		member := buildWarmMember(t, previousPool, otherTemplate, "other-template-member")
		require.Equal(t, currentRevision(current), member.Labels[sandboxv1beta1.SandboxTemplateHashLabel])
		h := newRevisionHarness(t, revisionTestClaim(), []client.Object{current, pool, member}, []*sandboxv1beta1.Sandbox{member})

		claim := h.reconcile(t)

		require.Empty(t, claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
		requirePoolOwned(t, h.sandbox(t, "other-template-member"), pool)
	})

	t.Run("an assigned member that went stale is released, not adopted", func(t *testing.T) {
		stale := buildWarmMember(t, pool, oldTemplate, "stale-member")
		claim := revisionTestClaim()
		claim.Annotations = map[string]string{extensionsv1beta1.AssignedSandboxNameAnnotation: stale.Name}
		h := newRevisionHarness(t, claim, []client.Object{current, pool, stale}, nil)

		got := h.reconcile(t)

		require.NotEqual(t, stale.Name, got.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
		requirePoolOwned(t, h.sandbox(t, "stale-member"), pool)
	})
}

// TestStaleRevisionPredicateIsShared: a template change that only touches Pod
// metadata changes the blueprint hash but not the member's revision. The pool
// must keep the member and a claim must adopt it; if the two sides disagreed,
// the member would be neither replaced nor claimable.
func TestStaleRevisionPredicateIsShared(t *testing.T) {
	pool := revisionTestPool()
	built := revisionTestTemplate("image-v1")
	member := buildWarmMember(t, pool, built, "member")
	current := built.DeepCopy()
	current.Spec.PodTemplate.ObjectMeta.Labels["team"] = "platform"
	require.NotEqual(t, currentRevision(built), currentRevision(current), "the fixture must change the blueprint hash")

	replicas := int32(1)
	poolWithReplicas := pool.DeepCopy()
	poolWithReplicas.Spec.Replicas = &replicas
	poolReconciler := &SandboxWarmPoolReconciler{
		Client:       newFakeClient(newTestScheme(), current.DeepCopy(), poolWithReplicas.DeepCopy(), member.DeepCopy()),
		Scheme:       newTestScheme(),
		MaxBatchSize: sandboxCreateDeleteMaxBatchSize,
	}
	_, err := poolReconciler.reconcilePool(t.Context(), poolWithReplicas)
	require.NoError(t, err)
	kept := &sandboxv1beta1.Sandbox{}
	require.NoError(t, poolReconciler.Get(t.Context(), client.ObjectKeyFromObject(member), kept), "the pool must keep a member whose revision still matches")

	h := newRevisionHarness(t, revisionTestClaim(), []client.Object{current, pool, member}, []*sandboxv1beta1.Sandbox{member})
	claim := h.reconcile(t)
	require.Equal(t, member.Name, claim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation], "the claim must adopt a member the pool keeps")
}

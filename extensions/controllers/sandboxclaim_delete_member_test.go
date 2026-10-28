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
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// TestClaimDeleteNeverReturnsAdoptedMemberToPool: once a claim adopts a warm
// pool member, deleting the claim must never put that Sandbox back in the
// pool, so a pool member never already holds a binding. Adoption strips the
// pool label and makes the claim the Sandbox's only owner, so the garbage
// collector deletes it with the claim. Even if it survives (the claim is
// deleted with propagationPolicy=Orphan), the pool neither lists, re-owns
// nor counts it, the claim-side queue never offers it, and the pool creates
// a fresh replacement instead.
//
// The fake client has no garbage collector, so each case applies what the
// GC does for its propagation policy.
func TestClaimDeleteNeverReturnsAdoptedMemberToPool(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy metav1.DeletionPropagation
	}{
		{name: "background cascade deletes the adopted Sandbox", policy: metav1.DeletePropagationBackground},
		{name: "foreground cascade deletes the adopted Sandbox", policy: metav1.DeletePropagationForeground},
		{name: "orphaned Sandbox is never re-adopted by the pool", policy: metav1.DeletePropagationOrphan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			template, pool, claim := adoptionReadyFixtures()
			replicas := int32(1)
			pool.Spec.Replicas = &replicas
			member := buildReadyWarmMember(t, pool, template, "member")
			memberKey := types.NamespacedName{Namespace: adoptionReadyNamespace, Name: member.Name}
			poolKey := types.NamespacedName{Namespace: adoptionReadyNamespace, Name: pool.Name}
			queueName := queue.GetNamespacedWarmPoolName(adoptionReadyNamespace, pool.Name)

			scheme := newScheme(t)
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(template, pool, member, claim).
				WithStatusSubresource(&sandboxv1beta1.Sandbox{}, &extensionsv1beta1.SandboxClaim{}, &extensionsv1beta1.SandboxWarmPool{}).
				WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
				WithIndex(&extensionsv1beta1.SandboxWarmPool{}, extensionsv1beta1.TemplateRefField, sandboxTemplateRefNameIndexer).
				Build()
			poolReconciler := &SandboxWarmPoolReconciler{Client: c, Scheme: scheme, MaxBatchSize: sandboxCreateDeleteMaxBatchSize}
			reconcilePool := func(t *testing.T) *extensionsv1beta1.SandboxWarmPool {
				t.Helper()
				_, err := poolReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
				require.NoError(t, err)
				syncPoolExpectations(poolReconciler, pool)
				got := &extensionsv1beta1.SandboxWarmPool{}
				require.NoError(t, c.Get(ctx, poolKey, got))
				return got
			}
			poolMembers := func(t *testing.T) []sandboxv1beta1.Sandbox {
				t.Helper()
				list := &sandboxv1beta1.SandboxList{}
				require.NoError(t, c.List(ctx, list, client.InNamespace(adoptionReadyNamespace)))
				var members []sandboxv1beta1.Sandbox
				for _, sb := range list.Items {
					if metav1.IsControlledBy(&sb, pool) {
						members = append(members, sb)
					}
				}
				return members
			}

			// The pool counts the member and creates nothing.
			require.Equal(t, int32(1), reconcilePool(t).Status.Replicas)
			require.Len(t, poolMembers(t), 1)

			// The claim adopts the member.
			q := queue.NewSimpleSandboxQueue()
			q.Add(queueName, queue.SandboxKey{Namespace: adoptionReadyNamespace, Name: member.Name})
			claimReconciler := &SandboxClaimReconciler{
				Client:           c,
				Scheme:           scheme,
				Recorder:         events.NewFakeRecorder(10),
				WarmSandboxQueue: q,
				Tracer:           asmetrics.NewNoOp(),
			}
			claimReq := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}}
			_, err := claimReconciler.Reconcile(ctx, claimReq)
			require.NoError(t, err)
			adopted := &sandboxv1beta1.Sandbox{}
			require.NoError(t, c.Get(ctx, memberKey, adopted))
			require.Equal(t, member.UID, adopted.UID)
			require.True(t, metav1.IsControlledBy(adopted, claim), "the claim adopted the member")
			require.Len(t, adopted.OwnerReferences, 1, "the claim is the adopted Sandbox's only owner, so the GC deletes it with the claim")
			require.True(t, *adopted.OwnerReferences[0].BlockOwnerDeletion)
			require.NotContains(t, adopted.Labels, warmPoolSandboxLabel)
			require.NotContains(t, adopted.Spec.PodTemplate.ObjectMeta.Labels, warmPoolSandboxLabel)

			// The claim-side watch never re-offers the adopted Sandbox.
			sandboxEvents := &sandboxEventHandler{sandboxQueue: q}
			sandboxEvents.Update(ctx, event.UpdateEvent{ObjectOld: member, ObjectNew: adopted}, nil)
			_, queued := q.Get(queueName)
			require.False(t, queued, "an adopted Sandbox must not be queued for the pool")

			// Delete the claim, then apply what the GC does for the policy.
			require.NoError(t, c.Delete(ctx, claim, client.PropagationPolicy(tc.policy)))
			require.NoError(t, c.Get(ctx, memberKey, adopted))
			if tc.policy == metav1.DeletePropagationOrphan {
				var kept []metav1.OwnerReference
				for _, ref := range adopted.OwnerReferences {
					if ref.UID != claim.UID {
						kept = append(kept, ref)
					}
				}
				adopted.OwnerReferences = kept
				require.NoError(t, c.Update(ctx, adopted))
				before := member.DeepCopy()
				before.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(claim, extensionsv1beta1.GroupVersion.WithKind("SandboxClaim"))}
				sandboxEvents.Update(ctx, event.UpdateEvent{ObjectOld: before, ObjectNew: adopted}, nil)
			} else {
				require.NoError(t, c.Delete(ctx, adopted))
				sandboxEvents.Delete(ctx, event.DeleteEvent{Object: adopted}, nil)
			}

			// The deleted claim's reconcile is a no-op.
			_, err = claimReconciler.Reconcile(ctx, claimReq)
			require.NoError(t, err)

			// The pool sees no member, so it creates a fresh replacement
			// instead of taking the adopted one back.
			got := reconcilePool(t)
			require.Equal(t, int32(0), got.Status.Replicas, "the adopted Sandbox is not counted as a member")
			members := poolMembers(t)
			require.Len(t, members, 1, "the pool holds exactly one member")
			require.NotEqual(t, member.UID, members[0].UID, "the pool's member is a fresh replacement")
			require.NotEqual(t, member.Name, members[0].Name)
			got = reconcilePool(t)
			require.Equal(t, int32(1), got.Status.Replicas, "only the replacement is counted")
			require.Len(t, poolMembers(t), 1, "the pool does not grow past spec.replicas")

			survivor := &sandboxv1beta1.Sandbox{}
			err = c.Get(ctx, memberKey, survivor)
			if tc.policy != metav1.DeletePropagationOrphan {
				require.True(t, k8errors.IsNotFound(err), "the adopted Sandbox is deleted with its claim: %v", err)
				return
			}
			require.NoError(t, err)
			require.Nil(t, metav1.GetControllerOf(survivor), "the pool must not re-own an orphaned ex-member")
			require.Empty(t, survivor.OwnerReferences)
			require.NotContains(t, survivor.Labels, warmPoolSandboxLabel)
			require.Error(t, isAdoptable(survivor), "an orphaned ex-member is never adoptable")
			_, queued = q.Get(queueName)
			require.False(t, queued, "an orphaned ex-member must not be queued for the pool")

			// Even if its key reaches the queue, a new claim never binds it.
			claim2 := &extensionsv1beta1.SandboxClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "second-claim", Namespace: adoptionReadyNamespace, UID: "second-claim-uid"},
				Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: pool.Name}},
			}
			require.NoError(t, c.Create(ctx, claim2))
			q.Add(queueName, queue.SandboxKey{Namespace: adoptionReadyNamespace, Name: member.Name})
			_, err = claimReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(claim2)})
			require.NoError(t, err)
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(claim2), claim2))
			require.NotEqual(t, member.Name, claim2.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
			require.NoError(t, c.Get(ctx, memberKey, survivor))
			require.Nil(t, metav1.GetControllerOf(survivor), "a new claim must not bind the orphaned ex-member")
		})
	}
}

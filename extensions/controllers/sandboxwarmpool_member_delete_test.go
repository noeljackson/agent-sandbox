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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
)

// memberDeleteHarness models the warm pool controller reading a lagging
// informer cache: while frozen, List keeps returning the Sandboxes as they
// were when the snapshot was taken, even after a claim adopted one of them on
// the API server. Every Delete call's options are recorded.
type memberDeleteHarness struct {
	client.WithWatch

	mu       sync.Mutex
	snapshot *sandboxv1beta1.SandboxList
	deletes  []client.DeleteOptions
}

func newMemberDeleteHarness(t *testing.T, objs ...client.Object) *memberDeleteHarness {
	t.Helper()
	h := &memberDeleteHarness{}
	inner := newFakeClient(newTestScheme())
	for _, obj := range objs {
		require.NoError(t, inner.Create(t.Context(), obj))
	}
	h.WithWatch = interceptor.NewClient(inner, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			h.mu.Lock()
			snapshot := h.snapshot
			h.mu.Unlock()
			if sl, ok := list.(*sandboxv1beta1.SandboxList); ok && snapshot != nil {
				snapshot.DeepCopyInto(sl)
				return nil
			}
			return c.List(ctx, list, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			var recorded client.DeleteOptions
			recorded.ApplyOptions(opts)
			h.mu.Lock()
			h.deletes = append(h.deletes, recorded)
			h.mu.Unlock()
			return c.Delete(ctx, obj, opts...)
		},
	})
	return h
}

// freeze pins List results to the current Sandboxes (the cache stops
// observing writes).
func (h *memberDeleteHarness) freeze(t *testing.T) {
	t.Helper()
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, h.List(t.Context(), list))
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snapshot = list
}

func (h *memberDeleteHarness) deleteCalls() []client.DeleteOptions {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]client.DeleteOptions(nil), h.deletes...)
}

// adoptOnServer applies the ownership transfer the claim controller's
// completeAdoption patch makes, directly on the API server.
func adoptOnServer(t *testing.T, c client.Client, name string) {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: name}, sb))
	delete(sb.Labels, warmPoolSandboxLabel)
	delete(sb.Labels, sandboxv1beta1.SandboxTemplateHashLabel)
	sb.Labels[extensionsv1beta1.SandboxIDLabel] = "claim-uid"
	sb.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: extensionsv1beta1.GroupVersion.String(),
		Kind:       extensionsv1beta1.SandboxClaimKind,
		Name:       "claim",
		UID:        "claim-uid",
		Controller: new(true),
	}}
	require.NoError(t, c.Update(t.Context(), sb))
}

func poolOwnerRef(pool *extensionsv1beta1.SandboxWarmPool) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: extensionsv1beta1.GroupVersion.String(),
		Kind:       extensionsv1beta1.SandboxWarmPoolKind,
		Name:       pool.Name,
		UID:        pool.UID,
		Controller: new(true),
	}
}

// TestReconcilePoolNeverDeletesAdoptedMember covers every pool delete path
// (excess, stale revision, stuck) against a cache that has not yet observed a
// claim adopting the member. The delete must be refused by its preconditions,
// so the claimed Sandbox (which owns the environment's Pod and PVCs) survives.
// The control run, without the adoption, proves each scenario really reaches
// the delete.
func TestReconcilePoolNeverDeletesAdoptedMember(t *testing.T) {
	type scenario struct {
		name     string
		replicas int32
		ready    bool
		strategy *extensionsv1beta1.SandboxWarmPoolUpdateStrategy
		// arrange mutates the fixture so the pool wants to delete the member.
		arrange func(t *testing.T, r *SandboxWarmPoolReconciler, c client.Client, template *extensionsv1beta1.SandboxTemplate, member *sandboxv1beta1.Sandbox)
	}
	scenarios := []scenario{
		{
			name:     "excess member on scale down",
			replicas: 0,
			ready:    true,
		},
		{
			name:     "stale-revision member under Recreate",
			replicas: 1,
			ready:    true,
			strategy: &extensionsv1beta1.SandboxWarmPoolUpdateStrategy{Type: extensionsv1beta1.RecreateSandboxWarmPoolUpdateStrategyType},
			arrange: func(t *testing.T, _ *SandboxWarmPoolReconciler, c client.Client, template *extensionsv1beta1.SandboxTemplate, _ *sandboxv1beta1.Sandbox) {
				current := &extensionsv1beta1.SandboxTemplate{}
				require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(template), current))
				current.Spec.PodTemplate.Spec.Containers[0].Image = "image-v2"
				require.NoError(t, c.Update(t.Context(), current))
			},
		},
		{
			name:     "stuck member past the readiness grace period",
			replicas: 1,
			arrange: func(_ *testing.T, r *SandboxWarmPoolReconciler, _ client.Client, _ *extensionsv1beta1.SandboxTemplate, member *sandboxv1beta1.Sandbox) {
				r.now = func() time.Time { return member.CreationTimestamp.Add(r.readinessGracePeriod() + time.Minute) }
			},
		},
	}

	for _, sc := range scenarios {
		for _, adopted := range []bool{false, true} {
			name := sc.name + "/control"
			if adopted {
				name = sc.name + "/adopted while cache lags"
			}
			t.Run(name, func(t *testing.T) {
				zeroGraceJitter(t)
				replicas := sc.replicas
				template := createTemplate("default")
				pool := &extensionsv1beta1.SandboxWarmPool{
					ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: "pool-uid"},
					Spec: extensionsv1beta1.SandboxWarmPoolSpec{
						Replicas:       &replicas,
						TemplateRef:    extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
						UpdateStrategy: sc.strategy,
					},
				}
				member := createPoolSandbox(pool.Name, "default", sandboxcontrollers.NameHash(pool.Name), template, "-member")
				member.UID = "member-uid"
				// Members built by the pool already carry the launch-type label,
				// so the pool issues no metadata update before deciding.
				member.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
				member.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
				member.OwnerReferences = []metav1.OwnerReference{poolOwnerRef(pool)}
				readyStatus := metav1.ConditionFalse
				if sc.ready {
					readyStatus = metav1.ConditionTrue
				}
				member.Status.Conditions = []metav1.Condition{{
					Type:   string(sandboxv1beta1.SandboxConditionReady),
					Status: readyStatus,
					Reason: "Test",
				}}

				h := newMemberDeleteHarness(t, template, pool, member)
				r := &SandboxWarmPoolReconciler{Client: h, Scheme: newTestScheme(), MaxBatchSize: sandboxCreateDeleteMaxBatchSize}
				r.now = time.Now
				if sc.arrange != nil {
					sc.arrange(t, r, h, template, member)
				}

				h.freeze(t)
				if adopted {
					adoptOnServer(t, h, member.Name)
				}

				requeueAfter, err := r.reconcilePool(t.Context(), pool)
				require.NoError(t, err)

				deletes := h.deleteCalls()
				require.Len(t, deletes, 1, "the pool must attempt exactly one member delete")
				require.NotNil(t, deletes[0].Preconditions, "member deletes must carry preconditions")
				require.NotNil(t, deletes[0].Preconditions.UID)
				require.Equal(t, types.UID("member-uid"), *deletes[0].Preconditions.UID)
				require.NotNil(t, deletes[0].Preconditions.ResourceVersion)
				require.NotEmpty(t, *deletes[0].Preconditions.ResourceVersion)

				got := &sandboxv1beta1.Sandbox{}
				err = h.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: member.Name}, got)
				if !adopted {
					require.Error(t, err, "control: the unadopted member is deleted")
					return
				}
				require.NoError(t, err, "an adopted Sandbox must never be deleted by the pool")
				require.True(t, got.DeletionTimestamp.IsZero())
				require.Equal(t, types.UID("claim-uid"), metav1.GetControllerOf(got).UID)
				require.NotZero(t, requeueAfter, "a refused delete is re-evaluated from a fresh view")
				require.LessOrEqual(t, requeueAfter, memberChangedRequeueDelay)
				require.True(t, r.exp().SatisfiedExpectations(types.NamespacedName{Namespace: "default", Name: pool.Name}),
					"a refused delete must not leave a pending deletion expectation")
			})
		}
	}
}

// TestDeletePoolMemberReverifiesInHand covers the in-hand re-verification:
// a member that is visibly no longer this pool's is never sent to the API
// server.
func TestDeletePoolMemberReverifiesInHand(t *testing.T) {
	pool := &extensionsv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: "pool-uid"}}
	poolHash := sandboxcontrollers.NameHash(pool.Name)
	base := func() *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{ObjectMeta: metav1.ObjectMeta{
			Name: "member", Namespace: "default", UID: "member-uid", ResourceVersion: "7",
			Labels:          map[string]string{warmPoolSandboxLabel: poolHash},
			OwnerReferences: []metav1.OwnerReference{poolOwnerRef(pool)},
		}}
	}
	cases := map[string]func(sb *sandboxv1beta1.Sandbox){
		"adopted by a claim": func(sb *sandboxv1beta1.Sandbox) {
			sb.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: extensionsv1beta1.GroupVersion.String(), Kind: extensionsv1beta1.SandboxClaimKind,
				Name: "claim", UID: "claim-uid", Controller: new(true),
			}}
		},
		"pool label removed": func(sb *sandboxv1beta1.Sandbox) { delete(sb.Labels, warmPoolSandboxLabel) },
		"another pool's label": func(sb *sandboxv1beta1.Sandbox) {
			sb.Labels[warmPoolSandboxLabel] = sandboxcontrollers.NameHash("other-pool")
		},
		"already terminating": func(sb *sandboxv1beta1.Sandbox) {
			now := metav1.Now()
			sb.DeletionTimestamp = &now
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			sb := base()
			mutate(sb)
			h := newMemberDeleteHarness(t)
			r := &SandboxWarmPoolReconciler{Client: h, Scheme: newTestScheme()}
			outcome, err := r.deletePoolMember(t.Context(), types.NamespacedName{Namespace: "default", Name: pool.Name}, pool, sb)
			require.NoError(t, err)
			require.Equal(t, memberNotDeletable, outcome)
			require.Empty(t, h.deleteCalls(), "no delete may be sent for a Sandbox that is not this pool's member")
		})
	}
}

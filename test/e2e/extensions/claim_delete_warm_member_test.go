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

package extensions

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/test/e2e/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// adoptWarmMember creates a one-replica warm pool, waits for its member to
// become Ready, and has a claim adopt it. It returns the pool, the claim and
// the adopted Sandbox.
func adoptWarmMember(t *testing.T, tc *framework.TestContext, nsPrefix string) (*extensionsv1beta1.SandboxWarmPool, *extensionsv1beta1.SandboxClaim, *sandboxv1beta1.Sandbox) {
	t.Helper()
	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("%s-%d", nsPrefix, time.Now().UnixNano())
	require.NoError(t, tc.CreateWithCleanup(t.Context(), ns))

	template := newWarmPoolTemplate(ns.Name)
	require.NoError(t, tc.CreateWithCleanup(t.Context(), template))

	replicas := int32(1)
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "member-pool", Namespace: ns.Name},
		Spec: extensionsv1beta1.SandboxWarmPoolSpec{
			Replicas:    &replicas,
			TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: template.Name},
		},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), warmPool))
	waitForWarmPoolSandboxReady(t, tc, ns.Name, warmPool)
	members := warmPoolMembers(t, tc, warmPool)
	require.Len(t, members, 1)
	member := members[0]

	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "member-claim", Namespace: ns.Name},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: warmPool.Name}},
	}
	require.NoError(t, tc.CreateWithCleanup(t.Context(), claim))
	waitForClaimReady(t, tc, claim)
	require.Equal(t, member.Name, claim.Status.SandboxStatus.Name, "the claim adopted the warm member")

	adopted := &sandboxv1beta1.Sandbox{}
	require.NoError(t, tc.Get(t.Context(), types.NamespacedName{Name: member.Name, Namespace: ns.Name}, adopted))
	require.Equal(t, member.UID, adopted.UID)
	require.True(t, metav1.IsControlledBy(adopted, claim), "the adopted Sandbox is controlled by the claim")
	require.Len(t, adopted.OwnerReferences, 1, "the claim is the adopted Sandbox's only owner")
	require.NotContains(t, adopted.Labels, sandboxv1beta1.SandboxWarmPoolLabel)
	return warmPool, claim, adopted
}

// warmPoolMembers lists the live Sandboxes the pool controls.
func warmPoolMembers(t *testing.T, tc *framework.TestContext, warmPool *extensionsv1beta1.SandboxWarmPool) []sandboxv1beta1.Sandbox {
	t.Helper()
	list := &sandboxv1beta1.SandboxList{}
	require.NoError(t, tc.List(t.Context(), list, client.InNamespace(warmPool.Namespace)))
	var members []sandboxv1beta1.Sandbox
	for _, sb := range list.Items {
		if sb.DeletionTimestamp.IsZero() && metav1.IsControlledBy(&sb, warmPool) {
			members = append(members, sb)
		}
	}
	return members
}

// requireFreshReplacement waits for the pool to settle on exactly one Ready
// member that is not the adopted Sandbox.
func requireFreshReplacement(t *testing.T, tc *framework.TestContext, warmPool *extensionsv1beta1.SandboxWarmPool, adopted *sandboxv1beta1.Sandbox) {
	t.Helper()
	require.Eventually(t, func() bool {
		members := warmPoolMembers(t, tc, warmPool)
		if len(members) != 1 || members[0].UID == adopted.UID || members[0].Name == adopted.Name || !isSandboxReady(&members[0]) {
			return false
		}
		pool := &extensionsv1beta1.SandboxWarmPool{}
		if err := tc.Get(t.Context(), client.ObjectKeyFromObject(warmPool), pool); err != nil {
			return false
		}
		return pool.Status.Replicas == 1 && pool.Status.ReadyReplicas == 1
	}, 90*time.Second, time.Second, "the pool should replenish with a fresh member")
}

// TestSandboxClaimDeleteDeletesAdoptedWarmMember: deleting a claim that
// adopted a warm pool member deletes that Sandbox and its Pod through the
// garbage collector, and the pool replenishes with a fresh member instead of
// taking the adopted one back.
func TestSandboxClaimDeleteDeletesAdoptedWarmMember(t *testing.T) {
	tc := framework.NewTestContext(t)
	warmPool, claim, adopted := adoptWarmMember(t, tc, "claim-delete-member")

	require.NoError(t, tc.Delete(t.Context(), claim))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), claim))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), adopted))
	pod := &corev1.Pod{}
	pod.Name = adopted.Name
	pod.Namespace = adopted.Namespace
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), pod))

	requireFreshReplacement(t, tc, warmPool, adopted)
}

// TestSandboxClaimOrphanDeleteNeverReturnsWarmMember: when a claim is deleted
// with propagationPolicy=Orphan, the adopted Sandbox survives with no owner.
// The pool must never re-own or count it, and replenishes with a fresh
// member instead.
func TestSandboxClaimOrphanDeleteNeverReturnsWarmMember(t *testing.T) {
	tc := framework.NewTestContext(t)
	warmPool, claim, adopted := adoptWarmMember(t, tc, "claim-orphan-member")

	orphan := metav1.DeletePropagationOrphan
	require.NoError(t, tc.DynamicClient().Resource(claimGVR).Namespace(claim.Namespace).Delete(t.Context(), claim.Name, metav1.DeleteOptions{PropagationPolicy: &orphan}))
	require.NoError(t, tc.WaitForObjectNotFound(t.Context(), claim))

	survivor := &sandboxv1beta1.Sandbox{}
	key := client.ObjectKeyFromObject(adopted)
	require.Eventually(t, func() bool {
		if err := tc.Get(t.Context(), key, survivor); err != nil {
			return false
		}
		return len(survivor.OwnerReferences) == 0
	}, 60*time.Second, time.Second, "the garbage collector should orphan the adopted Sandbox")
	require.Equal(t, adopted.UID, survivor.UID)

	requireFreshReplacement(t, tc, warmPool, adopted)

	// Give the pool several more reconciles to take the survivor back.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, tc.Get(t.Context(), key, survivor))
		require.Equal(t, adopted.UID, survivor.UID)
		require.Empty(t, survivor.OwnerReferences, "the pool must not re-own an orphaned ex-member")
		require.NotContains(t, survivor.Labels, sandboxv1beta1.SandboxWarmPoolLabel)
		members := warmPoolMembers(t, tc, warmPool)
		require.Len(t, members, 1)
		require.NotEqual(t, adopted.UID, members[0].UID, "the orphaned ex-member is not a pool member")
		pool := &extensionsv1beta1.SandboxWarmPool{}
		require.NoError(t, tc.Get(t.Context(), client.ObjectKeyFromObject(warmPool), pool))
		require.Equal(t, int32(1), pool.Status.Replicas, "the orphaned ex-member is not counted")
		time.Sleep(time.Second)
	}
}

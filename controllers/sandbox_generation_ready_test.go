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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// TestReconcileReportsReadyForGenerationOnlyAfterPodMetadata pins the
// invariant SandboxClaim readiness relies on after a warm adoption: a Ready
// condition stamped with the Sandbox's current generation is published only
// after the existing Pod carries that generation's PodTemplate metadata.
func TestReconcileReportsReadyForGenerationOnlyAfterPodMetadata(t *testing.T) {
	const (
		name     = "adopted-sandbox"
		ns       = "default"
		identity = "sandbox.users.io/environment-id"
	)
	nameHash := NameHash(name)

	// Adoption rewrote the PodTemplate (generation 2); the status still
	// describes the warm Pod of generation 1.
	newSandbox := func() *sandboxv1beta1.Sandbox {
		return &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns, UID: sandboxUID, Generation: 2,
				Annotations: map[string]string{sandboxv1beta1.SandboxPodNameAnnotation: name},
			},
			Spec: sandboxv1beta1.SandboxSpec{
				OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
					ObjectMeta: sandboxv1beta1.PodMetadata{Labels: map[string]string{identity: "env-1"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
				}},
			},
			Status: sandboxv1beta1.SandboxStatus{Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             sandboxv1beta1.SandboxReasonDependenciesReady,
				ObservedGeneration: 1,
			}}},
		}
	}
	newWarmPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns,
				Labels:          map[string]string{sandboxLabel: nameHash, sandboxv1beta1.SandboxWarmPoolLabel: NameHash("pool")},
				OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(name)},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				PodIPs:     []corev1.PodIP{{IP: "10.0.0.1"}},
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		}
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}

	readyCondition := func(t *testing.T, c client.Client) metav1.Condition {
		t.Helper()
		sb := &sandboxv1beta1.Sandbox{}
		require.NoError(t, c.Get(t.Context(), req.NamespacedName, sb))
		cond := meta.FindStatusCondition(sb.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
		require.NotNil(t, cond)
		return *cond
	}

	t.Run("pod metadata lands before Ready for the new generation", func(t *testing.T) {
		c := newFakeClient(newSandbox(), newWarmPod())
		r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}

		_, err := r.Reconcile(t.Context(), req)
		require.NoError(t, err)

		pod := &corev1.Pod{}
		require.NoError(t, c.Get(t.Context(), req.NamespacedName, pod))
		require.Equal(t, "env-1", pod.Labels[identity])
		require.NotContains(t, pod.Labels, sandboxv1beta1.SandboxWarmPoolLabel)
		ready := readyCondition(t, c)
		require.Equal(t, metav1.ConditionTrue, ready.Status)
		require.Equal(t, int64(2), ready.ObservedGeneration)
	})

	t.Run("a failed pod metadata patch never reports Ready for the new generation", func(t *testing.T) {
		c := interceptor.NewClient(newFakeClient(newSandbox(), newWarmPod()), interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod {
					return errors.New("injected pod patch failure")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		})
		r := &SandboxReconciler{Client: c, Scheme: Scheme, Tracer: asmetrics.NewNoOp()}

		_, err := r.Reconcile(t.Context(), req)
		require.Error(t, err)

		ready := readyCondition(t, c)
		require.False(t, ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == 2,
			"Ready must not be reported for generation 2 while the Pod lacks its metadata: %+v", ready)
	})
}

/*
Copyright 2025 The KubeFleet Authors.

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

package workgenerator

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clusterv1beta1 "go.goms.io/fleet/apis/cluster/v1beta1"
	placementv1beta1 "go.goms.io/fleet/apis/placement/v1beta1"
	"go.goms.io/fleet/pkg/utils"
	"go.goms.io/fleet/pkg/utils/controller"
	"go.goms.io/fleet/test/utils/informer"
	"go.goms.io/fleet/test/utils/resource"
)

func serviceScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := placementv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add v1beta1 scheme: %v", err)
	}
	return scheme
}

func TestFetchClusterResourceOverrideSnapshot(t *testing.T) {
	snapshots := []placementv1beta1.ClusterResourceOverrideSnapshot{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cro-1",
				Labels: map[string]string{
					placementv1beta1.IsLatestSnapshotLabel: "true",
				},
			},
			Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
				OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
					ClusterResourceSelectors: []placementv1beta1.ClusterResourceSelector{
						{
							Group:   "rbac.authorization.k8s.io",
							Version: "v1",
							Kind:    "ClusterRole",
							Name:    "test-cluster-role",
						},
						{
							Group:   "group",
							Version: "version",
							Kind:    "ClusterRole",
							Name:    "test-cluster-role",
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cro-2",
				Labels: map[string]string{
					placementv1beta1.IsLatestSnapshotLabel: "true",
				},
			},
			Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
				OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
					ClusterResourceSelectors: []placementv1beta1.ClusterResourceSelector{
						{
							Group:   "rbac.authorization.k8s.io",
							Version: "v1",
							Kind:    "ClusterRole",
							Name:    "test-cluster-role",
						},
						{
							Group:   "rbac.authorization.k8s.io",
							Version: "v1",
							Kind:    "ClusterRole",
							Name:    "test-cluster-role-1",
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name          string
		snapshotNames []string
		want          map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot
		wantErr       error
	}{
		{
			name: "snapshot not found",
			snapshotNames: []string{
				"not-found",
			},
			wantErr: controller.ErrUserError,
		},
		{
			name:          "nil overrides in the binding",
			snapshotNames: nil,
			want:          map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{},
		},
		{
			name:          "empty overrides in the binding",
			snapshotNames: []string{},
			want:          map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{},
		},
		{
			name: "single override in the binding",
			snapshotNames: []string{
				"cro-1",
			},
			want: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "test-cluster-role",
				}: {
					&snapshots[0],
				},
				{
					Group:   "group",
					Version: "version",
					Kind:    "ClusterRole",
					Name:    "test-cluster-role",
				}: {
					&snapshots[0],
				},
			},
		},
		{
			name: "multiple overrides in the binding",
			snapshotNames: []string{
				"cro-1",
				"cro-2",
			},
			want: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "test-cluster-role",
				}: {
					&snapshots[0],
					&snapshots[1],
				},
				{
					Group:   "group",
					Version: "version",
					Kind:    "ClusterRole",
					Name:    "test-cluster-role",
				}: {
					&snapshots[0],
				},
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "test-cluster-role-1",
				}: {
					&snapshots[1],
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := serviceScheme(t)
			var objects []client.Object
			for i := range snapshots {
				objects = append(objects, &snapshots[i])
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				Build()
			r := Reconciler{
				Client: fakeClient,
			}
			ctx := context.Background()
			binding := &placementv1beta1.ClusterResourceBinding{
				Spec: placementv1beta1.ResourceBindingSpec{
					ClusterResourceOverrideSnapshots: tc.snapshotNames,
				},
			}
			got, err := r.fetchClusterResourceOverrideSnapshots(ctx, binding)
			if gotErr, wantErr := err != nil, tc.wantErr != nil; gotErr != wantErr || !errors.Is(err, tc.wantErr) {
				t.Fatalf("fetchClusterResourceOverrideSnapshots() got error %v, want error %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.IgnoreFields(placementv1beta1.ClusterResourceOverrideSnapshot{}, "TypeMeta")); diff != "" {
				t.Errorf("fetchClusterResourceOverrideSnapshots() returned mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestFetchResourceOverrideSnapshot(t *testing.T) {
	snapshots := []placementv1beta1.ResourceOverrideSnapshot{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ro-1",
				Namespace: "svc-namespace",
				Labels: map[string]string{
					placementv1beta1.IsLatestSnapshotLabel: "true",
				},
			},
			Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
				OverrideSpec: placementv1beta1.ResourceOverrideSpec{
					ResourceSelectors: []placementv1beta1.ResourceSelector{
						{
							Group:   "",
							Version: "v1",
							Kind:    "Service",
							Name:    "svc-name",
						},
						{
							Group:   "",
							Version: "v1",
							Kind:    "Deployment",
							Name:    "svc-name",
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ro-2",
				Namespace: "svc-namespace-1",
				Labels: map[string]string{
					placementv1beta1.IsLatestSnapshotLabel: "true",
				},
			},
			Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
				OverrideSpec: placementv1beta1.ResourceOverrideSpec{
					ResourceSelectors: []placementv1beta1.ResourceSelector{
						{
							Group:   "",
							Version: "v1",
							Kind:    "Service",
							Name:    "svc-name",
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ro-3",
				Namespace: "svc-namespace",
				Labels: map[string]string{
					placementv1beta1.IsLatestSnapshotLabel: "true",
				},
			},
			Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
				OverrideSpec: placementv1beta1.ResourceOverrideSpec{
					ResourceSelectors: []placementv1beta1.ResourceSelector{
						{
							Group:   "",
							Version: "v1",
							Kind:    "Service",
							Name:    "svc-name",
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name          string
		snapshotNames []placementv1beta1.NamespacedName
		want          map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot
		wantErr       error
	}{
		{
			name: "snapshot not found",
			snapshotNames: []placementv1beta1.NamespacedName{
				{
					Name: "ro-1",
				},
			},
			wantErr: controller.ErrUserError,
		},
		{
			name:          "nil overrides in the binding",
			snapshotNames: nil,
			want:          map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{},
		},
		{
			name:          "empty overrides in the binding",
			snapshotNames: []placementv1beta1.NamespacedName{},
			want:          map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{},
		},
		{
			name: "single override in the binding",
			snapshotNames: []placementv1beta1.NamespacedName{
				{
					Name:      "ro-1",
					Namespace: "svc-namespace",
				},
			},
			want: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     "",
					Version:   "v1",
					Kind:      "Service",
					Name:      "svc-name",
					Namespace: "svc-namespace",
				}: {
					&snapshots[0],
				},
				{
					Group:     "",
					Version:   "v1",
					Kind:      "Deployment",
					Name:      "svc-name",
					Namespace: "svc-namespace",
				}: {
					&snapshots[0],
				},
			},
		},
		{
			name: "multiple overrides in the binding",
			snapshotNames: []placementv1beta1.NamespacedName{
				{
					Name:      "ro-1",
					Namespace: "svc-namespace",
				},
				{
					Name:      "ro-2",
					Namespace: "svc-namespace-1",
				},
				{
					Name:      "ro-3",
					Namespace: "svc-namespace",
				},
			},
			want: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     "",
					Version:   "v1",
					Kind:      "Service",
					Name:      "svc-name",
					Namespace: "svc-namespace",
				}: {
					&snapshots[0],
					&snapshots[2],
				},
				{
					Group:     "",
					Version:   "v1",
					Kind:      "Deployment",
					Name:      "svc-name",
					Namespace: "svc-namespace",
				}: {
					&snapshots[0],
				},
				{
					Group:     "",
					Version:   "v1",
					Kind:      "Service",
					Name:      "svc-name",
					Namespace: "svc-namespace-1",
				}: {
					&snapshots[1],
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := serviceScheme(t)
			var objects []client.Object
			for i := range snapshots {
				objects = append(objects, &snapshots[i])
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				Build()
			r := Reconciler{
				Client: fakeClient,
			}
			ctx := context.Background()
			binding := &placementv1beta1.ClusterResourceBinding{
				Spec: placementv1beta1.ResourceBindingSpec{
					ResourceOverrideSnapshots: tc.snapshotNames,
				},
			}
			got, err := r.fetchResourceOverrideSnapshots(ctx, binding)
			if gotErr, wantErr := err != nil, tc.wantErr != nil; gotErr != wantErr || !errors.Is(err, tc.wantErr) {
				t.Fatalf("fetchResourceOverrideSnapshots() got error %v, want error %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.IgnoreFields(placementv1beta1.ResourceOverrideSnapshot{}, "TypeMeta")); diff != "" {
				t.Errorf("fetchResourceOverrideSnapshots() returned mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestApplyOverrides_clusterScopedResource(t *testing.T) {
	fakeInformer := informer.FakeManager{
		APIResources: map[schema.GroupVersionKind]bool{
			{
				Group:   "",
				Version: "v1",
				Kind:    "Deployment",
			}: true,
		},
		IsClusterScopedResource: false,
	}
	clusterRoleType := metav1.TypeMeta{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "ClusterRole",
	}

	tests := []struct {
		name            string
		clusterRole     rbacv1.ClusterRole
		cluster         clusterv1beta1.MemberCluster
		croMap          map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot
		wantClusterRole rbacv1.ClusterRole
		wantErr         error
		wantDeleted     bool
	}{
		{
			name: "empty overrides",
			clusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{},
			wantClusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
				},
			},
		},
		{
			name: "no matched overrides",
			clusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "not-found",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/labels/new-label",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantClusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
		},
		{
			name: "selected by clusterResourceOverride but only one rule matched the cluster",
			clusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "clusterrole-name",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											// matching rule
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/labels/new-label",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
												},
											},
										},
										{
											// non matching rule
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key2": "value1",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/new-label",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value1"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantClusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
					Labels: map[string]string{
						"app":       "app1",
						"new-label": "new-value",
					},
				},
			},
		},
		{
			name: "selected by clusterResourceOverride with two rules that don't conflict",
			clusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"authorization.k8s.io"},
						Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"},
						Verbs:     []string{"create"},
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "clusterrole-name",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/rules/0/verbs/1",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"read"`)},
												},
											},
										},
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key2": "value2",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpRemove,
													Path:     "/rules/0/verbs/0",
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantClusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"authorization.k8s.io"},
						Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"},
						Verbs:     []string{"read"},
					},
				},
			},
		},
		{
			name: "selected by clusterResourceOverride with two rules that conflict but still a valid patch",
			clusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"authorization.k8s.io"},
						Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"},
						Verbs:     []string{"create"},
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "clusterrole-name",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/rules/0/verbs/1",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"read"`)},
												},
											},
										},
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key2": "value2",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpRemove,
													Path:     "/rules/0/verbs/1",
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantClusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"authorization.k8s.io"},
						Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"},
						Verbs:     []string{"create"},
					},
				},
			},
		},
		{
			name: "selected by clusterResourceOverride with two rules that conflict and result in error",
			clusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{"authorization.k8s.io"},
						Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"},
						Verbs:     []string{"create"},
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "clusterrole-name",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpRemove,
													Path:     "/rules/0/verbs",
												},
											},
										},
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key2": "value2",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/rules/0/verbs/1",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"read"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: controller.ErrUserError,
		},
		{
			name: "invalid json patch of clusterResourceOverride",
			clusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "clusterrole-name",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/labels/new-label",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: controller.ErrUserError,
		},
		{
			name: "delete during the clusterResourceOverride",
			clusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "clusterrole-name",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.DeleteOverrideType,
										},
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/key1",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value1"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantDeleted: true,
		},
		{
			name: "delete after patching the clusterResourceOverride",
			clusterRole: rbacv1.ClusterRole{
				TypeMeta: clusterRoleType,
				ObjectMeta: metav1.ObjectMeta{
					Name: "clusterrole-name",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   "rbac.authorization.k8s.io",
					Version: "v1",
					Kind:    "ClusterRole",
					Name:    "clusterrole-name",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key2": "value2",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/key1",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value1"`)},
												},
											},
										},
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.DeleteOverrideType,
										},
									},
								},
							},
						},
					},
				},
			},
			wantDeleted: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Reconciler{
				InformerManager: &fakeInformer,
			}
			rc := resource.CreateResourceContentForTest(t, tc.clusterRole)
			gotDeleted, err := r.applyOverrides(rc, &tc.cluster, tc.croMap, nil)
			if gotErr, wantErr := err != nil, tc.wantErr != nil; gotErr != wantErr || !errors.Is(err, tc.wantErr) {
				t.Fatalf("applyOverrides() got error %v, want error %v", err, tc.wantErr)
			}
			if gotDeleted != tc.wantDeleted {
				t.Fatalf("applyOverrides() gotDeleted %v, want %v", gotDeleted, tc.wantDeleted)
			}
			if tc.wantErr != nil {
				return
			}
			if tc.wantDeleted {
				return
			}

			var u unstructured.Unstructured
			if err := u.UnmarshalJSON(rc.Raw); err != nil {
				t.Fatalf("Failed to unmarshl the result: %v, want nil", err)
			}

			var clusterRole rbacv1.ClusterRole
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &clusterRole); err != nil {
				t.Fatalf("Failed to convert the result to clusterole: %v, want nil", err)
			}

			if diff := cmp.Diff(tc.wantClusterRole, clusterRole); diff != "" {
				t.Errorf("applyOverrides() clusterRole mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestApplyOverrides_namespacedScopeResource(t *testing.T) {
	fakeInformer := informer.FakeManager{
		APIResources: map[schema.GroupVersionKind]bool{
			{
				Group:   utils.DeploymentGVK.Group,
				Version: utils.DeploymentGVK.Version,
				Kind:    utils.DeploymentGVK.Kind,
			}: true,
		},
		IsClusterScopedResource: false,
	}
	deploymentType := metav1.TypeMeta{
		APIVersion: utils.DeploymentGVK.GroupVersion().String(),
		Kind:       utils.DeploymentGVK.Kind,
	}

	tests := []struct {
		name           string
		deployment     appsv1.Deployment
		cluster        clusterv1beta1.MemberCluster
		croMap         map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot
		roMap          map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot
		wantDeployment appsv1.Deployment
		wantErr        error
		wantDelete     bool
	}{
		{
			name: "empty overrides",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{},
			roMap:  map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
		},
		{
			name: "no matched overrides on clusters",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   utils.NamespaceMetaGVK.Group,
					Version: utils.NamespaceMetaGVK.Version,
					Kind:    utils.NamespaceMetaGVK.Kind,
					Name:    "invalid-namespace",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/labels/new-label",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
		},
		{
			name: "no matched overrides on resources",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   utils.NamespaceMetaGVK.Group,
					Version: utils.NamespaceMetaGVK.Version,
					Kind:    utils.NamespaceMetaGVK.Kind,
					Name:    "invalid-namespace",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/labels/new-label",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     "",
					Version:   "v1",
					Kind:      "Deployment",
					Name:      "deployment-name",
					Namespace: "deployment-namespace-1",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: nil, // matching all the clusters
											OverrideType:    placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/app",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"app3"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				},
			},
		},
		{
			name: "selected by clusterResourceOverride",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   utils.NamespaceMetaGVK.Group,
					Version: utils.NamespaceMetaGVK.Version,
					Kind:    utils.NamespaceMetaGVK.Kind,
					Name:    "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/labels/new-label",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
												},
											},
										},
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key2": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/new-label",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value1"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app":       "app1",
						"new-label": "new-value",
					},
				},
			},
		},
		{
			name: "selected by resourceOverride",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/spec/minReadySeconds",
													Value:    apiextensionsv1.JSON{Raw: []byte("1")},
												},
											},
										},
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // selecting all the clusters
											OverrideType:    placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/spec/minReadySeconds",
													Value:    apiextensionsv1.JSON{Raw: []byte("2")},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app1",
					},
				},
				Spec: appsv1.DeploymentSpec{MinReadySeconds: 2},
			},
		},
		{
			name: "resourceOverride wins",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   utils.NamespaceMetaGVK.Group,
					Version: utils.NamespaceMetaGVK.Version,
					Kind:    utils.NamespaceMetaGVK.Kind,
					Name:    "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/app",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"app2"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // matching all the clusters
											OverrideType:    placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/app",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"app3"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app3",
					},
				},
			},
		},
		{
			name: "invalid json patch of clusterResourceOverride",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   utils.NamespaceMetaGVK.Group,
					Version: utils.NamespaceMetaGVK.Version,
					Kind:    utils.NamespaceMetaGVK.Kind,
					Name:    "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/label/app",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"app2"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: controller.ErrUserError,
		},
		{
			name: "invalid json patch of resourceOverride",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // matching all the clusters
											OverrideType:    placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/spec",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"app3"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: controller.ErrUserError,
		},
		{
			name: "delete type of resourceOverride",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // matching all the clusters
											OverrideType:    placementv1beta1.DeleteOverrideType,
										},
									},
								},
							},
						},
					},
				},
			},
			wantDelete: true,
		},
		{
			name: "resourceOverride delete the cro override",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   utils.NamespaceMetaGVK.Group,
					Version: utils.NamespaceMetaGVK.Version,
					Kind:    utils.NamespaceMetaGVK.Kind,
					Name:    "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/app",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"app2"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // matching all the clusters
											OverrideType:    placementv1beta1.DeleteOverrideType,
										},
									},
								},
							},
						},
					},
				},
			},
			wantDelete: true,
		},
		{
			name: "resourceOverride no-op when the cro delete",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "app1",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{
				{
					Group:   utils.NamespaceMetaGVK.Group,
					Version: utils.NamespaceMetaGVK.Version,
					Kind:    utils.NamespaceMetaGVK.Kind,
					Name:    "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ClusterResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ClusterResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{
												ClusterSelectorTerms: []placementv1beta1.ClusterSelectorTerm{
													{
														LabelSelector: &metav1.LabelSelector{
															MatchLabels: map[string]string{
																"key1": "value1",
															},
														},
													},
												},
											},
											OverrideType: placementv1beta1.DeleteOverrideType,
										},
									},
								},
							},
						},
					},
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // matching all the clusters
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/labels/new-label",
													Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantDelete: true,
		},
		{
			name: "cluster name as value in json patch of resourceOverride",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app":  "app1",
						"key2": "value2",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"app": "value1",
					},
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // matching all the clusters
											OverrideType:    placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/app",
													Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`"%s"`, placementv1beta1.OverrideClusterNameVariable))},
												},
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/annotations",
													Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf("{\"app\": \"%s\", \"test\": \"nginx\"}", placementv1beta1.OverrideClusterNameVariable))},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app":  "cluster-1",
						"key2": "value2",
					},
					Annotations: map[string]string{
						"app":  "cluster-1",
						"test": "nginx",
					},
				},
			},
		},
		{
			name: "test multiple rules with cluster name template",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
				},
			},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // matching all the clusters
											OverrideType:    placementv1beta1.JSONPatchOverrideType,
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/app",
													Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`"%s"`, placementv1beta1.OverrideClusterNameVariable))},
												},
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/annotations",
													Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf("{\"app\": \"workload-%s\", \"test\": \"nginx\"}", placementv1beta1.OverrideClusterNameVariable))},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "cluster-1",
					},
					Annotations: map[string]string{
						"app":  "workload-cluster-1",
						"test": "nginx",
					},
				},
			},
		},
		{
			name: "replace using cluster label key variables",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"region": "us-west",
						"env":    "production",
						"zone":   "west-1a",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // matching all the clusters
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/app",
													Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`"%s-region"`, placementv1beta1.OverrideClusterLabelKeyVariablePrefix+"region}"))},
												},
												{
													Operator: placementv1beta1.JSONPatchOverrideOpAdd,
													Path:     "/metadata/annotations",
													Value: apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`{"environment": "%s", "zone": "%s"}`,
														placementv1beta1.OverrideClusterLabelKeyVariablePrefix+"env}",
														placementv1beta1.OverrideClusterLabelKeyVariablePrefix+"zone}"))},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "us-west-region",
					},
					Annotations: map[string]string{
						"environment": "production",
						"zone":        "west-1a",
					},
				},
			},
		},
		{
			name: "replace with non-existent label key",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			cluster: clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"region": "us-west",
					},
				},
			},
			croMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ClusterResourceOverrideSnapshot{},
			roMap: map[placementv1beta1.ResourceIdentifier][]*placementv1beta1.ResourceOverrideSnapshot{
				{
					Group:     utils.DeploymentGVK.Group,
					Version:   utils.DeploymentGVK.Version,
					Kind:      utils.DeploymentGVK.Kind,
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				}: {
					{
						Spec: placementv1beta1.ResourceOverrideSnapshotSpec{
							OverrideSpec: placementv1beta1.ResourceOverrideSpec{
								Policy: &placementv1beta1.OverridePolicy{
									OverrideRules: []placementv1beta1.OverrideRule{
										{
											ClusterSelector: &placementv1beta1.ClusterSelector{}, // matching all the clusters
											JSONPatchOverrides: []placementv1beta1.JSONPatchOverride{
												{
													Operator: placementv1beta1.JSONPatchOverrideOpReplace,
													Path:     "/metadata/labels/app",
													Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`"%s-app"`, placementv1beta1.OverrideClusterLabelKeyVariablePrefix+"non-existent}"))},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantErr: controller.ErrUserError,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Reconciler{
				InformerManager: &fakeInformer,
			}
			rc := resource.CreateResourceContentForTest(t, tc.deployment)
			gotDeleted, err := r.applyOverrides(rc, &tc.cluster, tc.croMap, tc.roMap)
			if gotErr, wantErr := err != nil, tc.wantErr != nil; gotErr != wantErr || !errors.Is(err, tc.wantErr) {
				t.Fatalf("applyOverrides() got error %v, want error %v", err, tc.wantErr)
			}
			if gotDeleted != tc.wantDelete {
				t.Fatalf("applyOverrides() gotDeleted %v, want %v", gotDeleted, tc.wantDelete)
			}
			if tc.wantErr != nil {
				return
			}
			if tc.wantDelete {
				return
			}

			var u unstructured.Unstructured
			if err := u.UnmarshalJSON(rc.Raw); err != nil {
				t.Fatalf("Failed to unmarshl the result: %v, want nil", err)
			}

			var deployment appsv1.Deployment
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &deployment); err != nil {
				t.Fatalf("Failed to convert the result to deployment: %v, want nil", err)
			}

			if diff := cmp.Diff(tc.wantDeployment, deployment); diff != "" {
				t.Errorf("applyOverrides() deployment mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestApplyJSONPatchOverride(t *testing.T) {
	deploymentType := metav1.TypeMeta{
		APIVersion: "v1",
		Kind:       "Deployment",
	}

	testCases := []struct {
		name           string
		deployment     appsv1.Deployment
		overrides      []placementv1beta1.JSONPatchOverride
		cluster        *clusterv1beta1.MemberCluster
		wantDeployment appsv1.Deployment
		wantErr        bool
	}{
		{
			name: "empty override",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
		},
		{
			name: "reset the labels using add operation",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx-1",
						"key": "value",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpAdd,
					Path:     "/metadata/labels",
					Value:    apiextensionsv1.JSON{Raw: []byte(`{"app": "nginx"}`)},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
		},
		{
			name: "reset the labels using replace operation",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx-1",
						"key": "value",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpReplace,
					Path:     "/metadata/labels",
					Value:    apiextensionsv1.JSON{Raw: []byte(`{"app": "nginx"}`)},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
		},
		{
			name: "add the first label key value",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					// To add the first key, it cannot use "replace" as the path is missing.
					Operator: placementv1beta1.JSONPatchOverrideOpAdd,
					Path:     "/metadata/labels",
					Value:    apiextensionsv1.JSON{Raw: []byte(`{"app": "nginx"}`)},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
		},
		{
			name: "add a label key value in the existing labels",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpAdd,
					Path:     "/metadata/labels/new-label",
					Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app":       "nginx",
						"new-label": "new-value",
					},
				},
			},
		},
		{
			name: "remove a label",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpRemove,
					Path:     "/metadata/labels/app",
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels:    map[string]string{},
				},
			},
		},
		{
			name: "replace a label",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpReplace,
					Path:     "/metadata/labels/app",
					Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "new-value",
					},
				},
			},
		},
		{
			name: "multiple rules",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
				Spec: appsv1.DeploymentSpec{
					MinReadySeconds: 10,
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpReplace,
					Path:     "/metadata/labels/app",
					Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
				},
				{
					Operator: placementv1beta1.JSONPatchOverrideOpAdd,
					Path:     "/spec/minReadySeconds",
					Value:    apiextensionsv1.JSON{Raw: []byte("1")},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "new-value",
					},
				},
				Spec: appsv1.DeploymentSpec{MinReadySeconds: 1},
			},
		},
		{
			name: "invalid JSON patch value (should have quotation marks)",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpReplace,
					Path:     "/metadata/labels/app",
					Value:    apiextensionsv1.JSON{Raw: []byte("new-value")},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid JSON patch path",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpReplace,
					Path:     "/metadata/invalid",
					Value:    apiextensionsv1.JSON{Raw: []byte(`"new-value"`)},
				},
			},
			wantErr: true,
		},
		{
			name: "typo in template variable should just be rendered as is",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpReplace,
					Path:     "/metadata/labels/app",
					Value:    apiextensionsv1.JSON{Raw: []byte(`"$CLUSTER_NAME"`)},
				},
				{
					Operator: placementv1beta1.JSONPatchOverrideOpAdd,
					Path:     "/metadata/labels/${Member-Cluster-Name}",
					Value:    apiextensionsv1.JSON{Raw: []byte(`"${CLUSTER-NAME}"`)},
				},
			},
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app":                    "$CLUSTER_NAME",
						"${Member-Cluster-Name}": "${CLUSTER-NAME}",
					},
				},
			},
		},
		{
			name: "multiple rules with cluster name template",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpReplace,
					Path:     "/metadata/labels/app",
					Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`"%s"`, placementv1beta1.OverrideClusterNameVariable))},
				},
				{
					Operator: placementv1beta1.JSONPatchOverrideOpAdd,
					Path:     "/metadata/annotations",
					Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf("{\"app\": \"workload-%s\", \"test\": \"nginx\"}", placementv1beta1.OverrideClusterNameVariable))},
				},
			},
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "cluster-1",
					},
					Annotations: map[string]string{
						"app":  "workload-cluster-1",
						"test": "nginx",
					},
				},
			},
		},
		{
			name: "replace using cluster label key variables",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpReplace,
					Path:     "/metadata/labels/app",
					Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`"%s-app"`, placementv1beta1.OverrideClusterLabelKeyVariablePrefix+"region}"))},
				},
				{
					Operator: placementv1beta1.JSONPatchOverrideOpAdd,
					Path:     "/metadata/annotations",
					Value: apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`{"environment": "%s", "zone": "%s"}`,
						placementv1beta1.OverrideClusterLabelKeyVariablePrefix+"fleet-kubernetes.io/env}",
						placementv1beta1.OverrideClusterLabelKeyVariablePrefix+"zone}"))},
				},
			},
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-1",
					Labels: map[string]string{
						"region":                  "us-west",
						"fleet-kubernetes.io/env": "production",
						"zone":                    "west-1a",
					},
				},
			},
			wantDeployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "us-west-app",
					},
					Annotations: map[string]string{
						"environment": "production",
						"zone":        "west-1a",
					},
				},
			},
		},
		{
			name: "replace with non-existent label key",
			deployment: appsv1.Deployment{
				TypeMeta: deploymentType,
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deployment-name",
					Namespace: "deployment-namespace",
					Labels: map[string]string{
						"app": "nginx",
					},
				},
			},
			overrides: []placementv1beta1.JSONPatchOverride{
				{
					Operator: placementv1beta1.JSONPatchOverrideOpReplace,
					Path:     "/metadata/labels/app",
					Value:    apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`"%s-app"`, placementv1beta1.OverrideClusterLabelKeyVariablePrefix+"non-existent}"))},
				},
			},
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rc := resource.CreateResourceContentForTest(t, tc.deployment)
			cluster := tc.cluster
			if cluster == nil {
				cluster = &clusterv1beta1.MemberCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster-1",
					},
				}
			}
			err := applyJSONPatchOverride(rc, cluster, tc.overrides)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("applyJSONPatchOverride() = error %v, want %v", err, tc.wantErr)
			}

			if tc.wantErr {
				return
			}

			var u unstructured.Unstructured
			if err := u.UnmarshalJSON(rc.Raw); err != nil {
				t.Fatalf("Failed to unmarshl the result: %v, want nil", err)
			}

			var deployment appsv1.Deployment
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &deployment); err != nil {
				t.Fatalf("Failed to convert the result to deployment: %v, want nil", err)
			}

			if diff := cmp.Diff(tc.wantDeployment, deployment); diff != "" {
				t.Errorf("applyJSONPatchOverride() deployment mismatch (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestReplaceClusterLabelKeyVariables(t *testing.T) {
	tests := map[string]struct {
		cluster   *clusterv1beta1.MemberCluster
		input     string
		expected  string
		expectErr bool
	}{
		"No clusterLabelKey variables": {
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"region": "us-west-1",
					},
				},
			},
			input:    "The cluster is in us-west-1",
			expected: "The cluster is in us-west-1",
		},
		"ClusterLabelKey Variable replaced": {
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"region": "us-west-1",
					},
				},
			},
			input:    "The cluster is in ${MEMBER-CLUSTER-LABEL-KEY-region}",
			expected: "The cluster is in us-west-1",
		},
		"The clusterLabelKey key is misspelled": {
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			input:    "The cluster is in $MEMBER-CLUSTER-LABEL-KEY-region",
			expected: "The cluster is in $MEMBER-CLUSTER-LABEL-KEY-region",
		},
		"Multiple complex clusterLabelKey variables replaced": {
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"fleet.azure.com/location-region_public": "us-west-1",
						"fleet.azure.com/env":                    "prod",
					},
				},
			},
			input:    "The cluster is in ${MEMBER-CLUSTER-LABEL-KEY-fleet.azure.com/location-region_public} and environment is ${MEMBER-CLUSTER-LABEL-KEY-fleet.azure.com/env}",
			expected: "The cluster is in us-west-1 and environment is prod",
		},
		"The clusterLabelKey key is not found": {
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			input:     "The cluster is in ${MEMBER-CLUSTER-LABEL-KEY-region}",
			expectErr: true,
		},
		"ClusterLabelKey Variable key case not match": {
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"region": "us-west-1",
					},
				},
			},
			input:     "The cluster is in ${MEMBER-CLUSTER-LABEL-KEY-REGION}",
			expectErr: true,
		},
		"Invalid  clusterLabelKey variable format": {
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"region": "us-west-1",
					},
				},
			},
			input:     "The cluster is in ${MEMBER-CLUSTER-LABEL-KEY-region",
			expectErr: true,
		},
		"ClusterLabelKey variable key empty": {
			cluster: &clusterv1beta1.MemberCluster{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"region": "us-west-1",
					},
				},
			},
			input:     "The cluster is in ${MEMBER-CLUSTER-LABEL-KEY-}",
			expectErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			result, err := replaceClusterLabelKeyVariables(tc.input, tc.cluster)
			if gotErr := err != nil; gotErr != tc.expectErr {
				t.Fatalf("applyJSONPatchOverride() = error %v, want %v", err, tc.expectErr)
			}
			if result != tc.expected {
				t.Errorf("replaceClusterLabelKeyVariables() = %v, want %v", result, tc.expected)
			}
		})
	}
}

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

package workapplier

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1beta1 "go.goms.io/fleet/apis/placement/v1beta1"
	"go.goms.io/fleet/pkg/utils"
	"go.goms.io/fleet/pkg/utils/condition"
)

const (
	workNameTemplate = "work-%s"
	nsNameTemplate   = "ns-%s"
)

const (
	eventuallyDuration   = time.Second * 10
	eventuallyInterval   = time.Second * 1
	consistentlyDuration = time.Second * 5
	consistentlyInterval = time.Millisecond * 500
)

var (
	ignoreFieldObjectMetaAutoGenFields = cmpopts.IgnoreFields(metav1.ObjectMeta{}, "CreationTimestamp", "Generation", "ResourceVersion", "SelfLink", "UID", "ManagedFields")
	ignoreFieldAppliedWorkStatus       = cmpopts.IgnoreFields(fleetv1beta1.AppliedWork{}, "Status")
	ignoreFieldConditionLTTMsg         = cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime", "Message")
	ignoreDriftDetailsObsTime          = cmpopts.IgnoreFields(fleetv1beta1.DriftDetails{}, "ObservationTime", "FirstDriftedObservedTime")
	ignoreDiffDetailsObsTime           = cmpopts.IgnoreFields(fleetv1beta1.DiffDetails{}, "ObservationTime", "FirstDiffedObservedTime")

	lessFuncPatchDetail = func(a, b fleetv1beta1.PatchDetail) bool {
		return a.Path < b.Path
	}
)

var (
	dummyLabelKey    = "foo"
	dummyLabelValue1 = "bar"
	dummyLabelValue2 = "baz"
	dummyLabelValue3 = "quz"
	dummyLabelValue4 = "qux"
)

// createWorkObject creates a new Work object with the given name, manifests, and apply strategy.
func createWorkObject(workName string, applyStrategy *fleetv1beta1.ApplyStrategy, rawManifestJSON ...[]byte) {
	manifests := make([]fleetv1beta1.Manifest, len(rawManifestJSON))
	for idx := range rawManifestJSON {
		manifests[idx] = fleetv1beta1.Manifest{
			RawExtension: runtime.RawExtension{
				Raw: rawManifestJSON[idx],
			},
		}
	}

	work := &fleetv1beta1.Work{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workName,
			Namespace: memberReservedNSName,
		},
		Spec: fleetv1beta1.WorkSpec{
			Workload: fleetv1beta1.WorkloadTemplate{
				Manifests: manifests,
			},
			ApplyStrategy: applyStrategy,
		},
	}
	Expect(hubClient.Create(ctx, work)).To(Succeed())
}

func updateWorkObject(workName string, applyStrategy *fleetv1beta1.ApplyStrategy, rawManifestJSON ...[]byte) {
	manifests := make([]fleetv1beta1.Manifest, len(rawManifestJSON))
	for idx := range rawManifestJSON {
		manifests[idx] = fleetv1beta1.Manifest{
			RawExtension: runtime.RawExtension{
				Raw: rawManifestJSON[idx],
			},
		}
	}

	work := &fleetv1beta1.Work{}
	Expect(hubClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, work)).To(Succeed())

	work.Spec.Workload.Manifests = manifests
	work.Spec.ApplyStrategy = applyStrategy
	Expect(hubClient.Update(ctx, work)).To(Succeed())
}

func marshalK8sObjJSON(obj runtime.Object) []byte {
	unstructuredObjMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	Expect(err).To(BeNil(), "Failed to convert the object to an unstructured object")
	unstructuredObj := &unstructured.Unstructured{Object: unstructuredObjMap}
	json, err := unstructuredObj.MarshalJSON()
	Expect(err).To(BeNil(), "Failed to marshal the unstructured object to JSON")
	return json
}

func workFinalizerAddedActual(workName string) func() error {
	return func() error {
		// Retrieve the Work object.
		work := &fleetv1beta1.Work{}
		if err := hubClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, work); err != nil {
			return fmt.Errorf("failed to retrieve the Work object: %w", err)
		}

		// Check that the cleanup finalizer has been added.
		if !controllerutil.ContainsFinalizer(work, fleetv1beta1.WorkFinalizer) {
			return fmt.Errorf("cleanup finalizer has not been added")
		}
		return nil
	}
}

func appliedWorkCreatedActual(workName string) func() error {
	return func() error {
		// Retrieve the AppliedWork object.
		appliedWork := &fleetv1beta1.AppliedWork{}
		if err := memberClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, appliedWork); err != nil {
			return fmt.Errorf("failed to retrieve the AppliedWork object: %w", err)
		}

		wantAppliedWork := &fleetv1beta1.AppliedWork{
			ObjectMeta: metav1.ObjectMeta{
				Name: workName,
			},
			Spec: fleetv1beta1.AppliedWorkSpec{
				WorkName:      workName,
				WorkNamespace: memberReservedNSName,
			},
		}
		if diff := cmp.Diff(
			appliedWork, wantAppliedWork,
			ignoreFieldObjectMetaAutoGenFields,
			ignoreFieldAppliedWorkStatus,
		); diff != "" {
			return fmt.Errorf("appliedWork diff (-got +want):\n%s", diff)
		}
		return nil
	}
}

func prepareAppliedWorkOwnerRef(workName string) *metav1.OwnerReference {
	// Retrieve the AppliedWork object.
	appliedWork := &fleetv1beta1.AppliedWork{}
	Expect(memberClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, appliedWork)).To(Succeed(), "Failed to retrieve the AppliedWork object")

	// Prepare the expected OwnerReference.
	return &metav1.OwnerReference{
		APIVersion:         fleetv1beta1.GroupVersion.String(),
		Kind:               "AppliedWork",
		Name:               appliedWork.Name,
		UID:                appliedWork.GetUID(),
		BlockOwnerDeletion: ptr.To(true),
	}
}

func regularNSObjectAppliedActual(nsName string, appliedWorkOwnerRef *metav1.OwnerReference) func() error {
	return func() error {
		// Retrieve the NS object.
		gotNS := &corev1.Namespace{}
		if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, gotNS); err != nil {
			return fmt.Errorf("failed to retrieve the NS object: %w", err)
		}

		// Check that the NS object has been created as expected.

		// To ignore default values automatically, here the test suite rebuilds the objects.
		wantNS := ns.DeepCopy()
		wantNS.TypeMeta = metav1.TypeMeta{}
		wantNS.Name = nsName
		wantNS.OwnerReferences = []metav1.OwnerReference{
			*appliedWorkOwnerRef,
		}

		rebuiltGotNS := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:            gotNS.Name,
				OwnerReferences: gotNS.OwnerReferences,
			},
		}

		if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
			return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
		}
		return nil
	}
}

func regularDeploymentObjectAppliedActual(nsName, deployName string, appliedWorkOwnerRef *metav1.OwnerReference) func() error {
	return func() error {
		// Retrieve the Deployment object.
		gotDeploy := &appsv1.Deployment{}
		if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, gotDeploy); err != nil {
			return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
		}

		// Check that the Deployment object has been created as expected.

		// To ignore default values automatically, here the test suite rebuilds the objects.
		wantDeploy := deploy.DeepCopy()
		wantDeploy.TypeMeta = metav1.TypeMeta{}
		wantDeploy.Namespace = nsName
		wantDeploy.Name = deployName
		wantDeploy.OwnerReferences = []metav1.OwnerReference{
			*appliedWorkOwnerRef,
		}

		if len(gotDeploy.Spec.Template.Spec.Containers) != 1 {
			return fmt.Errorf("number of containers in the Deployment object, got %d, want %d", len(gotDeploy.Spec.Template.Spec.Containers), 1)
		}
		if len(gotDeploy.Spec.Template.Spec.Containers[0].Ports) != 1 {
			return fmt.Errorf("number of ports in the first container, got %d, want %d", len(gotDeploy.Spec.Template.Spec.Containers[0].Ports), 1)
		}
		rebuiltGotDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       gotDeploy.Namespace,
				Name:            gotDeploy.Name,
				OwnerReferences: gotDeploy.OwnerReferences,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: gotDeploy.Spec.Replicas,
				Selector: gotDeploy.Spec.Selector,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": gotDeploy.Spec.Template.ObjectMeta.Labels["app"],
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  gotDeploy.Spec.Template.Spec.Containers[0].Name,
								Image: gotDeploy.Spec.Template.Spec.Containers[0].Image,
								Ports: []corev1.ContainerPort{
									{
										ContainerPort: gotDeploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort,
									},
								},
							},
						},
					},
				},
			},
		}
		if diff := cmp.Diff(rebuiltGotDeploy, wantDeploy); diff != "" {
			return fmt.Errorf("deployment diff (-got +want):\n%s", diff)
		}
		return nil
	}
}

func regularClusterRoleObjectAppliedActual(clusterRoleName string, appliedWorkOwnerRef *metav1.OwnerReference) func() error {
	return func() error {
		// Retrieve the ClusterRole object.
		gotClusterRole := &rbacv1.ClusterRole{}
		if err := memberClient.Get(ctx, client.ObjectKey{Name: clusterRoleName}, gotClusterRole); err != nil {
			return fmt.Errorf("failed to retrieve the ClusterRole object: %w", err)
		}

		// Check that the ClusterRole object has been created as expected.

		// To ignore default values automatically, here the test suite rebuilds the objects.
		wantClusterRole := clusterRole.DeepCopy()
		wantClusterRole.TypeMeta = metav1.TypeMeta{}
		wantClusterRole.Name = clusterRoleName
		wantClusterRole.OwnerReferences = []metav1.OwnerReference{
			*appliedWorkOwnerRef,
		}

		rebuiltGotClusterRole := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name:            gotClusterRole.Name,
				OwnerReferences: gotClusterRole.OwnerReferences,
			},
			Rules: gotClusterRole.Rules,
		}

		if diff := cmp.Diff(rebuiltGotClusterRole, wantClusterRole); diff != "" {
			return fmt.Errorf("clusterRole diff (-got +want):\n%s", diff)
		}
		return nil
	}
}

func regularConfigMapObjectAppliedActual(nsName, configMapName string, appliedWorkOwnerRef *metav1.OwnerReference) func() error {
	return func() error {
		// Retrieve the ConfigMap object.
		gotConfigMap := &corev1.ConfigMap{}
		if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: configMapName}, gotConfigMap); err != nil {
			return fmt.Errorf("failed to retrieve the ConfigMap object: %w", err)
		}

		// Check that the ConfigMap object has been created as expected.

		// To ignore default values automatically, here the test suite rebuilds the objects.
		wantConfigMap := configMap.DeepCopy()
		wantConfigMap.TypeMeta = metav1.TypeMeta{}
		wantConfigMap.Namespace = nsName
		wantConfigMap.Name = configMapName
		wantConfigMap.OwnerReferences = []metav1.OwnerReference{
			*appliedWorkOwnerRef,
		}

		rebuiltGotConfigMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:            gotConfigMap.Name,
				Namespace:       gotConfigMap.Namespace,
				OwnerReferences: gotConfigMap.OwnerReferences,
			},
			Data: gotConfigMap.Data,
		}
		if diff := cmp.Diff(rebuiltGotConfigMap, wantConfigMap); diff != "" {
			return fmt.Errorf("configmap diff (-got +want):\n%s", diff)
		}
		return nil
	}
}

func markDeploymentAsAvailable(nsName, deployName string) {
	// Retrieve the Deployment object.
	gotDeploy := &appsv1.Deployment{}
	Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, gotDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")

	// Mark the Deployment object as available.
	now := metav1.Now()
	requiredReplicas := int32(1)
	if gotDeploy.Spec.Replicas != nil {
		requiredReplicas = *gotDeploy.Spec.Replicas
	}
	gotDeploy.Status = appsv1.DeploymentStatus{
		ObservedGeneration:  gotDeploy.Generation,
		Replicas:            requiredReplicas,
		UpdatedReplicas:     requiredReplicas,
		ReadyReplicas:       requiredReplicas,
		AvailableReplicas:   requiredReplicas,
		UnavailableReplicas: 0,
		Conditions: []appsv1.DeploymentCondition{
			{
				Type:               appsv1.DeploymentAvailable,
				Status:             corev1.ConditionTrue,
				Reason:             "MarkedAsAvailable",
				Message:            "Deployment has been marked as available",
				LastUpdateTime:     now,
				LastTransitionTime: now,
			},
		},
	}
	Expect(memberClient.Status().Update(ctx, gotDeploy)).To(Succeed(), "Failed to mark the Deployment object as available")
}

func workStatusUpdated(
	workName string,
	workConds []metav1.Condition,
	manifestConds []fleetv1beta1.ManifestCondition,
	noLaterThanObservationTime *metav1.Time,
	noLaterThanFirstObservedTime *metav1.Time,
) func() error {
	return func() error {
		// Retrieve the Work object.
		work := &fleetv1beta1.Work{}
		if err := hubClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, work); err != nil {
			return fmt.Errorf("failed to retrieve the Work object: %w", err)
		}

		// Prepare the expected Work object status.

		// Update the conditions with the observed generation.
		//
		// Note that the observed generation of a manifest condition is that of an applied
		// resource, not that of the Work object.
		for idx := range workConds {
			workConds[idx].ObservedGeneration = work.Generation
		}
		wantWorkStatus := fleetv1beta1.WorkStatus{
			Conditions:         workConds,
			ManifestConditions: manifestConds,
		}

		// Check that the Work object status has been updated as expected.
		if diff := cmp.Diff(
			work.Status, wantWorkStatus,
			ignoreFieldConditionLTTMsg,
			ignoreDiffDetailsObsTime, ignoreDriftDetailsObsTime,
			cmpopts.SortSlices(lessFuncPatchDetail),
		); diff != "" {
			return fmt.Errorf("work status diff (-got, +want):\n%s", diff)
		}

		// For each manifest condition, verify the timestamps.
		for idx := range work.Status.ManifestConditions {
			manifestCond := &work.Status.ManifestConditions[idx]
			if manifestCond.DriftDetails != nil {
				if noLaterThanObservationTime != nil && manifestCond.DriftDetails.ObservationTime.After(noLaterThanObservationTime.Time) {
					return fmt.Errorf("drift observation time is later than expected (observed: %v, no later than: %v)", manifestCond.DriftDetails.ObservationTime, noLaterThanObservationTime)
				}

				if noLaterThanFirstObservedTime != nil && manifestCond.DriftDetails.FirstDriftedObservedTime.After(noLaterThanFirstObservedTime.Time) {
					return fmt.Errorf("first drifted observation time is later than expected (observed: %v, no later than: %v)", manifestCond.DriftDetails.FirstDriftedObservedTime, noLaterThanFirstObservedTime)
				}

				// The drift observation time can be equal or later than the first drifted observation time.
				if manifestCond.DriftDetails.ObservationTime.Before(&manifestCond.DriftDetails.FirstDriftedObservedTime) {
					return fmt.Errorf("drift observation time is later than first drifted observation time (observed: %v, first observed: %v)", manifestCond.DriftDetails.ObservationTime, manifestCond.DriftDetails.FirstDriftedObservedTime)
				}
			}

			if manifestCond.DiffDetails != nil {
				if noLaterThanObservationTime != nil && manifestCond.DiffDetails.ObservationTime.After(noLaterThanObservationTime.Time) {
					return fmt.Errorf("diff observation time is later than expected (observed: %v, no later than: %v)", manifestCond.DiffDetails.ObservationTime, noLaterThanObservationTime)
				}

				if noLaterThanFirstObservedTime != nil && manifestCond.DiffDetails.FirstDiffedObservedTime.After(noLaterThanFirstObservedTime.Time) {
					return fmt.Errorf("first diffed observation time is later than expected (observed: %v, no later than: %v)", manifestCond.DiffDetails.FirstDiffedObservedTime, noLaterThanFirstObservedTime)
				}

				// The diff observation time can be equal or later than the first diffed observation time.
				if manifestCond.DiffDetails.ObservationTime.Before(&manifestCond.DiffDetails.FirstDiffedObservedTime) {
					return fmt.Errorf("diff observation time is before the first diffed observation time (observed: %v, first observed: %v)", manifestCond.DiffDetails.ObservationTime, manifestCond.DiffDetails.FirstDiffedObservedTime)
				}
			}
		}
		return nil
	}
}

func appliedWorkStatusUpdated(workName string, appliedResourceMeta []fleetv1beta1.AppliedResourceMeta) func() error {
	return func() error {
		// Retrieve the AppliedWork object.
		appliedWork := &fleetv1beta1.AppliedWork{}
		if err := memberClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, appliedWork); err != nil {
			return fmt.Errorf("failed to retrieve the AppliedWork object: %w", err)
		}

		// Prepare the expected AppliedWork object status.
		wantAppliedWorkStatus := fleetv1beta1.AppliedWorkStatus{
			AppliedResources: appliedResourceMeta,
		}
		if diff := cmp.Diff(appliedWork.Status, wantAppliedWorkStatus); diff != "" {
			return fmt.Errorf("appliedWork status diff (-got, +want):\n%s", diff)
		}
		return nil
	}
}

func workRemovedActual(workName string) func() error {
	// Wait for the removal of the Work object.
	return func() error {
		work := &fleetv1beta1.Work{}
		if err := hubClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, work); !errors.IsNotFound(err) && err != nil {
			return fmt.Errorf("work object still exists or an unexpected error occurred: %w", err)
		}
		if controllerutil.ContainsFinalizer(work, fleetv1beta1.WorkFinalizer) {
			// The Work object is being deleted, but the finalizer is still present.
			return fmt.Errorf("work object is being deleted, but the finalizer is still present")
		}
		return nil
	}
}

func deleteWorkObject(workName string) {
	// Retrieve the Work object.
	work := &fleetv1beta1.Work{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workName,
			Namespace: memberReservedNSName,
		},
	}
	Expect(hubClient.Delete(ctx, work)).To(Succeed(), "Failed to delete the Work object")
}

func checkNSOwnerReferences(workName, nsName string) {
	// Retrieve the AppliedWork object.
	appliedWork := &fleetv1beta1.AppliedWork{}
	Expect(memberClient.Get(ctx, client.ObjectKey{Name: workName}, appliedWork)).To(Succeed(), "Failed to retrieve the AppliedWork object")

	// Check that the Namespace object has the AppliedWork as an owner reference.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: nsName,
		},
	}
	Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, ns)).To(Succeed(), "Failed to retrieve the Namespace object")
	Expect(ns.OwnerReferences).To(ContainElement(metav1.OwnerReference{
		APIVersion:         fleetv1beta1.GroupVersion.String(),
		Kind:               "AppliedWork",
		Name:               appliedWork.Name,
		UID:                appliedWork.GetUID(),
		BlockOwnerDeletion: ptr.To(true),
	}), " AppliedWork OwnerReference not found in Namespace object")
}

func appliedWorkRemovedActual(workName, nsName string) func() error {
	return func() error {
		// Retrieve the AppliedWork object.
		appliedWork := &fleetv1beta1.AppliedWork{}
		if err := memberClient.Get(ctx, client.ObjectKey{Name: workName}, appliedWork); err != nil {
			if errors.IsNotFound(err) {
				// The AppliedWork object has been deleted, which is expected.
				return nil
			}
			return fmt.Errorf("failed to retrieve the AppliedWork object: %w", err)
		}
		if !appliedWork.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(appliedWork, metav1.FinalizerDeleteDependents) {
			// The AppliedWork object is being deleted, but the finalizer is still present. Remove the finalizer as there
			// are no real built-in controllers in this test environment to handle garbage collection.
			controllerutil.RemoveFinalizer(appliedWork, metav1.FinalizerDeleteDependents)
			Expect(memberClient.Update(ctx, appliedWork)).To(Succeed(), "Failed to remove the finalizer from the AppliedWork object")
		}
		return fmt.Errorf("appliedWork object still exists")
	}
}

func regularDeployRemovedActual(nsName, deployName string) func() error {
	return func() error {
		// Retrieve the Deployment object.
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: nsName,
				Name:      deployName,
			},
		}
		if err := memberClient.Delete(ctx, deploy); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete the Deployment object: %w", err)
		}

		if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, deploy); !errors.IsNotFound(err) {
			return fmt.Errorf("deployment object still exists or an unexpected error occurred: %w", err)
		}
		return nil
	}
}

func regularClusterRoleRemovedActual(clusterRoleName string) func() error {
	return func() error {
		// Retrieve the ClusterRole object.
		clusterRole := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: clusterRoleName,
			},
		}
		if err := memberClient.Delete(ctx, clusterRole); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete the ClusterRole object: %w", err)
		}

		if err := memberClient.Get(ctx, client.ObjectKey{Name: clusterRoleName}, clusterRole); !errors.IsNotFound(err) {
			return fmt.Errorf("clusterRole object still exists or an unexpected error occurred: %w", err)
		}
		return nil
	}
}

func regularConfigMapRemovedActual(nsName, configMapName string) func() error {
	return func() error {
		// Retrieve the ConfigMap object.
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: nsName,
				Name:      configMapName,
			},
		}
		if err := memberClient.Delete(ctx, configMap); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("failed to delete the ConfigMap object: %w", err)
		}

		// Check that the ConfigMap object has been deleted.
		if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: configMapName}, configMap); !errors.IsNotFound(err) {
			return fmt.Errorf("configMap object still exists or an unexpected error occurred: %w", err)
		}
		return nil
	}
}

func regularNSObjectNotAppliedActual(nsName string) func() error {
	return func() error {
		// Retrieve the NS object.
		ns := &corev1.Namespace{}
		if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, ns); !errors.IsNotFound(err) {
			return fmt.Errorf("namespace object exists or an unexpected error occurred: %w", err)
		}
		return nil
	}
}

func regularDeployNotRemovedActual(nsName, deployName string) func() error {
	return func() error {
		// Retrieve the Deployment object.
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: nsName,
				Name:      deployName,
			},
		}
		if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, deploy); err != nil {
			return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
		}
		return nil
	}
}

var _ = Describe("applying manifests", func() {
	Context("apply new manifests (regular)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create a new Work object with all the manifest JSONs.
			createWorkObject(workName, nil, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the Deployment object has been applied as expected.
			regularDeploymentObjectAppliedActual := regularDeploymentObjectAppliedActual(nsName, deployName, appliedWorkOwnerRef)
			Eventually(regularDeploymentObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the deployment object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")
		})

		It("can mark the deployment as available", func() {
			markDeploymentAsAvailable(nsName, deployName)
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 1,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 1,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure applied manifest has been removed.
			regularDeployRemovedActual := regularDeployRemovedActual(nsName, deployName)
			Eventually(regularDeployRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("garbage collect removed manifests", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create a new Work object with all the manifest JSONs.
			createWorkObject(workName, nil, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the Deployment object has been applied as expected.
			regularDeploymentObjectAppliedActual := regularDeploymentObjectAppliedActual(nsName, deployName, appliedWorkOwnerRef)
			Eventually(regularDeploymentObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the deployment object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")
		})

		It("can mark the deployment as available", func() {
			markDeploymentAsAvailable(nsName, deployName)
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 1,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 1,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can delete some manifests", func() {
			// Update the work object and remove the Deployment manifest.

			// Re-prepare the JSON to make sure that type meta info. is included correctly.
			regularNS := ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			updateWorkObject(workName, nil, regularNSJSON)
		})

		It("should garbage collect removed manifests", func() {
			deployRemovedActual := regularDeployRemovedActual(nsName, deployName)
			Eventually(deployRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the deployment object")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")
			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("should handle objects with generate names properly", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())

		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		nsGenerateName := "work-"
		deployGenerateName := "deploy-foo-"

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object with both generate name and name.
			// This should be handled by the work applier properly.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNS.GenerateName = nsGenerateName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object with only generate name.
			// This should be rejected by the work applier.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = ""
			regularDeploy.GenerateName = deployGenerateName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create a new Work object with all the manifest JSONs.
			createWorkObject(workName, nil, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply some of the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			Eventually(func() error {
				// Retrieve the NS object.
				gotNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, gotNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// Check that the NS object has been created as expected.

				// To ignore default values automatically, here the test suite rebuilds the objects.
				wantNS := ns.DeepCopy()
				wantNS.TypeMeta = metav1.TypeMeta{}
				wantNS.Name = nsName
				wantNS.GenerateName = nsGenerateName
				wantNS.OwnerReferences = []metav1.OwnerReference{
					*appliedWorkOwnerRef,
				}

				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            gotNS.Name,
						GenerateName:    gotNS.GenerateName,
						OwnerReferences: gotNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")
		})

		It("should not apply the Deployment object", func() {
			Consistently(func() error {
				// List all Deployments.
				gotDeployList := &appsv1.DeploymentList{}
				if err := memberClient.List(ctx, gotDeployList, client.InNamespace(nsName)); err != nil {
					return fmt.Errorf("failed to list Deployment objects: %w", err)
				}

				for _, gotDeploy := range gotDeployList.Items {
					if gotDeploy.GenerateName == deployGenerateName {
						return fmt.Errorf("found a Deployment object with generate name that should not be applied")
					}
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Applied the deployment object; expected an error")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFoundGenerateName),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("can handle partial failures (pre-processing, decoding error)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var decodingErredDeploy *appsv1.Deployment
		var regularConfigMap *corev1.ConfigMap

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a mal-formed Deployment object.
			decodingErredDeploy = deploy.DeepCopy()
			decodingErredDeploy.TypeMeta = metav1.TypeMeta{
				APIVersion: "dummy/v10",
				Kind:       "Fake",
			}
			decodingErredDeploy.Namespace = nsName
			decodingErredDeploy.Name = deployName
			decodingErredDeployJSON := marshalK8sObjJSON(decodingErredDeploy)

			// Prepare a ConfigMap object.
			regularConfigMap = configMap.DeepCopy()
			regularConfigMap.Namespace = nsName
			regularConfigMapJSON := marshalK8sObjJSON(regularConfigMap)

			// Create a new Work object with all the manifest JSONs.
			createWorkObject(workName, nil, regularNSJSON, decodingErredDeployJSON, regularConfigMapJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the ConfigMap object has been applied as expected.
			regularConfigMapObjectAppliedActual := regularConfigMapObjectAppliedActual(nsName, configMapName, appliedWorkOwnerRef)
			Eventually(regularConfigMapObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the ConfigMap object")
			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: configMapName}, regularConfigMap)).To(Succeed(), "Failed to retrieve the ConfigMap object")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "dummy",
						Version:   "v10",
						Kind:      "Fake",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:   fleetv1beta1.WorkConditionTypeApplied,
							Status: metav1.ConditionFalse,
							Reason: string(ManifestProcessingApplyResultTypeDecodingErred),
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   2,
						Version:   "v1",
						Kind:      "ConfigMap",
						Resource:  "configmaps",
						Name:      configMapName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:   fleetv1beta1.WorkConditionTypeApplied,
							Status: metav1.ConditionTrue,
							Reason: string(ManifestProcessingApplyResultTypeApplied),
						},
						{
							Type:   fleetv1beta1.WorkConditionTypeAvailable,
							Status: metav1.ConditionTrue,
							Reason: string(ManifestProcessingAvailabilityResultTypeAvailable),
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   2,
						Group:     "",
						Version:   "v1",
						Kind:      "ConfigMap",
						Resource:  "configmaps",
						Name:      configMapName,
						Namespace: nsName,
					},
					UID: regularConfigMap.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure applied manifest has been removed.
			regularConfigMapRemovedActual := regularConfigMapRemovedActual(nsName, configMapName)
			Eventually(regularConfigMapRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the ConfigMap object")

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})
})

var _ = Describe("work applier garbage collection", func() {
	Context("update owner reference with blockOwnerDeletion to false (other owner reference does not exist)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())
		anotherOwnerReference := metav1.OwnerReference{
			APIVersion: "another-api-version",
			Kind:       "another-kind",
			Name:       "another-owner",
			UID:        "another-uid",
		}

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create a new Work object with all the manifest JSONs.
			createWorkObject(workName, &fleetv1beta1.ApplyStrategy{AllowCoOwnership: true}, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the Deployment object has been applied as expected.
			regularDeploymentObjectAppliedActual := regularDeploymentObjectAppliedActual(nsName, deployName, appliedWorkOwnerRef)
			Eventually(regularDeploymentObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the deployment object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")
		})

		It("can mark the deployment as available", func() {
			markDeploymentAsAvailable(nsName, deployName)
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 1,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 1,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can update Deployment object to add another owner reference", func() {
			// Retrieve the Deployment object.
			gotDeploy := &appsv1.Deployment{}
			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, gotDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")

			// Add another owner reference to the Deployment object.
			gotDeploy.OwnerReferences = append(gotDeploy.OwnerReferences, anotherOwnerReference)
			Expect(memberClient.Update(ctx, gotDeploy)).To(Succeed(), "Failed to update the Deployment object with another owner reference")

			// Ensure that the Deployment object has been updated as expected.
			Eventually(func() error {
				// Retrieve the Deployment object again.
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, gotDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				// Check that the Deployment object has been updated as expected.
				if len(gotDeploy.OwnerReferences) != 2 {
					return fmt.Errorf("expected 2 owner references, got %d", len(gotDeploy.OwnerReferences))
				}
				for _, ownerRef := range gotDeploy.OwnerReferences {
					if ownerRef.APIVersion == anotherOwnerReference.APIVersion &&
						ownerRef.Kind == anotherOwnerReference.Kind &&
						ownerRef.Name == anotherOwnerReference.Name &&
						ownerRef.UID == anotherOwnerReference.UID {
						return nil
					}
				}
				return fmt.Errorf("another owner reference not found in the Deployment object")
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to find another owner reference on Deployment")
		})

		It("should start deleting the Work object", func() {
			// Start deleting the Work object.
			deleteWorkObject(workName)
		})

		It("should start deleting the AppliedWork object", func() {
			// Ensure that the Work object is being deleted.
			Eventually(func() error {
				appliedWork := &fleetv1beta1.AppliedWork{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: workName}, appliedWork); err != nil {
					return err
				}
				if !appliedWork.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(appliedWork, metav1.FinalizerDeleteDependents) {
					return fmt.Errorf("appliedWork object still is not being deleted")
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to start deleting the AppliedWork object")

			// Explicitly wait a minute to let the deletion timestamp progress
			time.Sleep(30 * time.Second)
		})

		It("should update owner reference from the Deployment object", func() {
			// Ensure that the Deployment object has been updated applied owner reference with blockOwnerDeletion to false.
			Eventually(func() error {
				// Retrieve the Deployment object again.
				gotDeploy := &appsv1.Deployment{}
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, gotDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}
				// Check that the Deployment object has been updated as expected.
				for _, ownerRef := range gotDeploy.OwnerReferences {
					if ownerRef.APIVersion == fleetv1beta1.GroupVersion.String() &&
						ownerRef.Kind == fleetv1beta1.AppliedWorkKind &&
						ownerRef.Name == workName &&
						ownerRef.UID == appliedWorkOwnerRef.UID {
						if *ownerRef.BlockOwnerDeletion {
							return fmt.Errorf("owner reference from AppliedWork still has BlockOwnerDeletion set to true")
						}
					}
				}
				return nil
			}, 2*eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove owner reference from Deployment")
		})

		AfterAll(func() {
			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, 2*time.Minute, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// Ensure that the Deployment object still exists.
			Consistently(func() error {
				return memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Deployment object has been removed unexpectedly")
			// Delete objects created by the test suite so that the next test case can run without issues.
			Expect(memberClient.Delete(ctx, regularDeploy)).To(Succeed(), "Failed to delete the Deployment object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("update owner reference with blockOwnerDeletion to false (other owner reference invalid)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment
		var regularClusterRole *rbacv1.ClusterRole

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Prepare a ClusterRole object.
			regularClusterRole = clusterRole.DeepCopy()
			regularClusterRole.Name = clusterRoleName
			regularClusterRoleJSON := marshalK8sObjJSON(regularClusterRole)

			// Create a new Work object with all the manifest JSONs.
			createWorkObject(workName, &fleetv1beta1.ApplyStrategy{AllowCoOwnership: true}, regularNSJSON, regularDeployJSON, regularClusterRoleJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the Deployment object has been applied as expected.
			regularDeploymentObjectAppliedActual := regularDeploymentObjectAppliedActual(nsName, deployName, appliedWorkOwnerRef)
			Eventually(regularDeploymentObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the deployment object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")

			// Ensure that the ClusterRole object has been applied as expected.
			regularClusterRoleObjectAppliedActual := regularClusterRoleObjectAppliedActual(clusterRoleName, appliedWorkOwnerRef)
			Eventually(regularClusterRoleObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the clusterRole object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: clusterRoleName}, regularClusterRole)).To(Succeed(), "Failed to retrieve the clusterRole object")
		})

		It("can mark the deployment as available", func() {
			markDeploymentAsAvailable(nsName, deployName)
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 1,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 1,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  2,
						Group:    "rbac.authorization.k8s.io",
						Version:  "v1",
						Kind:     "ClusterRole",
						Resource: "clusterroles",
						Name:     clusterRoleName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  2,
						Group:    "rbac.authorization.k8s.io",
						Version:  "v1",
						Kind:     "ClusterRole",
						Resource: "clusterroles",
						Name:     clusterRoleName,
					},
					UID: regularClusterRole.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can update ClusterRole object to add another owner reference", func() {
			// Retrieve the ClusterRole object.
			gotClusterRole := &rbacv1.ClusterRole{}
			Expect(memberClient.Get(ctx, client.ObjectKey{Name: clusterRoleName}, gotClusterRole)).To(Succeed(), "Failed to retrieve the ClusterRole object")

			// Retrieve the Deployment object.
			gotDeploy := &appsv1.Deployment{}
			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, gotDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")

			// Add another owner reference to the ClusterRole object.
			// Note: This is an invalid owner reference, as it adds a namespace-scoped object as an owner of a cluster-scoped object.
			gotClusterRole.OwnerReferences = append(gotClusterRole.OwnerReferences, metav1.OwnerReference{
				APIVersion: appsv1.SchemeGroupVersion.String(),
				Kind:       "Deployment",
				Name:       gotDeploy.Name,
				UID:        gotDeploy.UID,
			})
			Expect(memberClient.Update(ctx, gotClusterRole)).To(Succeed(), "Failed to update the ClusterRole object with another owner reference")

			// Ensure that the ClusterRole object has been updated as expected.
			Eventually(func() error {
				// Retrieve the ClusterRole object again.
				if err := memberClient.Get(ctx, client.ObjectKey{Name: clusterRoleName}, gotClusterRole); err != nil {
					return fmt.Errorf("failed to retrieve the ClusterRole object: %w", err)
				}

				// Check that the ClusterRole object has been updated as expected.
				if len(gotClusterRole.OwnerReferences) != 2 {
					return fmt.Errorf("expected 2 owner references, got %d", len(gotClusterRole.OwnerReferences))
				}
				for _, ownerRef := range gotClusterRole.OwnerReferences {
					if ownerRef.APIVersion == appsv1.SchemeGroupVersion.String() &&
						ownerRef.Kind == "Deployment" &&
						ownerRef.Name == gotDeploy.Name &&
						ownerRef.UID == gotDeploy.UID {
						return nil
					}
				}
				return fmt.Errorf("another owner reference not found in the ClusterRole object")
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to find another owner reference on ClusterRole")
		})

		It("should start deleting the Work object", func() {
			// Start deleting the Work object.
			deleteWorkObject(workName)
		})

		It("should start deleting the AppliedWork object", func() {
			// Ensure that the Work object is being deleted.
			Eventually(func() error {
				appliedWork := &fleetv1beta1.AppliedWork{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: workName}, appliedWork); err != nil {
					return err
				}
				if !appliedWork.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(appliedWork, metav1.FinalizerDeleteDependents) {
					return fmt.Errorf("appliedWork object still is not being deleted")
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to start deleting the AppliedWork object")

			// Explicitly wait a minute to let the deletion timestamp progress
			time.Sleep(30 * time.Second)
		})

		It("should update owner reference from the ClusterRole object", func() {
			// Ensure that the ClusterRole object has been updated with AppliedWork owner reference to have BlockOwnerDeletion set to false.
			Eventually(func() error {
				// Retrieve the ClusterRole object again.
				gotClusterRole := &rbacv1.ClusterRole{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: clusterRoleName}, gotClusterRole); err != nil {
					return fmt.Errorf("failed to retrieve the ClusterRole object: %w", err)
				}

				// Check that the ClusterRole object has been updated as expected.
				for _, ownerRef := range gotClusterRole.OwnerReferences {
					if ownerRef.APIVersion == appliedWorkOwnerRef.APIVersion &&
						ownerRef.Kind == appliedWorkOwnerRef.Kind &&
						ownerRef.Name == appliedWorkOwnerRef.Name &&
						ownerRef.UID == appliedWorkOwnerRef.UID && *ownerRef.BlockOwnerDeletion {
						return fmt.Errorf("owner reference from AppliedWork still has BlockOwnerDeletion set to true")
					}
				}

				return nil
			}, 2*eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove owner reference from ClusterRole")
		})

		AfterAll(func() {
			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure applied manifest has been removed.
			regularDeployRemovedActual := regularDeployRemovedActual(nsName, deployName)
			Eventually(regularDeployRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, 2*time.Minute, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// Ensure that the ClusterRole object still exists.
			Consistently(func() error {
				return memberClient.Get(ctx, client.ObjectKey{Name: clusterRoleName}, regularClusterRole)
			}, consistentlyDuration, consistentlyInterval).Should(BeNil(), "ClusterRole object has been removed unexpectedly")
			// Delete objects created by the test suite so that the next test case can run without issues.
			Expect(memberClient.Delete(ctx, regularClusterRole)).To(Succeed(), "Failed to delete the clusterRole object")
			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("update owner reference with blockOwnerDeletion to false (other owner reference valid)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment
		var regularClusterRole *rbacv1.ClusterRole

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Prepare a ClusterRole object.
			regularClusterRole = clusterRole.DeepCopy()
			regularClusterRole.Name = clusterRoleName
			regularClusterRoleJSON := marshalK8sObjJSON(regularClusterRole)

			// Create a new Work object with all the manifest JSONs.
			createWorkObject(workName, &fleetv1beta1.ApplyStrategy{AllowCoOwnership: true}, regularNSJSON, regularDeployJSON, regularClusterRoleJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the Deployment object has been applied as expected.
			regularDeploymentObjectAppliedActual := regularDeploymentObjectAppliedActual(nsName, deployName, appliedWorkOwnerRef)
			Eventually(regularDeploymentObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the deployment object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")

			// Ensure that the ClusterRole object has been applied as expected.
			regularClusterRoleObjectAppliedActual := regularClusterRoleObjectAppliedActual(clusterRoleName, appliedWorkOwnerRef)
			Eventually(regularClusterRoleObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the clusterRole object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: clusterRoleName}, regularClusterRole)).To(Succeed(), "Failed to retrieve the clusterRole object")
		})

		It("can mark the deployment as available", func() {
			markDeploymentAsAvailable(nsName, deployName)
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 1,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 1,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  2,
						Group:    "rbac.authorization.k8s.io",
						Version:  "v1",
						Kind:     "ClusterRole",
						Resource: "clusterroles",
						Name:     clusterRoleName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  2,
						Group:    "rbac.authorization.k8s.io",
						Version:  "v1",
						Kind:     "ClusterRole",
						Resource: "clusterroles",
						Name:     clusterRoleName,
					},
					UID: regularClusterRole.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can update Deployment object to add another owner reference", func() {
			// Retrieve the ClusterRole object.
			gotClusterRole := &rbacv1.ClusterRole{}
			Expect(memberClient.Get(ctx, client.ObjectKey{Name: clusterRoleName}, gotClusterRole)).To(Succeed(), "Failed to retrieve the ClusterRole object")

			// Retrieve the Deployment object.
			gotDeploy := &appsv1.Deployment{}
			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, gotDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")

			// Add another owner reference to the Deployment object.
			gotDeploy.OwnerReferences = append(gotDeploy.OwnerReferences, metav1.OwnerReference{
				APIVersion: rbacv1.SchemeGroupVersion.String(),
				Kind:       "ClusterRole",
				Name:       gotClusterRole.Name,
				UID:        gotClusterRole.UID,
			})
			Expect(memberClient.Update(ctx, gotDeploy)).To(Succeed(), "Failed to update the Deployment object with another owner reference")

			// Ensure that the Deployment object has been updated as expected.
			Eventually(func() error {
				// Retrieve the Deployment object again.
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, gotDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				// Check that the Deployment object has been updated as expected.
				if len(gotDeploy.OwnerReferences) != 2 {
					return fmt.Errorf("expected 2 owner references, got %d", len(gotDeploy.OwnerReferences))
				}
				for _, ownerRef := range gotDeploy.OwnerReferences {
					if ownerRef.APIVersion == rbacv1.SchemeGroupVersion.String() &&
						ownerRef.Kind == "ClusterRole" &&
						ownerRef.Name == gotClusterRole.Name &&
						ownerRef.UID == gotClusterRole.UID {
						return nil
					}
				}
				return fmt.Errorf("another owner reference not found in the Deployment object")
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to find another owner reference on Deployment")
		})

		It("should start deleting the Work object", func() {
			// Start deleting the Work object.
			deleteWorkObject(workName)
		})

		It("should start deleting the AppliedWork object", func() {
			// Ensure that the Work object is being deleted.
			Eventually(func() error {
				appliedWork := &fleetv1beta1.AppliedWork{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: workName}, appliedWork); err != nil {
					return err
				}
				if !appliedWork.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(appliedWork, metav1.FinalizerDeleteDependents) {
					return fmt.Errorf("appliedWork object still is not being deleted")
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to start deleting the AppliedWork object")

			// Explicitly wait a minute to let the deletion timestamp progress
			time.Sleep(30 * time.Second)
		})

		It("should update owner reference from the Deployment object", func() {
			// Ensure that the Deployment object has been updated with AppliedWork owner reference to have BlockOwnerDeletion set to false.
			Eventually(func() error {
				// Retrieve the Deployment object.
				gotDeploy := &appsv1.Deployment{}
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, gotDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the ClusterRole object: %w", err)
				}

				// Check that the Deployment object has been updated as expected.
				for _, ownerRef := range gotDeploy.OwnerReferences {
					if ownerRef.APIVersion == appliedWorkOwnerRef.APIVersion &&
						ownerRef.Kind == appliedWorkOwnerRef.Kind &&
						ownerRef.Name == appliedWorkOwnerRef.Name &&
						ownerRef.UID == appliedWorkOwnerRef.UID && *ownerRef.BlockOwnerDeletion {
						return fmt.Errorf("owner reference from AppliedWork still has BlockOwnerDeletion set to true")
					}
				}
				return nil
			}, 2*eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove owner reference from Deployment")
		})

		AfterAll(func() {
			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure applied manifest has been removed.
			regularClusterRoleRemovedActual := regularClusterRoleRemovedActual(clusterRoleName)
			Eventually(regularClusterRoleRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the ClusterRole object")

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, 2*time.Minute, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// Ensure that the Deployment object still exists.
			Consistently(func() error {
				return memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)
			}, consistentlyDuration, consistentlyInterval).Should(BeNil(), "Deployment object has been removed unexpectedly")
			// Delete objects created by the test suite so that the next test case can run without issues.
			Expect(memberClient.Delete(ctx, regularDeploy)).To(Succeed(), "Failed to delete the Deployment object")
			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})
})

var _ = Describe("drift detection and takeover", func() {
	Context("take over pre-existing resources (take over if no diff, no diff present)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName

			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName

			// Prepare the JSONs for the resources.
			regularNSJSON := marshalK8sObjJSON(regularNS)
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create the resources on the member cluster side.
			Expect(memberClient.Create(ctx, regularNS)).To(Succeed(), "Failed to create the NS object")
			Expect(memberClient.Create(ctx, regularDeploy)).To(Succeed(), "Failed to create the Deployment object")

			markDeploymentAsAvailable(nsName, deployName)

			// Create the Work object.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				WhenToTakeOver:   fleetv1beta1.WhenToTakeOverTypeIfNoDiff,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the Deployment object has been applied as expected.
			regularDeploymentObjectAppliedActual := regularDeploymentObjectAppliedActual(nsName, deployName, appliedWorkOwnerRef)
			Eventually(regularDeploymentObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the deployment object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")
		})

		It("can mark the deployment as available", func() {
			markDeploymentAsAvailable(nsName, deployName)
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:   fleetv1beta1.WorkConditionTypeApplied,
							Status: metav1.ConditionTrue,
							Reason: string(ManifestProcessingApplyResultTypeApplied),
						},
						{
							Type:   fleetv1beta1.WorkConditionTypeAvailable,
							Status: metav1.ConditionTrue,
							Reason: string(ManifestProcessingAvailabilityResultTypeAvailable),
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 2,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 2,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure applied manifest has been removed.
			regularDeployRemovedActual := regularDeployRemovedActual(nsName, deployName)
			Eventually(regularDeployRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("take over pre-existing resources (take over if no diff, with diff present, partial comparison)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName

			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName

			// Prepare the JSONs for the resources.
			regularNSJSON := marshalK8sObjJSON(regularNS)
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Make cluster specific changes.

			// Labels is not a managed field; with partial comparison this variance will be
			// ignored.
			regularNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
			}
			// Replicas is a managed field; with partial comparison this variance will be noted.
			regularDeploy.Spec.Replicas = ptr.To(int32(2))

			// Create the resources on the member cluster side.
			Expect(memberClient.Create(ctx, regularNS)).To(Succeed(), "Failed to create the NS object")
			Expect(memberClient.Create(ctx, regularDeploy)).To(Succeed(), "Failed to create the Deployment object")

			markDeploymentAsAvailable(nsName, deployName)

			// Create the Work object.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				WhenToTakeOver:   fleetv1beta1.WhenToTakeOverTypeIfNoDiff,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply some manifests (while preserving diffs in unmanaged fields)", func() {
			// Verify that the object has been taken over, but all the unmanaged fields are
			// left alone.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName
			wantNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
				// The label below is added by K8s itself (system-managed well-known label).
				"kubernetes.io/metadata.name": nsName,
			}
			wantNS.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}

			Eventually(func() error {
				// Retrieve the NS object.
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            regularNS.Name,
						Labels:          regularNS.Labels,
						OwnerReferences: regularNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to take over the NS object")
		})

		It("should not take over some objects", func() {
			// Verify that the object has not been taken over.
			wantDeploy := deploy.DeepCopy()
			wantDeploy.TypeMeta = metav1.TypeMeta{}
			wantDeploy.Namespace = nsName
			wantDeploy.Name = deployName
			wantDeploy.Spec.Replicas = ptr.To(int32(2))

			Consistently(func() error {
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				if len(regularDeploy.Spec.Template.Spec.Containers) != 1 {
					return fmt.Errorf("number of containers in the Deployment object, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers), 1)
				}
				if len(regularDeploy.Spec.Template.Spec.Containers[0].Ports) != 1 {
					return fmt.Errorf("number of ports in the first container, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers[0].Ports), 1)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotDeploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       regularDeploy.Namespace,
						Name:            regularDeploy.Name,
						OwnerReferences: regularDeploy.OwnerReferences,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: regularDeploy.Spec.Replicas,
						Selector: regularDeploy.Spec.Selector,
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: regularDeploy.Spec.Template.ObjectMeta.Labels,
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  regularDeploy.Spec.Template.Spec.Containers[0].Name,
										Image: regularDeploy.Spec.Template.Spec.Containers[0].Image,
										Ports: []corev1.ContainerPort{
											{
												ContainerPort: regularDeploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort,
											},
										},
									},
								},
							},
						},
					},
				}

				if diff := cmp.Diff(rebuiltGotDeploy, wantDeploy); diff != "" {
					return fmt.Errorf("deployment diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to leave the Deployment object alone")
		})

		It("should update the Work object status", func() {
			noLaterThanTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFailedToTakeOver),
							ObservedGeneration: 1,
						},
					},
					DiffDetails: &fleetv1beta1.DiffDetails{
						ObservedInMemberClusterGeneration: &regularDeploy.Generation,
						ObservedDiffs: []fleetv1beta1.PatchDetail{
							{
								Path:          "/spec/replicas",
								ValueInMember: "2",
								ValueInHub:    "1",
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &noLaterThanTimestamp, &noLaterThanTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure that the Deployment object has been left alone.
			regularDeployNotRemovedActual := regularDeployNotRemovedActual(nsName, deployName)
			Consistently(regularDeployNotRemovedActual, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("take over pre-existing resources (take over if no diff, with diff, full comparison)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName

			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName

			// Prepare the JSONs for the resources.
			regularNSJSON := marshalK8sObjJSON(regularNS)
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Make cluster specific changes.

			// Labels is not a managed field; with partial comparison this variance will be
			// ignored.
			regularNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
			}
			// Replicas is a managed field; with partial comparison this variance will be noted.
			regularDeploy.Spec.Replicas = ptr.To(int32(2))

			// Create the resources on the member cluster side.
			Expect(memberClient.Create(ctx, regularNS)).To(Succeed(), "Failed to create the NS object")
			Expect(memberClient.Create(ctx, regularDeploy)).To(Succeed(), "Failed to create the Deployment object")

			markDeploymentAsAvailable(nsName, deployName)

			// Create the Work object.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypeFullComparison,
				WhenToTakeOver:   fleetv1beta1.WhenToTakeOverTypeIfNoDiff,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")
		})

		It("should not take over any object", func() {
			// Verify that the NS object has not been taken over.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName
			wantNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
				// The label below is added by K8s itself (system-managed well-known label).
				"kubernetes.io/metadata.name": nsName,
			}

			Consistently(func() error {
				// Retrieve the NS object.
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            regularNS.Name,
						Labels:          regularNS.Labels,
						OwnerReferences: regularNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to take over the NS object")

			// Verify that the Deployment object has not been taken over.
			wantDeploy := deploy.DeepCopy()
			wantDeploy.TypeMeta = metav1.TypeMeta{}
			wantDeploy.Namespace = nsName
			wantDeploy.Name = deployName
			wantDeploy.Spec.Replicas = ptr.To(int32(2))

			Consistently(func() error {
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				if len(regularDeploy.Spec.Template.Spec.Containers) != 1 {
					return fmt.Errorf("number of containers in the Deployment object, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers), 1)
				}
				if len(regularDeploy.Spec.Template.Spec.Containers[0].Ports) != 1 {
					return fmt.Errorf("number of ports in the first container, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers[0].Ports), 1)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotDeploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       regularDeploy.Namespace,
						Name:            regularDeploy.Name,
						OwnerReferences: regularDeploy.OwnerReferences,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: regularDeploy.Spec.Replicas,
						Selector: regularDeploy.Spec.Selector,
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: regularDeploy.Spec.Template.ObjectMeta.Labels,
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  regularDeploy.Spec.Template.Spec.Containers[0].Name,
										Image: regularDeploy.Spec.Template.Spec.Containers[0].Image,
										Ports: []corev1.ContainerPort{
											{
												ContainerPort: regularDeploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort,
											},
										},
									},
								},
							},
						},
					},
				}

				if diff := cmp.Diff(rebuiltGotDeploy, wantDeploy); diff != "" {
					return fmt.Errorf("deployment diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval, "Failed to leave the Deployment object alone")
		})

		It("should update the Work object status", func() {
			noLaterThanTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFailedToTakeOver),
							ObservedGeneration: 0,
						},
					},
					DiffDetails: &fleetv1beta1.DiffDetails{
						ObservedInMemberClusterGeneration: &regularNS.Generation,
						ObservedDiffs: []fleetv1beta1.PatchDetail{
							{
								Path:          "/metadata/labels/foo",
								ValueInMember: dummyLabelValue1,
							},
							// TO-DO (chenyu1): This is a namespace specific field; consider
							// if this should be added as an exception which allows ignoring
							// this diff automatically.
							{
								Path:          "/spec/finalizers",
								ValueInMember: "[kubernetes]",
							},
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFailedToTakeOver),
							ObservedGeneration: 1,
						},
					},
					DiffDetails: &fleetv1beta1.DiffDetails{
						ObservedInMemberClusterGeneration: &regularDeploy.Generation,
						ObservedDiffs: []fleetv1beta1.PatchDetail{
							{Path: "/spec/progressDeadlineSeconds", ValueInMember: "600"},
							{
								Path:          "/spec/replicas",
								ValueInMember: "2",
								ValueInHub:    "1",
							},
							{Path: "/spec/revisionHistoryLimit", ValueInMember: "10"},
							{
								Path:          "/spec/strategy/rollingUpdate",
								ValueInMember: "map[maxSurge:25% maxUnavailable:25%]",
							},
							{Path: "/spec/strategy/type", ValueInMember: "RollingUpdate"},
							{
								Path:          "/spec/template/spec/containers/0/imagePullPolicy",
								ValueInMember: "Always",
							},
							{Path: "/spec/template/spec/containers/0/ports/0/protocol", ValueInMember: "TCP"},
							{
								Path:          "/spec/template/spec/containers/0/terminationMessagePath",
								ValueInMember: "/dev/termination-log",
							},
							{
								Path:          "/spec/template/spec/containers/0/terminationMessagePolicy",
								ValueInMember: "File",
							},
							{Path: "/spec/template/spec/dnsPolicy", ValueInMember: "ClusterFirst"},
							{Path: "/spec/template/spec/restartPolicy", ValueInMember: "Always"},
							{Path: "/spec/template/spec/schedulerName", ValueInMember: "default-scheduler"},
							{Path: "/spec/template/spec/securityContext", ValueInMember: "map[]"},
							{Path: "/spec/template/spec/terminationGracePeriodSeconds", ValueInMember: "30"},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &noLaterThanTimestamp, &noLaterThanTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// No object can be applied, hence no resource are bookkept in the AppliedWork object status.
			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, nil)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure that the Deployment object has been left alone.
			regularDeployNotRemovedActual := regularDeployNotRemovedActual(nsName, deployName)
			Consistently(regularDeployNotRemovedActual, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("detect drifts (apply if no drift, drift occurred, partial comparison)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				WhenToApply:      fleetv1beta1.WhenToApplyTypeIfNotDrifted,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the Deployment object has been applied as expected.
			regularDeploymentObjectAppliedActual := regularDeploymentObjectAppliedActual(nsName, deployName, appliedWorkOwnerRef)
			Eventually(regularDeploymentObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the deployment object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")
		})

		It("can mark the deployment as available", func() {
			markDeploymentAsAvailable(nsName, deployName)
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 1,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 1,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can make changes to the objects", func() {
			// Use Eventually blocks to avoid conflicts.
			Eventually(func() error {
				// Retrieve the Deployment object.
				updatedDeploy := &appsv1.Deployment{}
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, updatedDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				// Make changes to the Deployment object.
				updatedDeploy.Spec.Replicas = ptr.To(int32(2))

				// Update the Deployment object.
				if err := memberClient.Update(ctx, updatedDeploy); err != nil {
					return fmt.Errorf("failed to update the Deployment object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the Deployment object")

			Eventually(func() error {
				// Retrieve the NS object.
				updatedNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, updatedNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// Make changes to the NS object.
				if updatedNS.Labels == nil {
					updatedNS.Labels = map[string]string{}
				}
				updatedNS.Labels[dummyLabelKey] = dummyLabelValue1

				// Update the NS object.
				if err := memberClient.Update(ctx, updatedNS); err != nil {
					return fmt.Errorf("failed to update the NS object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the NS object")
		})

		It("should continue to apply some manifest (while preserving drifts in unmanaged fields)", func() {
			// Verify that the object are still being applied, with the drifts in unmanaged fields
			// untouched.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName
			wantNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
				// The label below is added by K8s itself (system-managed well-known label).
				"kubernetes.io/metadata.name": nsName,
			}
			wantNS.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}

			Consistently(func() error {
				// Retrieve the NS object.
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            regularNS.Name,
						Labels:          regularNS.Labels,
						OwnerReferences: regularNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to take over the NS object")
		})

		It("should stop applying some objects", func() {
			// Verify that the changes in managed fields are not overwritten.
			wantDeploy := deploy.DeepCopy()
			wantDeploy.TypeMeta = metav1.TypeMeta{}
			wantDeploy.Namespace = nsName
			wantDeploy.Name = deployName
			wantDeploy.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}
			wantDeploy.Spec.Replicas = ptr.To(int32(2))

			Consistently(func() error {
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				if len(regularDeploy.Spec.Template.Spec.Containers) != 1 {
					return fmt.Errorf("number of containers in the Deployment object, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers), 1)
				}
				if len(regularDeploy.Spec.Template.Spec.Containers[0].Ports) != 1 {
					return fmt.Errorf("number of ports in the first container, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers[0].Ports), 1)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotDeploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       regularDeploy.Namespace,
						Name:            regularDeploy.Name,
						OwnerReferences: regularDeploy.OwnerReferences,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: regularDeploy.Spec.Replicas,
						Selector: regularDeploy.Spec.Selector,
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: regularDeploy.Spec.Template.ObjectMeta.Labels,
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  regularDeploy.Spec.Template.Spec.Containers[0].Name,
										Image: regularDeploy.Spec.Template.Spec.Containers[0].Image,
										Ports: []corev1.ContainerPort{
											{
												ContainerPort: regularDeploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort,
											},
										},
									},
								},
							},
						},
					},
				}

				if diff := cmp.Diff(rebuiltGotDeploy, wantDeploy); diff != "" {
					return fmt.Errorf("deployment diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to leave the Deployment object alone")
		})

		It("should update the Work object status", func() {
			// Shift the timestamp to account for drift detection delays.
			noLaterThanTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFoundDrifts),
							ObservedGeneration: 2,
						},
					},
					DriftDetails: &fleetv1beta1.DriftDetails{
						ObservedInMemberClusterGeneration: regularDeploy.Generation,
						ObservedDrifts: []fleetv1beta1.PatchDetail{
							{
								Path:          "/spec/replicas",
								ValueInMember: "2",
								ValueInHub:    "1",
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &noLaterThanTimestamp, &noLaterThanTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure that the Deployment object has been left alone.
			regularDeployNotRemovedActual := regularDeployNotRemovedActual(nsName, deployName)
			Consistently(regularDeployNotRemovedActual, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	// For simplicity reasons, this test case will only involve a NS object.
	Context("detect drifts (apply if no drift, drift occurred, full comparison)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNS.Spec.Finalizers = []corev1.FinalizerName{"kubernetes"}
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypeFullComparison,
				WhenToApply:      fleetv1beta1.WhenToApplyTypeIfNotDrifted,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can make changes to the objects", func() {
			Eventually(func() error {
				// Retrieve the NS object.
				updatedNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, updatedNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// Make changes to the NS object.
				if updatedNS.Labels == nil {
					updatedNS.Labels = map[string]string{}
				}
				updatedNS.Labels[dummyLabelKey] = dummyLabelValue1

				// Update the NS object.
				if err := memberClient.Update(ctx, updatedNS); err != nil {
					return fmt.Errorf("failed to update the NS object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the NS object")
		})

		It("should stop applying some objects", func() {
			// Verify that the changes in unmanaged fields are not overwritten.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName
			wantNS.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}
			wantNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
				// The label below is added by K8s itself (system-managed well-known label).
				"kubernetes.io/metadata.name": nsName,
			}

			Consistently(func() error {
				// Retrieve the NS object.
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            regularNS.Name,
						Labels:          regularNS.Labels,
						OwnerReferences: regularNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to leave the NS object alone")
		})

		It("should update the Work object status", func() {
			// Shift the timestamp to account for drift detection delays.
			noLaterThanTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFoundDrifts),
							ObservedGeneration: 0,
						},
					},
					DriftDetails: &fleetv1beta1.DriftDetails{
						ObservedInMemberClusterGeneration: regularNS.Generation,
						ObservedDrifts: []fleetv1beta1.PatchDetail{
							{
								Path:          "/metadata/labels/foo",
								ValueInMember: dummyLabelValue1,
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &noLaterThanTimestamp, &noLaterThanTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// No object can be applied, hence no resource are bookkept in the AppliedWork object status.
			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, nil)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	// For simplicity reasons, this test case will only involve a NS object.
	Context("overwrite drifts (always apply, partial comparison)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
			}
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				WhenToApply:      fleetv1beta1.WhenToApplyTypeAlways,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can make changes to the objects", func() {
			Eventually(func() error {
				// Retrieve the NS object.
				updatedNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, updatedNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// Make changes to the NS object.
				if updatedNS.Labels == nil {
					updatedNS.Labels = map[string]string{}
				}
				updatedNS.Labels[dummyLabelKey] = dummyLabelValue2

				// Update the NS object.
				if err := memberClient.Update(ctx, updatedNS); err != nil {
					return fmt.Errorf("failed to update the NS object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the NS object")
		})

		It("should continue to apply some manifest (while overwriting drifts in managed fields)", func() {
			// Verify that the object are still being applied, with the drifts in managed fields
			// overwritten.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName
			wantNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
				// The label below is added by K8s itself (system-managed well-known label).
				"kubernetes.io/metadata.name": nsName,
			}
			wantNS.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}

			nsOverwrittenActual := func() error {
				// Retrieve the NS object.
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            regularNS.Name,
						Labels:          regularNS.Labels,
						OwnerReferences: regularNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}
			Eventually(nsOverwrittenActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the NS object")
			Consistently(nsOverwrittenActual, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to apply the NS object")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	// For simplicity reasons, this test case will only involve a NS object.
	Context("overwrite drifts (apply if no drift, drift occurred before manifest version bump, partial comparison)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
			}

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				WhenToApply:      fleetv1beta1.WhenToApplyTypeIfNotDrifted,
			}
			createWorkObject(workName, applyStrategy, marshalK8sObjJSON(regularNS))
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can make changes to the objects", func() {
			Eventually(func() error {
				// Retrieve the NS object.
				updatedNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, updatedNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// Make changes to the NS object.
				if updatedNS.Labels == nil {
					updatedNS.Labels = map[string]string{}
				}
				updatedNS.Labels[dummyLabelKey] = dummyLabelValue2

				// Update the NS object.
				if err := memberClient.Update(ctx, updatedNS); err != nil {
					return fmt.Errorf("failed to update the NS object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the NS object")
		})

		It("should stop applying some objects", func() {
			// Verify that the changes in unmanaged fields are not overwritten.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName
			wantNS.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}
			wantNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue2,
				// The label below is added by K8s itself (system-managed well-known label).
				"kubernetes.io/metadata.name": nsName,
			}

			Consistently(func() error {
				// Retrieve the NS object.
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            regularNS.Name,
						Labels:          regularNS.Labels,
						OwnerReferences: regularNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to leave the NS object alone")
		})

		It("should update the Work object status", func() {
			// Shift the timestamp to account for drift detection delays.
			noLaterThanTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFoundDrifts),
							ObservedGeneration: 0,
						},
					},
					DriftDetails: &fleetv1beta1.DriftDetails{
						ObservedInMemberClusterGeneration: regularNS.Generation,
						ObservedDrifts: []fleetv1beta1.PatchDetail{
							{
								Path:          "/metadata/labels/foo",
								ValueInMember: dummyLabelValue2,
								ValueInHub:    dummyLabelValue1,
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &noLaterThanTimestamp, &noLaterThanTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// No object can be applied, hence no resource are bookkept in the AppliedWork object status.
			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, nil)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can update the Work object", func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue3,
			}

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				WhenToApply:      fleetv1beta1.WhenToApplyTypeIfNotDrifted,
			}
			updateWorkObject(workName, applyStrategy, marshalK8sObjJSON(regularNS))
		})

		It("should apply the new manifests and overwrite all drifts in managed fields", func() {
			// Verify that the new manifests are applied.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName
			wantNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue3,
				// The label below is added by K8s itself (system-managed well-known label).
				"kubernetes.io/metadata.name": nsName,
			}
			wantNS.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}

			Eventually(func() error {
				// Retrieve the NS object.
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            regularNS.Name,
						Labels:          regularNS.Labels,
						OwnerReferences: regularNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply new manifests")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	// For simplicity reasons, this test case will only involve a NS object.
	Context("first drifted time preservation", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNS.Labels = map[string]string{
				dummyLabelKey: dummyLabelValue1,
			}

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				WhenToApply:      fleetv1beta1.WhenToApplyTypeIfNotDrifted,
			}
			createWorkObject(workName, applyStrategy, marshalK8sObjJSON(regularNS))
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can make changes to the objects", func() {
			Eventually(func() error {
				// Retrieve the NS object.
				updatedNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, updatedNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// Make changes to the NS object.
				if updatedNS.Labels == nil {
					updatedNS.Labels = map[string]string{}
				}
				updatedNS.Labels[dummyLabelKey] = dummyLabelValue2

				// Update the NS object.
				if err := memberClient.Update(ctx, updatedNS); err != nil {
					return fmt.Errorf("failed to update the NS object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the NS object")
		})

		var firstDriftedMustBeforeTimestamp metav1.Time

		It("should update the Work object status", func() {
			// Shift the timestamp to account for drift detection delays.
			noLaterThanTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFoundDrifts),
							ObservedGeneration: 0,
						},
					},
					DriftDetails: &fleetv1beta1.DriftDetails{
						ObservedInMemberClusterGeneration: regularNS.Generation,
						ObservedDrifts: []fleetv1beta1.PatchDetail{
							{
								Path:          "/metadata/labels/foo",
								ValueInHub:    dummyLabelValue1,
								ValueInMember: dummyLabelValue2,
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &noLaterThanTimestamp, &noLaterThanTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")

			// Track the timestamp that was just after the drift was first detected.
			firstDriftedMustBeforeTimestamp = metav1.Now()
		})

		It("can make changes to the objects, again", func() {
			Eventually(func() error {
				// Retrieve the NS object.
				updatedNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, updatedNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// Make changes to the NS object.
				if updatedNS.Labels == nil {
					updatedNS.Labels = map[string]string{}
				}
				updatedNS.Labels[dummyLabelKey] = dummyLabelValue4

				// Update the NS object.
				if err := memberClient.Update(ctx, updatedNS); err != nil {
					return fmt.Errorf("failed to update the NS object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the NS object")
		})

		It("should update the Work object status (must track timestamps correctly)", func() {
			// Shift the timestamp to account for drift detection delays.
			driftObservedMustBeforeTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFoundDrifts),
							ObservedGeneration: 0,
						},
					},
					DriftDetails: &fleetv1beta1.DriftDetails{
						ObservedInMemberClusterGeneration: regularNS.Generation,
						ObservedDrifts: []fleetv1beta1.PatchDetail{
							{
								Path:          "/metadata/labels/foo",
								ValueInMember: dummyLabelValue4,
								ValueInHub:    dummyLabelValue1,
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &driftObservedMustBeforeTimestamp, &firstDriftedMustBeforeTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("never take over", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName

			// Prepare the JSONs for the resources.
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Create the resources on the member cluster side.
			Expect(memberClient.Create(ctx, regularNS)).To(Succeed(), "Failed to create the NS object")

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				WhenToTakeOver: fleetv1beta1.WhenToTakeOverTypeNever,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, marshalK8sObjJSON(regularDeploy))
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests that haven not been created yet", func() {
			// Ensure that the Deployment object has been applied as expected.
			regularDeploymentObjectAppliedActual := regularDeploymentObjectAppliedActual(nsName, deployName, appliedWorkOwnerRef)
			Eventually(regularDeploymentObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the deployment object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")
		})

		It("should not apply the manifests that have corresponding resources", func() {
			Eventually(func() error {
				// Retrieve the NS object.
				updatedNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, updatedNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// Rebuild the NS object to ignore default values automatically.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            updatedNS.Name,
						OwnerReferences: updatedNS.OwnerReferences,
					},
				}

				wantNS := ns.DeepCopy()
				wantNS.Name = nsName
				if diff := cmp.Diff(rebuiltGotNS, wantNS, ignoreFieldTypeMetaInNamespace); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to leave the NS object alone")
		})

		It("can mark the deployment as available", func() {
			markDeploymentAsAvailable(nsName, deployName)
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeNotTakenOver),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 1,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 1,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure applied manifest has been removed.
			regularDeployRemovedActual := regularDeployRemovedActual(nsName, deployName)
			Eventually(regularDeployRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})
})

var _ = Describe("report diff", func() {
	// For simplicity reasons, this test case will only involve a NS object.
	Context("report diff only (new object)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var regularNS *corev1.Namespace

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				Type: fleetv1beta1.ApplyStrategyTypeReportDiff,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should not apply the manifests", func() {
			// Ensure that the NS object has not been applied.
			regularNSObjectNotAppliedActual := regularNSObjectNotAppliedActual(nsName)
			Eventually(regularNSObjectNotAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to avoid applying the namespace object")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeDiffReported,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsDiffReportedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeFoundDiff),
							ObservedGeneration: 0,
						},
					},
					DiffDetails: &fleetv1beta1.DiffDetails{
						ObservedDiffs: []fleetv1beta1.PatchDetail{
							{
								Path:       "/",
								ValueInHub: "(the whole object)",
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, nil)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("report diff only (with diff present, diff disappears later, partial comparison)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create the objects first in the member cluster.
			Expect(memberClient.Create(ctx, regularNS)).To(Succeed(), "Failed to create the NS object")

			// Create a diff in the replica count field.
			regularDeploy.Spec.Replicas = ptr.To(int32(2))
			Expect(memberClient.Create(ctx, regularDeploy)).To(Succeed(), "Failed to create the Deployment object")

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				Type:             fleetv1beta1.ApplyStrategyTypeReportDiff,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should own the objects, but not apply any manifests", func() {
			// Verify that the Deployment manifest has not been applied, yet Fleet has assumed
			// its ownership.
			wantDeploy := deploy.DeepCopy()
			wantDeploy.TypeMeta = metav1.TypeMeta{}
			wantDeploy.Namespace = nsName
			wantDeploy.Name = deployName
			wantDeploy.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}
			wantDeploy.Spec.Replicas = ptr.To(int32(2))

			deployOwnedButNotApplied := func() error {
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				if len(regularDeploy.Spec.Template.Spec.Containers) != 1 {
					return fmt.Errorf("number of containers in the Deployment object, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers), 1)
				}
				if len(regularDeploy.Spec.Template.Spec.Containers[0].Ports) != 1 {
					return fmt.Errorf("number of ports in the first container, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers[0].Ports), 1)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotDeploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       regularDeploy.Namespace,
						Name:            regularDeploy.Name,
						OwnerReferences: regularDeploy.OwnerReferences,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: regularDeploy.Spec.Replicas,
						Selector: regularDeploy.Spec.Selector,
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: regularDeploy.Spec.Template.ObjectMeta.Labels,
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  regularDeploy.Spec.Template.Spec.Containers[0].Name,
										Image: regularDeploy.Spec.Template.Spec.Containers[0].Image,
										Ports: []corev1.ContainerPort{
											{
												ContainerPort: regularDeploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort,
											},
										},
									},
								},
							},
						},
					},
				}

				if diff := cmp.Diff(rebuiltGotDeploy, wantDeploy); diff != "" {
					return fmt.Errorf("deployment diff (-got +want):\n%s", diff)
				}
				return nil
			}

			Eventually(deployOwnedButNotApplied, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to own the Deployment object without applying the manifest")
			Consistently(deployOwnedButNotApplied, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to own the Deployment object without applying the manifest")

			// Verify that Fleet has assumed ownership of the NS object.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName
			wantNS.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}

			nsOwnedButNotApplied := func() error {
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            regularNS.Name,
						OwnerReferences: regularNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}
			Eventually(nsOwnedButNotApplied, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to own the NS object without applying the manifest")
			Consistently(nsOwnedButNotApplied, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to own the NS object without applying the manifest")
		})

		It("should update the Work object status", func() {
			noLaterThanTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeDiffReported,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsDiffReportedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeNoDiffFound),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeFoundDiff),
							ObservedGeneration: 1,
						},
					},
					DiffDetails: &fleetv1beta1.DiffDetails{
						ObservedInMemberClusterGeneration: &regularDeploy.Generation,
						ObservedDiffs: []fleetv1beta1.PatchDetail{
							{
								Path:          "/spec/replicas",
								ValueInMember: "2",
								ValueInHub:    "1",
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &noLaterThanTimestamp, &noLaterThanTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should have no applied object reportings in the AppliedWork status", func() {
			// Prepare the status information.
			var appliedResourceMeta []fleetv1beta1.AppliedResourceMeta

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can make changes to the objects", func() {
			// Use Eventually blocks to avoid conflicts.
			Eventually(func() error {
				// Retrieve the Deployment object.
				updatedDeploy := &appsv1.Deployment{}
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, updatedDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				// Make changes to the Deployment object.
				updatedDeploy.Spec.Replicas = ptr.To(int32(1))

				// Update the Deployment object.
				if err := memberClient.Update(ctx, updatedDeploy); err != nil {
					return fmt.Errorf("failed to update the Deployment object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the Deployment object")
		})

		It("can mark the deployment as available", func() {
			markDeploymentAsAvailable(nsName, deployName)
		})

		It("should update the Work object status", func() {
			// Shift the timestamp to account for drift/diff detection delays.
			noLaterThanTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeDiffReported,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsDiffReportedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeNoDiffFound),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeNoDiffFound),
							ObservedGeneration: 2,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &noLaterThanTimestamp, &noLaterThanTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should have no applied object reportings in the AppliedWork status", func() {
			// Prepare the status information.
			var appliedResourceMeta []fleetv1beta1.AppliedResourceMeta

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure that the Deployment object has been left alone.
			regularDeployNotRemovedActual := regularDeployNotRemovedActual(nsName, deployName)
			Consistently(regularDeployNotRemovedActual, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("report diff only (w/ not taken over resources, partial comparison, a.k.a. do not touch anything and just report diff)", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create the objects first in the member cluster.
			Expect(memberClient.Create(ctx, regularNS)).To(Succeed(), "Failed to create the NS object")

			// Create a diff in the replica count field.
			regularDeploy.Spec.Replicas = ptr.To(int32(2))
			Expect(memberClient.Create(ctx, regularDeploy)).To(Succeed(), "Failed to create the Deployment object")

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				Type:             fleetv1beta1.ApplyStrategyTypeReportDiff,
				WhenToTakeOver:   fleetv1beta1.WhenToTakeOverTypeNever,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")
		})

		It("should not apply any manifest", func() {
			// Verify that the NS manifest has not been applied.
			Eventually(func() error {
				// Retrieve the NS object.
				updatedNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, updatedNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// Rebuild the NS object to ignore default values automatically.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            updatedNS.Name,
						OwnerReferences: updatedNS.OwnerReferences,
					},
				}
				wantNS := ns.DeepCopy()
				wantNS.Name = nsName
				if diff := cmp.Diff(rebuiltGotNS, wantNS, ignoreFieldTypeMetaInNamespace); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}

				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to leave the NS object alone")

			// Verify that the Deployment manifest has not been applied.
			Eventually(func() error {
				// Retrieve the Deployment object.
				updatedDeploy := &appsv1.Deployment{}
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, updatedDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				// Rebuild the Deployment object to ignore default values automatically.
				rebuiltGotDeploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       updatedDeploy.Namespace,
						Name:            updatedDeploy.Name,
						OwnerReferences: updatedDeploy.OwnerReferences,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: updatedDeploy.Spec.Replicas,
						Selector: updatedDeploy.Spec.Selector,
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: updatedDeploy.Spec.Template.ObjectMeta.Labels,
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  updatedDeploy.Spec.Template.Spec.Containers[0].Name,
										Image: updatedDeploy.Spec.Template.Spec.Containers[0].Image,
										Ports: []corev1.ContainerPort{
											{
												ContainerPort: updatedDeploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort,
											},
										},
									},
								},
							},
						},
					},
				}

				wantDeploy := deploy.DeepCopy()
				wantDeploy.TypeMeta = metav1.TypeMeta{}
				wantDeploy.Namespace = nsName
				wantDeploy.Name = deployName
				wantDeploy.Spec.Replicas = ptr.To(int32(2))

				if diff := cmp.Diff(rebuiltGotDeploy, wantDeploy); diff != "" {
					return fmt.Errorf("deployment diff (-got +want):\n%s", diff)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to leave the Deployment object alone")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeDiffReported,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsDiffReportedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeNoDiffFound),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeFoundDiff),
							ObservedGeneration: 1,
						},
					},
					DiffDetails: &fleetv1beta1.DiffDetails{
						ObservedDiffs: []fleetv1beta1.PatchDetail{
							{
								Path:          "/spec/replicas",
								ValueInHub:    "1",
								ValueInMember: "2",
							},
						},
						ObservedInMemberClusterGeneration: ptr.To(int64(1)),
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should have no applied object reportings in the AppliedWork status", func() {
			// Prepare the status information.
			var appliedResourceMeta []fleetv1beta1.AppliedResourceMeta

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure applied manifest has been removed.
			regularDeployRemovedActual := regularDeployRemovedActual(nsName, deployName)
			Eventually(regularDeployRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})
})

var _ = Describe("handling different apply strategies", func() {
	Context("switch from report diff to CSA", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create the objects first in the member cluster.
			Expect(memberClient.Create(ctx, regularNS)).To(Succeed(), "Failed to create the NS object")

			// Create a diff in the replica count field.
			regularDeploy.Spec.Replicas = ptr.To(int32(2))
			Expect(memberClient.Create(ctx, regularDeploy)).To(Succeed(), "Failed to create the Deployment object")

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				Type:             fleetv1beta1.ApplyStrategyTypeReportDiff,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should own the objects, but not apply any manifests", func() {
			// Verify that the Deployment manifest has not been applied, yet Fleet has assumed
			// its ownership.
			wantDeploy := deploy.DeepCopy()
			wantDeploy.TypeMeta = metav1.TypeMeta{}
			wantDeploy.Namespace = nsName
			wantDeploy.Name = deployName
			wantDeploy.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}
			wantDeploy.Spec.Replicas = ptr.To(int32(2))

			deployOwnedButNotApplied := func() error {
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				if len(regularDeploy.Spec.Template.Spec.Containers) != 1 {
					return fmt.Errorf("number of containers in the Deployment object, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers), 1)
				}
				if len(regularDeploy.Spec.Template.Spec.Containers[0].Ports) != 1 {
					return fmt.Errorf("number of ports in the first container, got %d, want %d", len(regularDeploy.Spec.Template.Spec.Containers[0].Ports), 1)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotDeploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       regularDeploy.Namespace,
						Name:            regularDeploy.Name,
						OwnerReferences: regularDeploy.OwnerReferences,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: regularDeploy.Spec.Replicas,
						Selector: regularDeploy.Spec.Selector,
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: regularDeploy.Spec.Template.ObjectMeta.Labels,
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  regularDeploy.Spec.Template.Spec.Containers[0].Name,
										Image: regularDeploy.Spec.Template.Spec.Containers[0].Image,
										Ports: []corev1.ContainerPort{
											{
												ContainerPort: regularDeploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort,
											},
										},
									},
								},
							},
						},
					},
				}

				if diff := cmp.Diff(rebuiltGotDeploy, wantDeploy); diff != "" {
					return fmt.Errorf("deployment diff (-got +want):\n%s", diff)
				}
				return nil
			}

			Eventually(deployOwnedButNotApplied, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to own the Deployment object without applying the manifest")
			Consistently(deployOwnedButNotApplied, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to own the Deployment object without applying the manifest")

			// Verify that Fleet has assumed ownership of the NS object.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName
			wantNS.OwnerReferences = []metav1.OwnerReference{
				*appliedWorkOwnerRef,
			}

			nsOwnedButNotApplied := func() error {
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            regularNS.Name,
						OwnerReferences: regularNS.OwnerReferences,
					},
				}

				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}
			Eventually(nsOwnedButNotApplied, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to own the NS object without applying the manifest")
			Consistently(nsOwnedButNotApplied, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to own the NS object without applying the manifest")
		})

		It("should update the Work object status", func() {
			noLaterThanTimestamp := metav1.Time{
				Time: time.Now().Add(time.Second * 30),
			}

			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeDiffReported,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsDiffReportedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeNoDiffFound),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeFoundDiff),
							ObservedGeneration: 1,
						},
					},
					DiffDetails: &fleetv1beta1.DiffDetails{
						ObservedInMemberClusterGeneration: &regularDeploy.Generation,
						ObservedDiffs: []fleetv1beta1.PatchDetail{
							{
								Path:          "/spec/replicas",
								ValueInMember: "2",
								ValueInHub:    "1",
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, &noLaterThanTimestamp, &noLaterThanTimestamp)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should have no applied object reportings in the AppliedWork status", func() {
			// Prepare the status information.
			var appliedResourceMeta []fleetv1beta1.AppliedResourceMeta

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("can update the apply strategy", func() {
			Eventually(func() error {
				work := &fleetv1beta1.Work{}
				if err := hubClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, work); err != nil {
					return fmt.Errorf("failed to retrieve the Work object: %w", err)
				}

				work.Spec.ApplyStrategy = &fleetv1beta1.ApplyStrategy{
					Type:             fleetv1beta1.ApplyStrategyTypeClientSideApply,
					ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
					WhenToApply:      fleetv1beta1.WhenToApplyTypeAlways,
					WhenToTakeOver:   fleetv1beta1.WhenToTakeOverTypeAlways,
				}
				if err := hubClient.Update(ctx, work); err != nil {
					return fmt.Errorf("failed to update the Work object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the apply strategy")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 2,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingAvailabilityResultTypeNotYetAvailable),
							ObservedGeneration: 2,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure applied manifest has been removed.
			regularDeployRemovedActual := regularDeployRemovedActual(nsName, deployName)
			Eventually(regularDeployRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("switch from SSA to report diff", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				Type:             fleetv1beta1.ApplyStrategyTypeServerSideApply,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the Deployment object has been applied as expected.
			regularDeploymentObjectAppliedActual := regularDeploymentObjectAppliedActual(nsName, deployName, appliedWorkOwnerRef)
			Eventually(regularDeploymentObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the deployment object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, regularDeploy)).To(Succeed(), "Failed to retrieve the Deployment object")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					UID: regularDeploy.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 1,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingAvailabilityResultTypeNotYetAvailable),
							ObservedGeneration: 1,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("can update the apply strategy", func() {
			Eventually(func() error {
				work := &fleetv1beta1.Work{}
				if err := hubClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, work); err != nil {
					return fmt.Errorf("failed to retrieve the Work object: %w", err)
				}

				work.Spec.ApplyStrategy = &fleetv1beta1.ApplyStrategy{
					ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
					Type:             fleetv1beta1.ApplyStrategyTypeReportDiff,
				}
				if err := hubClient.Update(ctx, work); err != nil {
					return fmt.Errorf("failed to update the Work object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the apply strategy")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeDiffReported,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsDiffReportedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeNoDiffFound),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeDiffReported,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingReportDiffResultTypeNoDiffFound),
							ObservedGeneration: 1,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should have no applied object reportings in the AppliedWork status", func() {
			// Prepare the status information.
			var appliedResourceMeta []fleetv1beta1.AppliedResourceMeta

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure applied manifest has been removed.
			regularDeployRemovedActual := regularDeployRemovedActual(nsName, deployName)
			Eventually(regularDeployRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("switch from never takeover to takeover if no diff", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var regularDeploy *appsv1.Deployment

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare a Deployment object.
			regularDeploy = deploy.DeepCopy()
			regularDeploy.Namespace = nsName
			regularDeploy.Name = deployName
			regularDeployJSON := marshalK8sObjJSON(regularDeploy)

			// Create objects in the member cluster.
			preExistingNS := regularNS.DeepCopy()
			Expect(memberClient.Create(ctx, preExistingNS)).To(Succeed(), "Failed to create the NS object")
			preExistingDeploy := regularDeploy.DeepCopy()
			preExistingDeploy.Spec.Replicas = ptr.To(int32(2))
			Expect(memberClient.Create(ctx, preExistingDeploy)).To(Succeed(), "Failed to create the Deployment object")

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			applyStrategy := &fleetv1beta1.ApplyStrategy{
				ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
				Type:             fleetv1beta1.ApplyStrategyTypeClientSideApply,
				WhenToTakeOver:   fleetv1beta1.WhenToTakeOverTypeNever,
			}
			createWorkObject(workName, applyStrategy, regularNSJSON, regularDeployJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should not take over some objects", func() {
			// Verify that the NS object has not been taken over.
			wantNS := ns.DeepCopy()
			wantNS.TypeMeta = metav1.TypeMeta{}
			wantNS.Name = nsName

			Consistently(func() error {
				preExistingNS := &corev1.Namespace{}
				if err := memberClient.Get(ctx, client.ObjectKey{Name: nsName}, preExistingNS); err != nil {
					return fmt.Errorf("failed to retrieve the NS object: %w", err)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotNS := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:            preExistingNS.Name,
						OwnerReferences: preExistingNS.OwnerReferences,
					},
				}
				if diff := cmp.Diff(rebuiltGotNS, wantNS); diff != "" {
					return fmt.Errorf("namespace diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to leave the NS object alone")

			// Verify that the Deployment object has not been taken over.
			wantDeploy := regularDeploy.DeepCopy()
			wantDeploy.TypeMeta = metav1.TypeMeta{}
			wantDeploy.Namespace = nsName
			wantDeploy.Name = deployName
			wantDeploy.Spec.Replicas = ptr.To(int32(2))

			Consistently(func() error {
				preExistingDeploy := &appsv1.Deployment{}
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, preExistingDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				if len(preExistingDeploy.Spec.Template.Spec.Containers) != 1 {
					return fmt.Errorf("number of containers in the Deployment object, got %d, want %d", len(preExistingDeploy.Spec.Template.Spec.Containers), 1)
				}
				if len(preExistingDeploy.Spec.Template.Spec.Containers[0].Ports) != 1 {
					return fmt.Errorf("number of ports in the first container, got %d, want %d", len(preExistingDeploy.Spec.Template.Spec.Containers[0].Ports), 1)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotDeploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       preExistingDeploy.Namespace,
						Name:            preExistingDeploy.Name,
						OwnerReferences: preExistingDeploy.OwnerReferences,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: preExistingDeploy.Spec.Replicas,
						Selector: preExistingDeploy.Spec.Selector,
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: preExistingDeploy.Spec.Template.ObjectMeta.Labels,
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  preExistingDeploy.Spec.Template.Spec.Containers[0].Name,
										Image: preExistingDeploy.Spec.Template.Spec.Containers[0].Image,
										Ports: []corev1.ContainerPort{
											{
												ContainerPort: preExistingDeploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort,
											},
										},
									},
								},
							},
						},
					},
				}

				if diff := cmp.Diff(rebuiltGotDeploy, wantDeploy); diff != "" {
					return fmt.Errorf("deployment diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to leave the Deployment object alone")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeNotTakenOver),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeNotTakenOver),
							ObservedGeneration: 1,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("can update the apply strategy", func() {
			Eventually(func() error {
				work := &fleetv1beta1.Work{}
				if err := hubClient.Get(ctx, client.ObjectKey{Name: workName, Namespace: memberReservedNSName}, work); err != nil {
					return fmt.Errorf("failed to retrieve the Work object: %w", err)
				}

				work.Spec.ApplyStrategy = &fleetv1beta1.ApplyStrategy{
					ComparisonOption: fleetv1beta1.ComparisonOptionTypePartialComparison,
					Type:             fleetv1beta1.ApplyStrategyTypeClientSideApply,
					WhenToTakeOver:   fleetv1beta1.WhenToTakeOverTypeIfNoDiff,
				}
				if err := hubClient.Update(ctx, work); err != nil {
					return fmt.Errorf("failed to update the Work object: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update the apply strategy")
		})

		It("should take over some objects", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")
		})

		It("should not take over some objects", func() {
			// Verify that the Deployment object has not been taken over.
			wantDeploy := deploy.DeepCopy()
			wantDeploy.TypeMeta = metav1.TypeMeta{}
			wantDeploy.Namespace = nsName
			wantDeploy.Name = deployName
			wantDeploy.Spec.Replicas = ptr.To(int32(2))

			Consistently(func() error {
				preExistingDeploy := &appsv1.Deployment{}
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: deployName}, preExistingDeploy); err != nil {
					return fmt.Errorf("failed to retrieve the Deployment object: %w", err)
				}

				if len(preExistingDeploy.Spec.Template.Spec.Containers) != 1 {
					return fmt.Errorf("number of containers in the Deployment object, got %d, want %d", len(preExistingDeploy.Spec.Template.Spec.Containers), 1)
				}
				if len(preExistingDeploy.Spec.Template.Spec.Containers[0].Ports) != 1 {
					return fmt.Errorf("number of ports in the first container, got %d, want %d", len(preExistingDeploy.Spec.Template.Spec.Containers[0].Ports), 1)
				}

				// To ignore default values automatically, here the test suite rebuilds the objects.
				rebuiltGotDeploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       preExistingDeploy.Namespace,
						Name:            preExistingDeploy.Name,
						OwnerReferences: preExistingDeploy.OwnerReferences,
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: preExistingDeploy.Spec.Replicas,
						Selector: preExistingDeploy.Spec.Selector,
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: preExistingDeploy.Spec.Template.ObjectMeta.Labels,
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  preExistingDeploy.Spec.Template.Spec.Containers[0].Name,
										Image: preExistingDeploy.Spec.Template.Spec.Containers[0].Image,
										Ports: []corev1.ContainerPort{
											{
												ContainerPort: preExistingDeploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort,
											},
										},
									},
								},
							},
						},
					},
				}

				if diff := cmp.Diff(rebuiltGotDeploy, wantDeploy); diff != "" {
					return fmt.Errorf("deployment diff (-got +want):\n%s", diff)
				}
				return nil
			}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to leave the Deployment object alone")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionFalse,
					Reason: condition.WorkNotAllManifestsAppliedReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "apps",
						Version:   "v1",
						Kind:      "Deployment",
						Resource:  "deployments",
						Name:      deployName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionFalse,
							Reason:             string(ManifestProcessingApplyResultTypeFailedToTakeOver),
							ObservedGeneration: 1,
						},
					},
					DiffDetails: &fleetv1beta1.DiffDetails{
						ObservedInMemberClusterGeneration: ptr.To(int64(1)),
						ObservedDiffs: []fleetv1beta1.PatchDetail{
							{
								Path:          "/spec/replicas",
								ValueInMember: "2",
								ValueInHub:    "1",
							},
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		It("should update the AppliedWork object status", func() {
			// Prepare the status information.
			appliedResourceMeta := []fleetv1beta1.AppliedResourceMeta{
				{
					WorkResourceIdentifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					UID: regularNS.UID,
				},
			}

			appliedWorkStatusUpdatedActual := appliedWorkStatusUpdated(workName, appliedResourceMeta)
			Eventually(appliedWorkStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update appliedWork status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure applied manifest has been removed.
			regularDeployRemovedActual := regularDeployRemovedActual(nsName, deployName)
			Eventually(regularDeployRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the deployment object")

			// Kubebuilder suggests that in a testing environment like this, to check for the existence of the AppliedWork object
			// OwnerReference in the Namespace object (https://book.kubebuilder.io/reference/envtest.html#testing-considerations).
			checkNSOwnerReferences(workName, nsName)

			// Ensure that the AppliedWork object has been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			workRemovedActual := workRemovedActual(workName)
			Eventually(workRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the Work object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})

	Context("falling back from CSA to SSA", Ordered, func() {
		workName := fmt.Sprintf(workNameTemplate, utils.RandStr())
		// The environment prepared by the envtest package does not support namespace
		// deletion; each test case would use a new namespace.
		nsName := fmt.Sprintf(nsNameTemplate, utils.RandStr())

		var appliedWorkOwnerRef *metav1.OwnerReference
		var regularNS *corev1.Namespace
		var oversizedCM *corev1.ConfigMap

		BeforeAll(func() {
			// Prepare a NS object.
			regularNS = ns.DeepCopy()
			regularNS.Name = nsName
			regularNSJSON := marshalK8sObjJSON(regularNS)

			// Prepare an oversized configMap object.

			// Generate a large bytes array.
			//
			// Kubernetes will reject configMaps larger than 1048576 bytes (~1 MB);
			// and when an object's spec size exceeds 262144 bytes, KubeFleet will not
			// be able to use client-side apply with the object as it cannot set
			// an last applied configuration annotation of that size. Consequently,
			// for this test case, it prepares a configMap object of 600000 bytes so
			// that Kubernetes will accept it but CSA cannot use it, forcing the
			// work applier to fall back to server-side apply.
			randomBytes := make([]byte, 600000)
			// Note that this method never returns an error and will always fill the given
			// slice completely.
			_, _ = rand.Read(randomBytes)
			// Encode the random bytes to a base64 string.
			randomBase64Str := base64.StdEncoding.EncodeToString(randomBytes)
			oversizedCM = &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					Kind:       "ConfigMap",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: nsName,
					Name:      configMapName,
				},
				Data: map[string]string{
					"randomBase64Str": randomBase64Str,
				},
			}
			oversizedCMJSON := marshalK8sObjJSON(oversizedCM)

			// Create a new Work object with all the manifest JSONs and proper apply strategy.
			createWorkObject(workName, nil, regularNSJSON, oversizedCMJSON)
		})

		It("should add cleanup finalizer to the Work object", func() {
			finalizerAddedActual := workFinalizerAddedActual(workName)
			Eventually(finalizerAddedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to add cleanup finalizer to the Work object")
		})

		It("should prepare an AppliedWork object", func() {
			appliedWorkCreatedActual := appliedWorkCreatedActual(workName)
			Eventually(appliedWorkCreatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to prepare an AppliedWork object")

			appliedWorkOwnerRef = prepareAppliedWorkOwnerRef(workName)
		})

		It("should apply the manifests", func() {
			// Ensure that the NS object has been applied as expected.
			regularNSObjectAppliedActual := regularNSObjectAppliedActual(nsName, appliedWorkOwnerRef)
			Eventually(regularNSObjectAppliedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Name: nsName}, regularNS)).To(Succeed(), "Failed to retrieve the NS object")

			// Ensure that the oversized ConfigMap object has been applied as expected via SSA.
			Eventually(func() error {
				gotConfigMap := &corev1.ConfigMap{}
				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: configMapName}, gotConfigMap); err != nil {
					return fmt.Errorf("failed to retrieve the ConfigMap object: %w", err)
				}

				wantConfigMap := oversizedCM.DeepCopy()
				wantConfigMap.TypeMeta = metav1.TypeMeta{}
				wantConfigMap.OwnerReferences = []metav1.OwnerReference{
					*appliedWorkOwnerRef,
				}

				rebuiltConfigMap := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:       gotConfigMap.Namespace,
						Name:            gotConfigMap.Name,
						OwnerReferences: gotConfigMap.OwnerReferences,
					},
					Data: gotConfigMap.Data,
				}
				if diff := cmp.Diff(rebuiltConfigMap, wantConfigMap); diff != "" {
					return fmt.Errorf("configMap diff (-got +want):\n%s", diff)
				}

				// Perform additional checks to ensure that the work applier has fallen back
				// from CSA to SSA.
				lastAppliedConf, foundAnnotation := gotConfigMap.Annotations[fleetv1beta1.LastAppliedConfigAnnotation]
				if foundAnnotation && len(lastAppliedConf) > 0 {
					return fmt.Errorf("the configMap object has annotation %s (value: %s) in presence when SSA should be used", fleetv1beta1.LastAppliedConfigAnnotation, lastAppliedConf)
				}

				foundFieldMgr := false
				fieldMgrs := gotConfigMap.GetManagedFields()
				for _, fieldMgr := range fieldMgrs {
					// For simplicity reasons, here the test case verifies only against the field
					// manager name.
					if fieldMgr.Manager == workFieldManagerName {
						foundFieldMgr = true
					}
				}
				if !foundFieldMgr {
					return fmt.Errorf("the configMap object does not list the KubeFleet member agent as a field manager (%s) when SSA should be used", workFieldManagerName)
				}

				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the oversized configMap object")

			Expect(memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: configMapName}, oversizedCM)).To(Succeed(), "Failed to retrieve the ConfigMap object")
		})

		It("should update the Work object status", func() {
			// Prepare the status information.
			workConds := []metav1.Condition{
				{
					Type:   fleetv1beta1.WorkConditionTypeApplied,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAppliedReason,
				},
				{
					Type:   fleetv1beta1.WorkConditionTypeAvailable,
					Status: metav1.ConditionTrue,
					Reason: condition.WorkAllManifestsAvailableReason,
				},
			}
			manifestConds := []fleetv1beta1.ManifestCondition{
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:  0,
						Group:    "",
						Version:  "v1",
						Kind:     "Namespace",
						Resource: "namespaces",
						Name:     nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
				{
					Identifier: fleetv1beta1.WorkResourceIdentifier{
						Ordinal:   1,
						Group:     "",
						Version:   "v1",
						Kind:      "ConfigMap",
						Resource:  "configmaps",
						Name:      configMapName,
						Namespace: nsName,
					},
					Conditions: []metav1.Condition{
						{
							Type:               fleetv1beta1.WorkConditionTypeApplied,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingApplyResultTypeApplied),
							ObservedGeneration: 0,
						},
						{
							Type:               fleetv1beta1.WorkConditionTypeAvailable,
							Status:             metav1.ConditionTrue,
							Reason:             string(ManifestProcessingAvailabilityResultTypeAvailable),
							ObservedGeneration: 0,
						},
					},
				},
			}

			workStatusUpdatedActual := workStatusUpdated(workName, workConds, manifestConds, nil, nil)
			Eventually(workStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update work status")
		})

		AfterAll(func() {
			// Delete the Work object and related resources.
			deleteWorkObject(workName)

			// Ensure that all applied manifests have been removed.
			appliedWorkRemovedActual := appliedWorkRemovedActual(workName, nsName)
			Eventually(appliedWorkRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the AppliedWork object")

			Eventually(func() error {
				// Retrieve the ConfigMap object.
				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: nsName,
						Name:      configMapName,
					},
				}
				if err := memberClient.Delete(ctx, cm); err != nil && !errors.IsNotFound(err) {
					return fmt.Errorf("failed to delete the ConfigMap object: %w", err)
				}

				if err := memberClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: configMapName}, cm); !errors.IsNotFound(err) {
					return fmt.Errorf("the ConfigMap object still exists or an unexpected error occurred: %w", err)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove the oversized configMap object")

			// The environment prepared by the envtest package does not support namespace
			// deletion; consequently this test suite would not attempt so verify its deletion.
		})
	})
})

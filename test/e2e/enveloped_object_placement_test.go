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

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/go-cmp/cmp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	placementv1beta1 "go.goms.io/fleet/apis/placement/v1beta1"
	"go.goms.io/fleet/pkg/controllers/workapplier"
	"go.goms.io/fleet/pkg/utils"
	"go.goms.io/fleet/pkg/utils/condition"
	"go.goms.io/fleet/test/e2e/framework"
)

var (
	// pre loaded test manifests
	testConfigMap, testEnvelopConfigMap corev1.ConfigMap
	testEnvelopeResourceQuota           corev1.ResourceQuota
)

const (
	wrapperCMName = "wrapper"

	cmDataKey = "foo"
	cmDataVal = "bar"
)

// Note that this container will run in parallel with other containers.
var _ = Describe("placing wrapped resources using a CRP", func() {
	Context("Test a CRP place enveloped objects successfully", Ordered, func() {
		crpName := fmt.Sprintf(crpNameTemplate, GinkgoParallelProcess())
		workNamespaceName := appNamespace().Name
		var wantSelectedResources []placementv1beta1.ResourceIdentifier

		BeforeAll(func() {
			// Create the test resources.
			readEnvelopTestManifests()
			wantSelectedResources = []placementv1beta1.ResourceIdentifier{
				{
					Kind:    "Namespace",
					Name:    workNamespaceName,
					Version: "v1",
				},
				{
					Kind:      "ConfigMap",
					Name:      testConfigMap.Name,
					Version:   "v1",
					Namespace: workNamespaceName,
				},
				{
					Kind:      "ConfigMap",
					Name:      testEnvelopConfigMap.Name,
					Version:   "v1",
					Namespace: workNamespaceName,
				},
			}
		})

		It("Create the test resources in the namespace", createWrappedResourcesForEnvelopTest)

		It("Create the CRP that select the name space", func() {
			crp := &placementv1beta1.ClusterResourcePlacement{
				ObjectMeta: metav1.ObjectMeta{
					Name: crpName,
					// Add a custom finalizer; this would allow us to better observe
					// the behavior of the controllers.
					Finalizers: []string{customDeletionBlockerFinalizer},
				},
				Spec: placementv1beta1.ClusterResourcePlacementSpec{
					ResourceSelectors: workResourceSelector(),
					Strategy: placementv1beta1.RolloutStrategy{
						Type: placementv1beta1.RollingUpdateRolloutStrategyType,
						RollingUpdate: &placementv1beta1.RollingUpdateConfig{
							UnavailablePeriodSeconds: ptr.To(2),
						},
					},
				},
			}
			Expect(hubClient.Create(ctx, crp)).To(Succeed(), "Failed to create CRP")
		})

		It("should update CRP status as expected", func() {
			// resourceQuota is enveloped so it's not trackable yet
			crpStatusUpdatedActual := customizedCRPStatusUpdatedActual(crpName, wantSelectedResources, allMemberClusterNames, nil, "0", true)
			Eventually(crpStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update CRP status as expected")
		})

		It("should place the resources on all member clusters", func() {
			for idx := range allMemberClusters {
				memberCluster := allMemberClusters[idx]
				workResourcesPlacedActual := checkEnvelopQuotaPlacement(memberCluster)
				Eventually(workResourcesPlacedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to place work resources on member cluster %s", memberCluster.ClusterName)
			}
		})

		It("Update the envelop configMap with bad configuration", func() {
			// modify the embedded namespaced resource to add a scope but it will be rejected as its immutable
			badEnvelopeResourceQuota := testEnvelopeResourceQuota.DeepCopy()
			badEnvelopeResourceQuota.Spec.Scopes = []corev1.ResourceQuotaScope{
				corev1.ResourceQuotaScopeNotBestEffort, corev1.ResourceQuotaScopeNotTerminating,
			}
			badResourceQuotaByte, err := json.Marshal(badEnvelopeResourceQuota)
			Expect(err).Should(Succeed())
			// Get the config map.
			Expect(hubClient.Get(ctx, types.NamespacedName{Namespace: workNamespaceName, Name: testEnvelopConfigMap.Name}, &testEnvelopConfigMap)).To(Succeed(), "Failed to get config map")
			testEnvelopConfigMap.Data["resourceQuota.yaml"] = string(badResourceQuotaByte)
			Expect(hubClient.Update(ctx, &testEnvelopConfigMap)).To(Succeed(), "Failed to update the enveloped config map")
		})

		It("should update CRP status with failed to apply resourceQuota", func() {
			// rolloutStarted is false, but other conditions are true.
			// "The rollout is being blocked by the rollout strategy in 2 cluster(s)",
			crpStatusUpdatedActual := checkForRolloutStuckOnOneFailedClusterStatus(wantSelectedResources)
			Eventually(crpStatusUpdatedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update CRP status as expected")
			Consistently(crpStatusUpdatedActual, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Failed to update CRP status as expected")
		})

		It("Update the envelop configMap back with good configuration", func() {
			// Get the config map.
			Expect(hubClient.Get(ctx, types.NamespacedName{Namespace: workNamespaceName, Name: testEnvelopConfigMap.Name}, &testEnvelopConfigMap)).To(Succeed(), "Failed to get config map")
			resourceQuotaByte, err := json.Marshal(testEnvelopeResourceQuota)
			Expect(err).Should(Succeed())
			testEnvelopConfigMap.Data["resourceQuota.yaml"] = string(resourceQuotaByte)
			Expect(hubClient.Update(ctx, &testEnvelopConfigMap)).To(Succeed(), "Failed to update the enveloped config map")
		})

		It("should update CRP status as success again", func() {
			crpStatusUpdatedActual := customizedCRPStatusUpdatedActual(crpName, wantSelectedResources, allMemberClusterNames, nil, "2", true)
			Eventually(crpStatusUpdatedActual, longEventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update CRP status as expected")
		})

		It("should place the resources on all member clusters again", func() {
			for idx := range allMemberClusters {
				memberCluster := allMemberClusters[idx]
				workResourcesPlacedActual := checkEnvelopQuotaPlacement(memberCluster)
				Eventually(workResourcesPlacedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to place work resources on member cluster %s", memberCluster.ClusterName)
			}
		})

		It("can delete the CRP", func() {
			crp := &placementv1beta1.ClusterResourcePlacement{
				ObjectMeta: metav1.ObjectMeta{
					Name: crpName,
				},
			}
			Expect(hubClient.Delete(ctx, crp)).To(Succeed(), "Failed to delete CRP")
		})

		It("should remove placed resources from all member clusters", checkIfRemovedWorkResourcesFromAllMemberClusters)

		It("should remove controller finalizers from CRP", func() {
			finalizerRemovedActual := allFinalizersExceptForCustomDeletionBlockerRemovedFromCRPActual(crpName)
			Eventually(finalizerRemovedActual, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to remove controller finalizers from CRP")
		})

		AfterAll(func() {
			By(fmt.Sprintf("deleting placement %s and related resources", crpName))
			ensureCRPAndRelatedResourcesDeleted(crpName, allMemberClusters)
		})
	})

	Context("Test a CRP place workload objects with mixed availability", Ordered, func() {
		crpName := fmt.Sprintf(crpNameTemplate, GinkgoParallelProcess())
		workNamespace := appNamespace()
		var wantSelectedResources []placementv1beta1.ResourceIdentifier
		var testDeployment appv1.Deployment
		var testDaemonSet appv1.DaemonSet
		var testStatefulSet appv1.StatefulSet
		var testEnvelopeConfig corev1.ConfigMap

		BeforeAll(func() {
			// read the test resources.
			readDeploymentTestManifest(&testDeployment)
			readDaemonSetTestManifest(&testDaemonSet)
			readStatefulSetTestManifest(&testStatefulSet, true)
			readEnvelopeConfigMapTestManifest(&testEnvelopeConfig)
			wantSelectedResources = []placementv1beta1.ResourceIdentifier{
				{
					Kind:    utils.NamespaceKind,
					Name:    workNamespace.Name,
					Version: corev1.SchemeGroupVersion.Version,
				},
				{
					Kind:      utils.ConfigMapKind,
					Name:      testEnvelopeConfig.Name,
					Version:   corev1.SchemeGroupVersion.Version,
					Namespace: workNamespace.Name,
				},
			}
		})

		It("Create the namespace", func() {
			Expect(hubClient.Create(ctx, &workNamespace)).To(Succeed(), "Failed to create namespace %s", workNamespace.Name)
		})

		It("Create the wrapped resources in the namespace", func() {
			testEnvelopeConfig.Data = make(map[string]string)
			constructWrappedResources(&testEnvelopeConfig, &testDeployment, utils.DeploymentKind, workNamespace)
			constructWrappedResources(&testEnvelopeConfig, &testDaemonSet, utils.DaemonSetKind, workNamespace)
			constructWrappedResources(&testEnvelopeConfig, &testStatefulSet, utils.StatefulSetKind, workNamespace)
			Expect(hubClient.Create(ctx, &testEnvelopeConfig)).To(Succeed(), "Failed to create testEnvelop object %s containing workloads", testEnvelopeConfig.Name)
		})

		It("Create the CRP that select the namespace", func() {
			crp := &placementv1beta1.ClusterResourcePlacement{
				ObjectMeta: metav1.ObjectMeta{
					Name: crpName,
					// Add a custom finalizer; this would allow us to better observe the behavior of the controllers.
					Finalizers: []string{customDeletionBlockerFinalizer},
				},
				Spec: placementv1beta1.ClusterResourcePlacementSpec{
					ResourceSelectors: workResourceSelector(),
					Strategy: placementv1beta1.RolloutStrategy{
						Type: placementv1beta1.RollingUpdateRolloutStrategyType,
						RollingUpdate: &placementv1beta1.RollingUpdateConfig{
							UnavailablePeriodSeconds: ptr.To(2),
						},
					},
				},
			}
			Expect(hubClient.Create(ctx, crp)).To(Succeed(), "Failed to create CRP")
		})

		It("should update CRP status with only a not available statefulset", func() {
			// the statefulset has an invalid storage class PVC
			failedStatefulSetResourceIdentifier := placementv1beta1.ResourceIdentifier{
				Group:     appv1.SchemeGroupVersion.Group,
				Version:   appv1.SchemeGroupVersion.Version,
				Kind:      utils.StatefulSetKind,
				Name:      testStatefulSet.Name,
				Namespace: testStatefulSet.Namespace,
				Envelope: &placementv1beta1.EnvelopeIdentifier{
					Name:      testEnvelopeConfig.Name,
					Namespace: workNamespace.Name,
					Type:      placementv1beta1.ConfigMapEnvelopeType,
				},
			}
			// We only expect the statefulset to not be available all the clusters
			PlacementStatuses := make([]placementv1beta1.ResourcePlacementStatus, 0)
			for _, memberClusterName := range allMemberClusterNames {
				unavailableResourcePlacementStatus := placementv1beta1.ResourcePlacementStatus{
					ClusterName: memberClusterName,
					Conditions: []metav1.Condition{
						{
							Type:               string(placementv1beta1.ResourceScheduledConditionType),
							Status:             metav1.ConditionTrue,
							Reason:             condition.ScheduleSucceededReason,
							ObservedGeneration: 1,
						},
						{
							Type:               string(placementv1beta1.ResourceRolloutStartedConditionType),
							Status:             metav1.ConditionTrue,
							Reason:             condition.RolloutStartedReason,
							ObservedGeneration: 1,
						},
						{
							Type:               string(placementv1beta1.ResourceOverriddenConditionType),
							Status:             metav1.ConditionTrue,
							Reason:             condition.OverrideNotSpecifiedReason,
							ObservedGeneration: 1,
						},
						{
							Type:               string(placementv1beta1.ResourceWorkSynchronizedConditionType),
							Status:             metav1.ConditionTrue,
							Reason:             condition.AllWorkSyncedReason,
							ObservedGeneration: 1,
						},
						{
							Type:               string(placementv1beta1.ResourcesAppliedConditionType),
							Status:             metav1.ConditionTrue,
							Reason:             condition.AllWorkAppliedReason,
							ObservedGeneration: 1,
						},
						{
							Type:               string(placementv1beta1.ResourcesAvailableConditionType),
							Status:             metav1.ConditionFalse,
							Reason:             condition.WorkNotAvailableReason,
							ObservedGeneration: 1,
						},
					},
					FailedPlacements: []placementv1beta1.FailedResourcePlacement{
						{
							ResourceIdentifier: failedStatefulSetResourceIdentifier,
							Condition: metav1.Condition{
								Type:               string(placementv1beta1.ResourcesAvailableConditionType),
								Status:             metav1.ConditionFalse,
								Reason:             string(workapplier.ManifestProcessingAvailabilityResultTypeNotYetAvailable),
								ObservedGeneration: 1,
							},
						},
					},
				}
				PlacementStatuses = append(PlacementStatuses, unavailableResourcePlacementStatus)
			}
			wantStatus := placementv1beta1.ClusterResourcePlacementStatus{
				Conditions:            crpNotAvailableConditions(1, false),
				PlacementStatuses:     PlacementStatuses,
				SelectedResources:     wantSelectedResources,
				ObservedResourceIndex: "0",
			}

			Eventually(func() error {
				crp := &placementv1beta1.ClusterResourcePlacement{}
				if err := hubClient.Get(ctx, types.NamespacedName{Name: crpName}, crp); err != nil {
					return err
				}

				if diff := cmp.Diff(crp.Status, wantStatus, crpStatusCmpOptions...); diff != "" {
					return fmt.Errorf("CRP status diff (-got, +want): %s", diff)
				}
				return nil
			}, longEventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update CRP status as expected")
		})

		AfterAll(func() {
			By(fmt.Sprintf("deleting placement %s and related resources", crpName))
			ensureCRPAndRelatedResourcesDeleted(crpName, allMemberClusters)
		})
	})

	Context("Block envelopes that wrap cluster-scoped resources", Ordered, func() {
		crpName := fmt.Sprintf(crpNameTemplate, GinkgoParallelProcess())
		workNamespaceName := fmt.Sprintf(workNamespaceNameTemplate, GinkgoParallelProcess())

		wrappedCMName := "app"
		wrappedCBName := "standard"

		BeforeAll(func() {
			// Use an envelope to create duplicate resource entries.
			ns := appNamespace()
			Expect(hubClient.Create(ctx, &ns)).To(Succeed(), "Failed to create namespace %s", ns.Name)

			// Create an envelope config map.
			wrapperCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wrapperCMName,
					Namespace: ns.Name,
					Annotations: map[string]string{
						placementv1beta1.EnvelopeConfigMapAnnotation: "true",
					},
				},
				Data: map[string]string{},
			}

			// Create a configMap and a clusterRole as wrapped resources.
			wrappedCM := &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns.Name,
					Name:      wrappedCMName,
				},
				Data: map[string]string{
					cmDataKey: cmDataVal,
				},
			}
			wrappedCMBytes, err := json.Marshal(wrappedCM)
			Expect(err).To(BeNil(), "Failed to marshal configMap %s", wrappedCM.Name)
			wrapperCM.Data["cm.yaml"] = string(wrappedCMBytes)

			wrappedCB := &rbacv1.ClusterRole{
				TypeMeta: metav1.TypeMeta{
					APIVersion: rbacv1.SchemeGroupVersion.String(),
					Kind:       "ClusterRole",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: wrappedCBName,
				},
				Rules: []rbacv1.PolicyRule{
					{
						APIGroups: []string{""},
						Resources: []string{"pods"},
						Verbs:     []string{"get", "list", "watch"},
					},
				},
			}
			wrappedCBBytes, err := json.Marshal(wrappedCB)
			Expect(err).To(BeNil(), "Failed to marshal clusterRole %s", wrappedCB.Name)
			wrapperCM.Data["cb.yaml"] = string(wrappedCBBytes)

			Expect(hubClient.Create(ctx, wrapperCM)).To(Succeed(), "Failed to create configMap %s", wrapperCM.Name)

			// Create a CRP.
			crp := &placementv1beta1.ClusterResourcePlacement{
				ObjectMeta: metav1.ObjectMeta{
					Name: crpName,
					// Add a custom finalizer; this would allow us to better observe
					// the behavior of the controllers.
					Finalizers: []string{customDeletionBlockerFinalizer},
				},
				Spec: placementv1beta1.ClusterResourcePlacementSpec{
					ResourceSelectors: workResourceSelector(),
					Policy: &placementv1beta1.PlacementPolicy{
						PlacementType: placementv1beta1.PickFixedPlacementType,
						ClusterNames: []string{
							memberCluster1EastProdName,
						},
					},
					Strategy: placementv1beta1.RolloutStrategy{
						Type: placementv1beta1.RollingUpdateRolloutStrategyType,
						RollingUpdate: &placementv1beta1.RollingUpdateConfig{
							UnavailablePeriodSeconds: ptr.To(2),
						},
					},
				},
			}
			Expect(hubClient.Create(ctx, crp)).To(Succeed(), "Failed to create CRP")
		})

		It("should update CRP status as expected", func() {
			Eventually(func() error {
				crp := &placementv1beta1.ClusterResourcePlacement{}
				if err := hubClient.Get(ctx, types.NamespacedName{Name: crpName}, crp); err != nil {
					return err
				}

				wantStatus := placementv1beta1.ClusterResourcePlacementStatus{
					Conditions: crpWorkSynchronizedFailedConditions(crp.Generation, false),
					PlacementStatuses: []placementv1beta1.ResourcePlacementStatus{
						{
							ClusterName: memberCluster1EastProdName,
							Conditions:  resourcePlacementWorkSynchronizedFailedConditions(crp.Generation, false),
						},
					},
					SelectedResources: []placementv1beta1.ResourceIdentifier{
						{
							Kind:    "Namespace",
							Name:    workNamespaceName,
							Version: "v1",
						},
						{
							Kind:      "ConfigMap",
							Name:      wrapperCMName,
							Version:   "v1",
							Namespace: workNamespaceName,
						},
					},
					ObservedResourceIndex: "0",
				}
				if diff := cmp.Diff(crp.Status, wantStatus, crpStatusCmpOptions...); diff != "" {
					return fmt.Errorf("CRP status diff (-got, +want): %s", diff)
				}
				return nil
			}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to update CRP status as expected")
		})

		// Note that due to the order in which the work generator handles resources, the synchronization error is
		// triggered before the primary work object is applied; that is, the namespace itself will not be created
		// either.

		AfterAll(func() {
			// Remove the CRP and the namespace from the hub cluster.
			ensureCRPAndRelatedResourcesDeleted(crpName, []*framework.Cluster{memberCluster1EastProd})
		})
	})
})

var _ = Describe("Process objects with generate name", Ordered, func() {
	crpName := fmt.Sprintf(crpNameTemplate, GinkgoParallelProcess())
	workNamespaceName := fmt.Sprintf(workNamespaceNameTemplate, GinkgoParallelProcess())

	nsGenerateName := "application-"
	wrappedCMGenerateName := "wrapped-foo-"

	BeforeAll(func() {
		// Create the namespace with both name and generate name set.
		ns := appNamespace()
		ns.GenerateName = nsGenerateName
		Expect(hubClient.Create(ctx, &ns)).To(Succeed(), "Failed to create namespace %s", ns.Name)

		// Create an envelope config map.
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      wrapperCMName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					placementv1beta1.EnvelopeConfigMapAnnotation: "true",
				},
			},
			Data: map[string]string{},
		}

		wrappedCM := &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				APIVersion: corev1.SchemeGroupVersion.String(),
				Kind:       "ConfigMap",
			},
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: wrappedCMGenerateName,
				Namespace:    ns.Name,
			},
			Data: map[string]string{
				cmDataKey: cmDataVal,
			},
		}
		wrappedCMByte, err := json.Marshal(wrappedCM)
		Expect(err).Should(BeNil())
		cm.Data["wrapped.yaml"] = string(wrappedCMByte)
		Expect(hubClient.Create(ctx, cm)).To(Succeed(), "Failed to create config map %s", cm.Name)

		// Create a CRP that selects the namespace.
		crp := &placementv1beta1.ClusterResourcePlacement{
			ObjectMeta: metav1.ObjectMeta{
				Name: crpName,
				// Add a custom finalizer; this would allow us to better observe
				// the behavior of the controllers.
				Finalizers: []string{customDeletionBlockerFinalizer},
			},
			Spec: placementv1beta1.ClusterResourcePlacementSpec{
				ResourceSelectors: workResourceSelector(),
				Policy: &placementv1beta1.PlacementPolicy{
					PlacementType: placementv1beta1.PickFixedPlacementType,
					ClusterNames: []string{
						memberCluster1EastProdName,
					},
				},
				Strategy: placementv1beta1.RolloutStrategy{
					Type: placementv1beta1.RollingUpdateRolloutStrategyType,
					RollingUpdate: &placementv1beta1.RollingUpdateConfig{
						UnavailablePeriodSeconds: ptr.To(2),
					},
				},
			},
		}
		Expect(hubClient.Create(ctx, crp)).To(Succeed(), "Failed to create CRP")
	})

	It("should update CRP status as expected", func() {
		Eventually(func() error {
			crp := &placementv1beta1.ClusterResourcePlacement{}
			if err := hubClient.Get(ctx, types.NamespacedName{Name: crpName}, crp); err != nil {
				return err
			}

			wantStatus := placementv1beta1.ClusterResourcePlacementStatus{
				Conditions: crpAppliedFailedConditions(crp.Generation),
				PlacementStatuses: []placementv1beta1.ResourcePlacementStatus{
					{
						ClusterName: memberCluster1EastProdName,
						FailedPlacements: []placementv1beta1.FailedResourcePlacement{
							{
								ResourceIdentifier: placementv1beta1.ResourceIdentifier{
									Kind:      "ConfigMap",
									Namespace: workNamespaceName,
									Version:   "v1",
									Envelope: &placementv1beta1.EnvelopeIdentifier{
										Name:      wrapperCMName,
										Namespace: workNamespaceName,
										Type:      placementv1beta1.ConfigMapEnvelopeType,
									},
								},
								Condition: metav1.Condition{
									Type:               placementv1beta1.WorkConditionTypeApplied,
									Status:             metav1.ConditionFalse,
									Reason:             string(workapplier.ManifestProcessingApplyResultTypeFoundGenerateName),
									ObservedGeneration: 0,
								},
							},
						},
						Conditions: resourcePlacementApplyFailedConditions(crp.Generation),
					},
				},
				SelectedResources: []placementv1beta1.ResourceIdentifier{
					{
						Kind:    "Namespace",
						Name:    workNamespaceName,
						Version: "v1",
					},
					{
						Kind:      "ConfigMap",
						Name:      wrapperCMName,
						Version:   "v1",
						Namespace: workNamespaceName,
					},
				},
				ObservedResourceIndex: "0",
			}
			if diff := cmp.Diff(crp.Status, wantStatus, crpStatusCmpOptions...); diff != "" {
				return fmt.Errorf("CRP status diff (-got, +want): %s", diff)
			}
			return nil
		}, eventuallyDuration*3, eventuallyInterval).Should(Succeed(), "Failed to update CRP status as expected")
	})

	It("should place some manifests on member clusters", func() {
		Eventually(func() error {
			return validateWorkNamespaceOnCluster(memberCluster1EastProd, types.NamespacedName{Name: workNamespaceName})
		}, eventuallyDuration, eventuallyInterval).Should(Succeed(), "Failed to apply the namespace object")
	})

	It("should not place some manifests on member clusters", func() {
		Consistently(func() error {
			cmList := &corev1.ConfigMapList{}
			if err := memberCluster1EastProdClient.List(ctx, cmList, client.InNamespace(workNamespaceName)); err != nil {
				return fmt.Errorf("failed to list ConfigMap objects: %w", err)
			}

			for _, cm := range cmList.Items {
				if cm.GenerateName == wrappedCMGenerateName {
					return fmt.Errorf("found a ConfigMap object with generate name that should not be applied")
				}
			}
			return nil
		}, consistentlyDuration, consistentlyInterval).Should(Succeed(), "Applied the wrapped config map on member cluster")
	})

	AfterAll(func() {
		By(fmt.Sprintf("deleting placement %s and related resources", crpName))
		ensureCRPAndRelatedResourcesDeleted(crpName, allMemberClusters)
	})
})

func checkEnvelopQuotaPlacement(memberCluster *framework.Cluster) func() error {
	workNamespaceName := appNamespace().Name
	return func() error {
		if err := validateWorkNamespaceOnCluster(memberCluster, types.NamespacedName{Name: workNamespaceName}); err != nil {
			return err
		}
		By("check the placedConfigMap")
		placedConfigMap := &corev1.ConfigMap{}
		if err := memberCluster.KubeClient.Get(ctx, types.NamespacedName{Namespace: workNamespaceName, Name: testConfigMap.Name}, placedConfigMap); err != nil {
			return err
		}
		hubConfigMap := &corev1.ConfigMap{}
		if err := hubCluster.KubeClient.Get(ctx, types.NamespacedName{Namespace: workNamespaceName, Name: testConfigMap.Name}, hubConfigMap); err != nil {
			return err
		}
		if diff := cmp.Diff(placedConfigMap.Data, hubConfigMap.Data); diff != "" {
			return fmt.Errorf("configmap diff (-got, +want): %s", diff)
		}
		By("check the namespaced envelope objects")
		placedResourceQuota := &corev1.ResourceQuota{}
		if err := memberCluster.KubeClient.Get(ctx, types.NamespacedName{Namespace: workNamespaceName, Name: testEnvelopeResourceQuota.Name}, placedResourceQuota); err != nil {
			return err
		}
		if diff := cmp.Diff(placedResourceQuota.Spec, testEnvelopeResourceQuota.Spec); diff != "" {
			return fmt.Errorf("resource quota diff (-got, +want): %s", diff)
		}
		return nil
	}
}

func checkForRolloutStuckOnOneFailedClusterStatus(wantSelectedResources []placementv1beta1.ResourceIdentifier) func() error {
	crpName := fmt.Sprintf(crpNameTemplate, GinkgoParallelProcess())
	workNamespaceName := fmt.Sprintf(workNamespaceNameTemplate, GinkgoParallelProcess())
	wantFailedResourcePlacement := []placementv1beta1.FailedResourcePlacement{
		{
			ResourceIdentifier: placementv1beta1.ResourceIdentifier{
				Kind:      "ResourceQuota",
				Name:      testEnvelopeResourceQuota.Name,
				Version:   "v1",
				Namespace: testEnvelopeResourceQuota.Namespace,
				Envelope: &placementv1beta1.EnvelopeIdentifier{
					Name:      testEnvelopConfigMap.Name,
					Namespace: workNamespaceName,
					Type:      placementv1beta1.ConfigMapEnvelopeType,
				},
			},
			Condition: metav1.Condition{
				Type:   placementv1beta1.WorkConditionTypeApplied,
				Status: metav1.ConditionFalse,
				Reason: string(workapplier.ManifestProcessingApplyResultTypeFailedToApply),
			},
		},
	}

	return func() error {
		crp := &placementv1beta1.ClusterResourcePlacement{}
		if err := hubClient.Get(ctx, types.NamespacedName{Name: crpName}, crp); err != nil {
			return err
		}
		wantCRPConditions := crpRolloutStuckConditions(crp.Generation)
		if diff := cmp.Diff(crp.Status.Conditions, wantCRPConditions, crpStatusCmpOptions...); diff != "" {
			return fmt.Errorf("CRP status diff (-got, +want): %s", diff)
		}
		// check the selected resources is still right
		if diff := cmp.Diff(crp.Status.SelectedResources, wantSelectedResources, crpStatusCmpOptions...); diff != "" {
			return fmt.Errorf("CRP status diff (-got, +want): %s", diff)
		}
		// check the placement status has a failed placement
		applyFailed := false
		for _, placementStatus := range crp.Status.PlacementStatuses {
			if len(placementStatus.FailedPlacements) != 0 {
				applyFailed = true
			}
		}
		if !applyFailed {
			return fmt.Errorf("CRP status does not have failed placement")
		}
		for _, placementStatus := range crp.Status.PlacementStatuses {
			// this is the cluster that got the new enveloped resource that was malformed
			if len(placementStatus.FailedPlacements) != 0 {
				if diff := cmp.Diff(placementStatus.FailedPlacements, wantFailedResourcePlacement, crpStatusCmpOptions...); diff != "" {
					return fmt.Errorf("CRP status diff (-got, +want): %s", diff)
				}
				// check that the applied error message is correct
				if !strings.Contains(placementStatus.FailedPlacements[0].Condition.Message, "field is immutable") {
					return fmt.Errorf("CRP failed resource placement does not have unsupported scope message")
				}
				if diff := cmp.Diff(placementStatus.Conditions, resourcePlacementApplyFailedConditions(crp.Generation), crpStatusCmpOptions...); diff != "" {
					return fmt.Errorf("CRP status diff (-got, +want): %s", diff)
				}
			} else {
				// the cluster is stuck behind a rollout schedule since we now have 1 cluster that is not in applied ready status
				if diff := cmp.Diff(placementStatus.Conditions, resourcePlacementSyncPendingConditions(crp.Generation), crpStatusCmpOptions...); diff != "" {
					return fmt.Errorf("CRP status diff (-got, +want): %s", diff)
				}
			}
		}
		return nil
	}
}

func readEnvelopTestManifests() {
	By("Read the testConfigMap resources")
	testConfigMap = corev1.ConfigMap{}
	err := utils.GetObjectFromManifest("resources/test-configmap.yaml", &testConfigMap)
	Expect(err).Should(Succeed())

	By("Read testEnvelopConfigMap resource")
	testEnvelopConfigMap = corev1.ConfigMap{}
	err = utils.GetObjectFromManifest("resources/test-envelop-configmap.yaml", &testEnvelopConfigMap)
	Expect(err).Should(Succeed())

	By("Read ResourceQuota")
	testEnvelopeResourceQuota = corev1.ResourceQuota{}
	err = utils.GetObjectFromManifest("resources/resourcequota.yaml", &testEnvelopeResourceQuota)
	Expect(err).Should(Succeed())
}

// createWrappedResourcesForEnvelopTest creates some enveloped resources on the hub cluster for testing purposes.
func createWrappedResourcesForEnvelopTest() {
	ns := appNamespace()
	Expect(hubClient.Create(ctx, &ns)).To(Succeed(), "Failed to create namespace %s", ns.Name)
	// modify the configMap according to the namespace
	testConfigMap.Namespace = ns.Name
	Expect(hubClient.Create(ctx, &testConfigMap)).To(Succeed(), "Failed to create config map %s", testConfigMap.Name)

	// modify the enveloped configMap according to the namespace
	testEnvelopConfigMap.Namespace = ns.Name

	// modify the embedded namespaced resource according to the namespace
	testEnvelopeResourceQuota.Namespace = ns.Name
	resourceQuotaByte, err := json.Marshal(testEnvelopeResourceQuota)
	Expect(err).Should(Succeed())
	testEnvelopConfigMap.Data["resourceQuota.yaml"] = string(resourceQuotaByte)
	Expect(hubClient.Create(ctx, &testEnvelopConfigMap)).To(Succeed(), "Failed to create testEnvelop config map %s", testEnvelopConfigMap.Name)
}

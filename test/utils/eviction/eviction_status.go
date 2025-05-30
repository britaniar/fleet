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

package eviction

import (
	"context"
	"fmt"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	placementv1beta1 "go.goms.io/fleet/apis/placement/v1beta1"
	"go.goms.io/fleet/pkg/utils/condition"
)

var (
	lessFuncCondition = func(a, b metav1.Condition) bool {
		return a.Type < b.Type
	}
	evictionStatusCmpOptions = cmp.Options{
		cmpopts.SortSlices(lessFuncCondition),
		cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime"),
		cmpopts.EquateEmpty(),
	}
)

func StatusUpdatedActual(ctx context.Context, client client.Client, evictionName string, isValidEviction *IsValidEviction, isExecutedEviction *IsExecutedEviction) func() error {
	return func() error {
		var eviction placementv1beta1.ClusterResourcePlacementEviction
		if err := client.Get(ctx, types.NamespacedName{Name: evictionName}, &eviction); err != nil {
			return err
		}
		var conditions []metav1.Condition
		if isValidEviction != nil {
			if isValidEviction.IsValid {
				validCondition := metav1.Condition{
					Type:               string(placementv1beta1.PlacementEvictionConditionTypeValid),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: eviction.GetGeneration(),
					Reason:             condition.ClusterResourcePlacementEvictionValidReason,
					Message:            isValidEviction.Msg,
				}
				conditions = append(conditions, validCondition)
			} else {
				invalidCondition := metav1.Condition{
					Type:               string(placementv1beta1.PlacementEvictionConditionTypeValid),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: eviction.GetGeneration(),
					Reason:             condition.ClusterResourcePlacementEvictionInvalidReason,
					Message:            isValidEviction.Msg,
				}
				conditions = append(conditions, invalidCondition)
			}
		}
		if isExecutedEviction != nil {
			if isExecutedEviction.IsExecuted {
				executedCondition := metav1.Condition{
					Type:               string(placementv1beta1.PlacementEvictionConditionTypeExecuted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: eviction.GetGeneration(),
					Reason:             condition.ClusterResourcePlacementEvictionExecutedReason,
					Message:            isExecutedEviction.Msg,
				}
				conditions = append(conditions, executedCondition)
			} else {
				notExecutedCondition := metav1.Condition{
					Type:               string(placementv1beta1.PlacementEvictionConditionTypeExecuted),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: eviction.GetGeneration(),
					Reason:             condition.ClusterResourcePlacementEvictionNotExecutedReason,
					Message:            isExecutedEviction.Msg,
				}
				conditions = append(conditions, notExecutedCondition)
			}
		}
		wantStatus := placementv1beta1.PlacementEvictionStatus{
			Conditions: conditions,
		}
		if diff := cmp.Diff(eviction.Status, wantStatus, evictionStatusCmpOptions...); diff != "" {
			return fmt.Errorf("CRP status diff (-got, +want): %s", diff)
		}
		return nil
	}
}

type IsValidEviction struct {
	IsValid bool
	Msg     string
}

type IsExecutedEviction struct {
	IsExecuted bool
	Msg        string
}

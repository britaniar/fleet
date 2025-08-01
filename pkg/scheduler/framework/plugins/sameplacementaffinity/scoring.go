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

package sameplacementaffinity

import (
	"context"

	clusterv1beta1 "go.goms.io/fleet/apis/cluster/v1beta1"
	placementv1beta1 "go.goms.io/fleet/apis/placement/v1beta1"
	"go.goms.io/fleet/pkg/scheduler/framework"
)

// Score allows the plugin to connect to the Score extension point in the scheduling framework.
func (p *Plugin) Score(
	_ context.Context,
	state framework.CycleStatePluginReadWriter,
	_ placementv1beta1.PolicySnapshotObj,
	cluster *clusterv1beta1.MemberCluster,
) (score *framework.ClusterScore, status *framework.Status) {
	if state.HasObsoleteBindingFor(cluster.Name) {
		return &framework.ClusterScore{ObsoletePlacementAffinityScore: 1}, nil
	}
	// All done.
	return &framework.ClusterScore{ObsoletePlacementAffinityScore: 0}, nil
}

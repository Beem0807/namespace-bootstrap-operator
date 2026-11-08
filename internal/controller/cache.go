/*
Copyright 2026.

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

package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
)

// CacheOptions bounds what the manager keeps in memory.
//
// The manager's client is cache-backed, so reading one object of a kind makes an
// informer LIST and WATCH that kind cluster-wide. Left at the defaults this
// operator would hold every Namespace, ResourceQuota, LimitRange and
// NetworkPolicy in the cluster, as full objects — which on a cluster with a few
// thousand namespaces is the difference between running and being OOM-killed.
//
// Lives here rather than in cmd/main.go so the integration suite starts its
// manager with the same configuration. A cache narrowed incorrectly does not
// error; it silently returns NotFound, so the production settings have to be the
// ones under test.
func CacheOptions() cache.Options {
	return cache.Options{
		// managedFields is usually the largest part of an object nothing reads,
		// and server-side apply makes it larger still. Ownership here is decided
		// from labels, and the applied generation is tracked in an annotation
		// rather than read back out of managedFields — see api/v1alpha1/wellknown.go,
		// which says so explicitly.
		DefaultTransform: func(obj any) (any, error) {
			if o, ok := obj.(client.Object); ok {
				o.SetManagedFields(nil)
			}
			return obj, nil
		},
		ByObject: map[client.Object]cache.ByObject{
			// Both are only ever read at their one deterministic name, so caching
			// every other quota and limit range in the cluster is pure cost.
			&corev1.ResourceQuota{}: {
				Field: fields.OneTermEqualSelector("metadata.name", platformv1alpha1.ResourceQuotaName),
			},
			&corev1.LimitRange{}: {
				Field: fields.OneTermEqualSelector("metadata.name", platformv1alpha1.LimitRangeName),
			},
			// NetworkPolicy is deliberately not narrowed. Cleanup lists policies
			// by label to catch ones left under names an older version generated,
			// and conflict detection has to see foreign policies occupying the
			// operator's names. Either scoping would silently break one of them.
		},
	}
}

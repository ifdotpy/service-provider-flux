// Copyright 2026.
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

package flux

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManageNamespace ensures that a namespace required by remotely configured
// controllers exists on the MCP and is retained because other services can share it.
func ManageNamespace(cluster ManagedCluster, name string) ManagedObject {
	namespace := NewManagedObject(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, ManagedObjectContext{
		ReconcileFunc: func(_ context.Context, object client.Object) error {
			if _, ok := object.(*corev1.Namespace); !ok {
				return fmt.Errorf("expected *corev1.Namespace, got %T", object)
			}
			return nil
		},
		DeletionPolicy: Orphan,
		StatusFunc:     SimpleStatus,
	})
	cluster.AddObject(namespace)
	return namespace
}

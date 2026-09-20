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

// RemoteCredentialName is the managed copy of the MCP credential on the platform cluster.
const RemoteCredentialName = "sp-flux-mcp-access"

// ManageMCPCredential copies the MCP credential without changing its API target.
// Helm must uninstall the controllers before this credential can be removed.
func ManageMCPCredential(cluster ManagedCluster, source client.Client, key client.ObjectKey) ManagedObject {
	object := NewManagedObject(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: RemoteCredentialName, Namespace: cluster.GetDefaultNamespace()}}, ManagedObjectContext{
		ReconcileFunc: func(ctx context.Context, object client.Object) error {
			original := &corev1.Secret{}
			if err := source.Get(ctx, key, original); err != nil {
				return err
			}
			data := original.Data["kubeconfig"]
			if len(data) == 0 {
				return fmt.Errorf("MCP credential does not contain kubeconfig")
			}
			secret := object.(*corev1.Secret)
			secret.Type = corev1.SecretTypeOpaque
			secret.Data = map[string][]byte{"kubeconfig": append([]byte(nil), data...)}
			return nil
		}, DeletionPolicy: Delete, StatusFunc: SecretStatus,
	})
	cluster.AddObject(object)
	return object
}

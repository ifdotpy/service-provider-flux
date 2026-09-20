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
	"testing"

	controllerclusters "github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMCPCredentialCopyAndRotation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	original := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "platform"}, Data: map[string][]byte{"kubeconfig": []byte("first"), "unrelated": []byte("not copied")}}
	source := fake.NewClientBuilder().WithScheme(scheme).WithObjects(original).Build()
	workload := fake.NewClientBuilder().WithScheme(scheme).Build()
	cluster := NewManagedCluster(controllerclusters.NewTestClusterFromClient("workload", workload), &rest.Config{}, "tenant", WorkloadCluster)
	credential := ManageMCPCredential(cluster, source, client.ObjectKeyFromObject(original))
	require.NoError(t, credential.Reconcile(ctx))
	target := credential.GetObject().(*corev1.Secret)
	require.Equal(t, "tenant", target.Namespace)
	require.Equal(t, map[string][]byte{"kubeconfig": []byte("first")}, target.Data)
	original.Data["kubeconfig"] = []byte("rotated")
	require.NoError(t, source.Update(ctx, original))
	require.NoError(t, credential.Reconcile(ctx))
	require.Equal(t, []byte("rotated"), target.Data["kubeconfig"])
	delete(original.Data, "kubeconfig")
	require.NoError(t, source.Update(ctx, original))
	require.ErrorContains(t, credential.Reconcile(ctx), "does not contain kubeconfig")
	require.NoError(t, source.Delete(ctx, original))
	require.Error(t, credential.Reconcile(ctx))
}

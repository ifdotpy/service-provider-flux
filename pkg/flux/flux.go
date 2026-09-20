// Copyright 2025.
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
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider/clusteraccess"

	apiv1alpha1 "github.com/openmcp-project/service-provider-flux/api/v1alpha1"
)

const (
	// DefaultFluxNamespace is the default namespace where Flux components are deployed on the ManagedControlPlane
	DefaultFluxNamespace = "flux-system"
	// OCIRepositoryName is the name of the Flux OCIRepository resource
	OCIRepositoryName = "flux"
	// HelmReleaseName is the name of the Flux HelmRelease resource
	HelmReleaseName = "flux"
)

// ManageFluxResourcesParams groups all parameters to create the required manage flux resources
type ManageFluxResourcesParams struct {
	// Cluster defines where the resources will be created
	Cluster ManagedCluster
	// MCPNamespace defines the namespace name that deploy ESO
	MCPNamespace string
	// ChartPullSecretName defines the name of the secret copy that will be placed in the Cluster namespace
	ChartPullSecretName string
	// Obj is the tenant API object that is being reconciled
	Obj *apiv1alpha1.Flux
	// ProviderConfig of the current reconciliation context
	ProviderConfig *apiv1alpha1.ProviderConfig
	// ClusterContext of the current reconciliation context
	ClusterContext clusteraccess.ClusterContext
	// RequestedVersion is the version of flux that a user requested through the onboarding API
	RequestedVersion apiv1alpha1.FluxVersion
	// ControllersOnPlatform installs the chart on the platform cluster and configures
	// its controllers to address the MCP through the mounted access credential.
	ControllersOnPlatform bool
	// RemoteNamespace is the MCP namespace used by the locally running controllers.
	RemoteNamespace  ManagedObject
	RemoteCredential ManagedObject
}

// ManageFluxResources configures OCIRepository and HelmRelease on the platform cluster.
func ManageFluxResources(p ManageFluxResourcesParams) {
	ociRepo := NewManagedObject(&sourcev1.OCIRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OCIRepositoryName,
			Namespace: p.Cluster.GetDefaultNamespace(),
		},
	}, ManagedObjectContext{
		ReconcileFunc: func(_ context.Context, o client.Object) error {
			ociRepo, ok := o.(*sourcev1.OCIRepository)
			if !ok {
				return fmt.Errorf("expected *sourcev1.OCIRepository, got %T", o)
			}
			if p.RequestedVersion.ChartURL == nil {
				// this should never happen as long as defaulting works properly
				return fmt.Errorf("missing ChartURL definition for Flux version %s", p.RequestedVersion.Version)
			}
			ociRepo.Spec = sourcev1.OCIRepositorySpec{
				Interval: metav1.Duration{Duration: p.ProviderConfig.PollInterval()},
				URL:      *p.RequestedVersion.ChartURL,
				Reference: &sourcev1.OCIRepositoryRef{
					Tag: p.RequestedVersion.ChartVersion,
				},
			}
			if p.ChartPullSecretName != "" {
				ociRepo.Spec.SecretRef = &meta.LocalObjectReference{
					Name: p.ChartPullSecretName,
				}
			}
			return nil
		},
		DependsOn:      []ManagedObject{},
		DeletionPolicy: Delete,
		StatusFunc:     FluxStatus,
	})
	p.Cluster.AddObject(ociRepo)

	helmRelease := NewManagedObject(&helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HelmReleaseName,
			Namespace: p.Cluster.GetDefaultNamespace(),
		},
	}, ManagedObjectContext{
		ReconcileFunc: func(_ context.Context, o client.Object) error {
			helmRelease, ok := o.(*helmv2.HelmRelease)
			if !ok {
				return fmt.Errorf("expected *helmv2.HelmRelease, got %T", o)
			}
			installCRDs := helmv2.Create
			upgradeCRDs := helmv2.CreateReplace
			var kubeConfig *meta.KubeConfigReference
			if p.ControllersOnPlatform {
				// A nil kubeconfig installs into the platform cluster hosting Helm.
				installCRDs = helmv2.Skip
				upgradeCRDs = helmv2.Skip
			} else {
				kubeConfig = &meta.KubeConfigReference{
					SecretRef: &meta.SecretKeyReference{
						Name: p.ClusterContext.MCPAccessSecretKey.Name,
						Key:  "kubeconfig",
					},
				}
			}
			helmRelease.Spec = helmv2.HelmReleaseSpec{
				Interval: metav1.Duration{Duration: p.ProviderConfig.PollInterval()},
				ChartRef: &helmv2.CrossNamespaceSourceReference{
					Kind:      "OCIRepository",
					Name:      OCIRepositoryName,
					Namespace: p.Cluster.GetDefaultNamespace(),
				},
				KubeConfig: kubeConfig,
				Install: &helmv2.Install{
					CRDs:            installCRDs,
					CreateNamespace: !p.ControllersOnPlatform,
					Remediation: &helmv2.InstallRemediation{
						Retries: 3,
					},
				},
				Upgrade: &helmv2.Upgrade{
					CRDs: upgradeCRDs,
					Remediation: &helmv2.UpgradeRemediation{
						Retries:  3,
						Strategy: ptr.To(helmv2.RollbackRemediationStrategy),
					},
				},
				Uninstall: &helmv2.Uninstall{
					KeepHistory: false,
					Timeout:     &metav1.Duration{Duration: 5 * time.Minute},
				},
				DriftDetection: &helmv2.DriftDetection{
					Mode: helmv2.DriftDetectionEnabled,
				},
				Values:           p.RequestedVersion.Values,
				TargetNamespace:  p.MCPNamespace,
				StorageNamespace: p.MCPNamespace,
			}
			return nil
		},
		DependsOn:      compactDependencies(ociRepo, p.RemoteNamespace, p.RemoteCredential),
		DeletionPolicy: Delete,
		StatusFunc:     FluxStatus,
	})
	p.Cluster.AddObject(helmRelease)
}

func compactDependencies(objects ...ManagedObject) []ManagedObject {
	result := make([]ManagedObject, 0, len(objects))
	for _, object := range objects {
		if object != nil {
			result = append(result, object)
		}
	}
	return result
}

// FluxStatus indicates whether the given Flux object is in phase terminating, pending or ready.
func FluxStatus(o client.Object, rl apiv1alpha1.ResourceLocation) Status { //nolint:revive // name matches ESO pattern
	fluxObject, ok := o.(conditions.Getter)
	if !ok {
		return Status{
			Phase:    apiv1alpha1.Unknown,
			Message:  fmt.Sprintf("Object %T does not implement conditions.Getter", o),
			Location: rl,
		}
	}
	if !o.GetDeletionTimestamp().IsZero() {
		return Status{
			Phase:    apiv1alpha1.Terminating,
			Message:  "Resource is terminating.",
			Location: rl,
		}
	}
	if conditions.IsTrue(fluxObject, meta.ReadyCondition) {
		return Status{
			Phase:    apiv1alpha1.Ready,
			Message:  "Resource is ready",
			Location: rl,
		}
	}
	return Status{
		Phase:    apiv1alpha1.Pending,
		Message:  "Resource is not ready",
		Location: rl,
	}
}

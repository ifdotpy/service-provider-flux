/*
Copyright 2025.

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
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	ctrlerrors "github.com/openmcp-project/controller-utils/pkg/errors"
	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider"
	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider/clusteraccess"
	libutils "github.com/openmcp-project/openmcp-operator/lib/utils"

	apiv1alpha1 "github.com/openmcp-project/service-provider-flux/api/v1alpha1"
	"github.com/openmcp-project/service-provider-flux/pkg/flux"
)

const conditionReasonError = "ReconcileError"

// ErrManagedResources is an end-user facing error if errors are present inside Flux.Status.ManagedResources
var ErrManagedResources = errors.New("resources contain reconcile errors")

// FluxReconciler reconciles a Flux object
type FluxReconciler struct {
	// OnboardingCluster is the cluster where this controller watches Flux resources and reacts to their changes.
	OnboardingCluster *clusters.Cluster
	// PlatformCluster is the cluster where this controller is deployed and configured.
	PlatformCluster *clusters.Cluster
	// PodNamespace is the namespace where this controller is deployed in.
	PodNamespace string
}

// CreateOrUpdate is called on every add or update event
func (r *FluxReconciler) CreateOrUpdate(ctx context.Context, obj *apiv1alpha1.Flux, pc *apiv1alpha1.ProviderConfig, clusters clusteraccess.ClusterContext) (ctrl.Result, error) {
	serviceprovider.StatusProgressing(obj, "Reconciling", "Reconcile in progress")
	mgr, err := r.createObjectManager(ctx, obj, pc, clusters)
	if err != nil {
		serviceprovider.StatusProgressing(obj, conditionReasonError, err.Error())
		return ctrl.Result{}, ctrlerrors.IgnoreInvalidUserInput(err)
	}
	results, err := mgr.Apply(ctx)
	managedResources, resultContainsErrors := resultsToResources(ctx, results)
	obj.Status.Resources = managedResources
	if allResourcesReady(managedResources) && err == nil {
		serviceprovider.StatusReady(obj)
	}
	if resultContainsErrors || err != nil {
		return ctrl.Result{}, updateStatusError(obj, resultContainsErrors, err)
	}
	return ctrl.Result{}, nil
}

// Delete is called on every delete event
func (r *FluxReconciler) Delete(ctx context.Context, obj *apiv1alpha1.Flux, pc *apiv1alpha1.ProviderConfig, clusters clusteraccess.ClusterContext) (ctrl.Result, error) {
	serviceprovider.StatusTerminating(obj)
	mgr, err := r.createObjectManager(ctx, obj, pc, clusters)
	if err != nil {
		serviceprovider.StatusProgressing(obj, conditionReasonError, err.Error())
		return ctrl.Result{}, ctrlerrors.IgnoreInvalidUserInput(err)
	}
	results, err := mgr.Delete(ctx)
	managedResources, resultContainsErrors := resultsToResources(ctx, results)
	obj.Status.Resources = managedResources
	if flux.AllDeleted(results) && err == nil {
		return ctrl.Result{}, nil
	}
	if resultContainsErrors || err != nil {
		return ctrl.Result{}, updateStatusError(obj, resultContainsErrors, err)
	}
	return ctrl.Result{
		RequeueAfter: time.Second * 5,
	}, nil
}

func updateStatusError(obj *apiv1alpha1.Flux, resourceErrors bool, err error) error {
	if resourceErrors {
		err = errors.Join(ErrManagedResources, err)
	}
	serviceprovider.StatusProgressing(obj, conditionReasonError, userErrorMessage(err))
	return ctrlerrors.IgnoreInvalidUserInput(err)
}

// userErrorMessage constructs an end-user facing error message.
// Only end-user errors are processed.
func userErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	errorMessages := []string{}
	if errors.Is(err, ErrManagedResources) {
		errorMessages = append(errorMessages, ErrManagedResources.Error())
	}
	if errors.Is(err, flux.ErrSecretCleanup) {
		errorMessages = append(errorMessages, flux.ErrSecretCleanup.Error())
	}
	if errors.Is(err, flux.ErrConfigMapCleanup) {
		errorMessages = append(errorMessages, flux.ErrConfigMapCleanup.Error())
	}
	return strings.Join(errorMessages, "; ")
}

func (r *FluxReconciler) createObjectManager(ctx context.Context, obj *apiv1alpha1.Flux, pc *apiv1alpha1.ProviderConfig, clusters clusteraccess.ClusterContext) (flux.Manager, error) {
	// In the multicluster (kcp) deployment mode the logical cluster of the
	// tenant qualifies the namespace, matching the derivation used for the
	// cluster access objects and by the ControlPlane controller
	// (StableMCPNamespaceCtx). In the classic mode no cluster is set in the
	// context and the derivation is unchanged.
	onboardingNamespace := obj.Namespace
	if cluster, ok := mccontext.ClusterFrom(ctx); ok && cluster != "" {
		onboardingNamespace = string(cluster) + "_" + onboardingNamespace
	}
	tenantNamespace, err := libutils.StableMCPNamespace(obj.Name, onboardingNamespace)
	if err != nil {
		return nil, fmt.Errorf("failed to determine tenant namespace for Flux deployment: %w", err)
	}

	// select requested version from provider config
	fluxVersion, err := selectFluxVersion(obj.Spec.Version, pc)
	if err != nil {
		return nil, err
	}

	// Extract helm values to determine namespace and image pull secrets
	helmValues, err := flux.ExtractHelmValues(fluxVersion.Values)
	if err != nil {
		return nil, fmt.Errorf("failed to extract helm values: %w", err)
	}

	// Create managed clusters
	platformCluster := flux.NewManagedCluster(r.PlatformCluster, r.PlatformCluster.RESTConfig(), tenantNamespace, flux.PlatformCluster)

	// Support namespace override from Helm values
	fluxNamespace := flux.DefaultFluxNamespace
	if helmValues.NamespaceOverride != "" {
		fluxNamespace = helmValues.NamespaceOverride
	}
	mcpCluster := flux.NewManagedCluster(clusters.MCPCluster, clusters.MCPCluster.RESTConfig(), fluxNamespace, flux.ManagedControlPlane)

	// Sync image pull secrets from platform cluster to MCP
	flux.ManagePullSecrets(mcpCluster, helmValues.ImagePullSecrets, flux.SecretCopyConfig{
		SourceClient:    r.PlatformCluster.Client(),
		SourceNamespace: r.PodNamespace,
		TargetNamespace: fluxNamespace,
	})

	// Sync chart pull secret within platform cluster from pod namespace to tenant namespace
	var prefixedChartPullSecret string
	if fluxVersion.ChartPullSecret != "" {
		prefixedChartPullSecret, err = flux.PrefixSecretName(fluxVersion.ChartPullSecret)
		if err != nil {
			return nil, fmt.Errorf("error generating secret name: %w", err)
		}
		flux.ManagePullSecrets(platformCluster, []corev1.LocalObjectReference{
			{Name: fluxVersion.ChartPullSecret},
		}, flux.SecretCopyConfig{
			SourceClient:    r.PlatformCluster.Client(),
			SourceNamespace: r.PodNamespace,
			TargetNamespace: tenantNamespace,
			TargetName:      prefixedChartPullSecret,
		})
	}

	if pc.Spec.CABundleRef != nil {
		// add custom ca volume, volumeMount and envVar to helm values
		fluxVersion.Values, err = flux.AddCAToHelmValues(fluxVersion.Values, pc.Spec.CABundleRef)
		if err != nil {
			return nil, fmt.Errorf("failed to add ca volume to helm values: %w", err)
		}

		// Sync ca configmap from platform cluster to MCP
		flux.ManageCaConfigMap(mcpCluster, pc.Spec.CABundleRef.LocalObjectReference, flux.ConfigMapCopyConfig{
			SourceClient:    r.PlatformCluster.Client(),
			SourceNamespace: r.PodNamespace,
			TargetNamespace: fluxNamespace,
			TargetName:      flux.CustomCABundleConfigMapName,
		})

	}

	// Configure Flux resources (OCIRepository and HelmRelease)
	flux.ManageFluxResources(flux.ManageFluxResourcesParams{
		Cluster:             platformCluster,
		MCPNamespace:        fluxNamespace,
		ChartPullSecretName: prefixedChartPullSecret,
		Obj:                 obj,
		ProviderConfig:      pc,
		ClusterContext:      clusters,
		RequestedVersion:    fluxVersion,
	})

	// Create manager and add clusters
	mgr := flux.NewManager()
	mgr.AddCluster(mcpCluster)
	mgr.AddCluster(platformCluster)

	// create cleaners to remove orphaned pull secret copies
	platformCleaner := flux.NewSecretCleaner(platformCluster, tenantNamespace, []corev1.LocalObjectReference{
		{
			Name: prefixedChartPullSecret,
		},
	})
	controlPlaneSecretCleaner := flux.NewSecretCleaner(mcpCluster, fluxNamespace, helmValues.ImagePullSecrets)

	configMapsToKeep := []corev1.LocalObjectReference{}
	if pc.Spec.CABundleRef != nil {
		configMapsToKeep = append(configMapsToKeep, corev1.LocalObjectReference{Name: flux.CustomCABundleConfigMapName})
	}

	controlPlaneConfigMapCleaner := flux.NewConfigMapCleaner(mcpCluster, fluxNamespace, configMapsToKeep)

	mgr.AddCleaner(platformCleaner)
	mgr.AddCleaner(controlPlaneSecretCleaner)
	mgr.AddCleaner(controlPlaneConfigMapCleaner)

	return mgr, nil
}

func selectFluxVersion(requestedVersion string, pc *apiv1alpha1.ProviderConfig) (apiv1alpha1.FluxVersion, error) {
	for _, configVersion := range pc.Spec.Versions {
		if configVersion.Version == requestedVersion {
			return configVersion, nil
		}
	}
	return apiv1alpha1.FluxVersion{}, fmt.Errorf("%w: requested version (%s) is not available", ctrlerrors.ErrInvalidUserInput, requestedVersion)
}

func resultsToResources(ctx context.Context, results []flux.Result) ([]apiv1alpha1.ManagedResource, bool) {
	l := log.FromContext(ctx)
	containsError := false
	resources := make([]apiv1alpha1.ManagedResource, 0, len(results))
	for _, res := range results {
		obj := res.Object.GetObject()
		status := res.Object.GetStatus(apiv1alpha1.ResourceLocation(res.Cluster.GetClusterType()))
		resources = append(resources, apiv1alpha1.ManagedResource{
			TypedObjectReference: corev1.TypedObjectReference{
				Kind:      reflect.TypeOf(obj).Elem().Name(),
				Name:      obj.GetName(),
				Namespace: nilIfEmptyString(obj.GetNamespace()),
			},
			Phase:    status.Phase,
			Message:  status.Message,
			Location: status.Location,
		})
		if res.Error != nil {
			containsError = true
			l.Error(res.Error, "object reconcile failed", "objectID", flux.ObjectID(obj))
		}
	}
	return resources, containsError
}

func nilIfEmptyString(str string) *string {
	if str == "" {
		return nil
	}
	return ptr.To(str)
}

func allResourcesReady(resources []apiv1alpha1.ManagedResource) bool {
	for _, res := range resources {
		if res.Phase != apiv1alpha1.Ready {
			return false
		}
	}
	return true
}

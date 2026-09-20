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
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	ctrlerrors "github.com/openmcp-project/controller-utils/pkg/errors"
	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider"
	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider/clusteraccess"
	libutils "github.com/openmcp-project/openmcp-operator/lib/utils"

	apiv1alpha1 "github.com/openmcp-project/service-provider-flux/api/v1alpha1"
	"github.com/openmcp-project/service-provider-flux/pkg/flux"
)

const conditionReasonError = "ReconcileError"

// Placement selects where the managed service controllers are installed.
type Placement string

const (
	// PlacementMCP installs the service controllers on the managed control plane.
	PlacementMCP Placement = "mcp"
	// PlacementPlatform installs the service controllers on the existing platform cluster.
	// The controllers use the MCP access credential as their kubeconfig.
	PlacementPlatform Placement = "platform"
)

// Validate checks whether the controller cluster value is supported.
func (c Placement) Validate() error {
	if c != PlacementMCP && c != PlacementPlatform {
		return fmt.Errorf("service controller cluster must be %q or %q, got %q", PlacementMCP, PlacementPlatform, c)
	}
	return nil
}

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
	// Placement selects where the managed Flux controllers run.
	Placement Placement
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
	tenantNamespace, err := libutils.StableMCPNamespace(obj.Name, obj.Namespace)
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
	controllersOnPlatform := r.Placement == PlacementPlatform

	// Support namespace override from Helm values
	fluxNamespace := installationNamespace(flux.DefaultFluxNamespace, helmValues.NamespaceOverride, tenantNamespace, controllersOnPlatform)
	mcpNamespace := fluxNamespace
	mcpCluster := flux.NewManagedCluster(clusters.MCPCluster, clusters.MCPCluster.RESTConfig(), mcpNamespace, flux.ManagedControlPlane)
	var remoteNamespace flux.ManagedObject
	var credential flux.ManagedObject

	if controllersOnPlatform {
		if err := r.configureRemoteVersion(ctx, obj.DeletionTimestamp.IsZero(), &fluxVersion, tenantNamespace, clusters.MCPAccessSecretKey); err != nil {
			return nil, err
		}
		remoteNamespace = flux.ManageNamespace(mcpCluster, tenantNamespace)
	}

	// Sync image pull secrets from platform cluster to MCP
	controllerCluster := mcpCluster
	controllerNamespace := fluxNamespace
	if controllersOnPlatform {
		controllerCluster = platformCluster
		credential = flux.ManageMCPCredential(controllerCluster, r.PlatformCluster.Client(), clusters.MCPAccessSecretKey)
		controllerNamespace = tenantNamespace
	}
	flux.ManagePullSecrets(controllerCluster, helmValues.ImagePullSecrets, flux.SecretCopyConfig{
		SourceClient:    r.PlatformCluster.Client(),
		SourceNamespace: r.PodNamespace,
		TargetNamespace: controllerNamespace,
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

	if err := r.configureCA(controllerCluster, pc, &fluxVersion); err != nil {
		return nil, err
	}

	// Configure Flux resources (OCIRepository and HelmRelease)
	flux.ManageFluxResources(flux.ManageFluxResourcesParams{
		Cluster:               platformCluster,
		MCPNamespace:          controllerNamespace,
		ChartPullSecretName:   prefixedChartPullSecret,
		Obj:                   obj,
		ProviderConfig:        pc,
		ClusterContext:        clusters,
		RequestedVersion:      fluxVersion,
		ControllersOnPlatform: controllersOnPlatform,
		RemoteNamespace:       remoteNamespace,
		RemoteCredential:      credential,
	})

	// Create manager and add clusters
	mgr := flux.NewManager()
	mgr.AddCluster(mcpCluster)
	mgr.AddCluster(platformCluster)

	addCleaners(mgr, platformCluster, controllerCluster, helmValues, prefixedChartPullSecret, pc.Spec.CABundleRef != nil, controllersOnPlatform)

	return mgr, nil
}

func (r *FluxReconciler) mcpCredentialHash(ctx context.Context, key client.ObjectKey) (string, error) {
	secret := &corev1.Secret{}
	if err := r.PlatformCluster.Client().Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("failed to read MCP access credential: %w", err)
	}
	kubeconfig, ok := secret.Data["kubeconfig"]
	if !ok || len(kubeconfig) == 0 {
		return "", fmt.Errorf("MCP access credential %s does not contain kubeconfig", key)
	}
	return fmt.Sprintf("%x", sha256.Sum256(kubeconfig)), nil
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

func installationNamespace(defaultNamespace, override, tenantNamespace string, onPlatform bool) string {
	if onPlatform {
		return tenantNamespace
	}
	if override != "" {
		return override
	}
	return defaultNamespace
}

func (r *FluxReconciler) configureRemoteVersion(ctx context.Context, active bool, version *apiv1alpha1.FluxVersion, namespace string, key client.ObjectKey) error {
	hash := ""
	var err error
	if active {
		hash, err = r.mcpCredentialHash(ctx, key)
		if err != nil {
			return err
		}
	}
	version.Values, err = flux.ConfigureRemoteMCPControllers(version.Values, flux.RemoteCredentialName, namespace, hash)
	return err
}

func (r *FluxReconciler) configureCA(controllerCluster flux.ManagedCluster, pc *apiv1alpha1.ProviderConfig, fluxVersion *apiv1alpha1.FluxVersion) error {
	if pc.Spec.CABundleRef != nil {
		// add custom ca volume, volumeMount and envVar to helm values
		var err error
		fluxVersion.Values, err = flux.AddCAToHelmValues(fluxVersion.Values, pc.Spec.CABundleRef)
		if err != nil {
			return fmt.Errorf("failed to add ca volume to helm values: %w", err)
		}

		// Sync ca configmap from platform cluster to MCP
		flux.ManageCaConfigMap(controllerCluster, pc.Spec.CABundleRef.LocalObjectReference, flux.ConfigMapCopyConfig{
			SourceClient:    r.PlatformCluster.Client(),
			SourceNamespace: r.PodNamespace,
			TargetNamespace: controllerCluster.GetDefaultNamespace(),
			TargetName:      flux.CustomCABundleConfigMapName,
		})

	}

	return nil
}

func addCleaners(mgr flux.Manager, platformCluster, controllerCluster flux.ManagedCluster, helmValues *flux.HelmValues, prefixedChartPullSecret string, hasCA, controllersOnPlatform bool) {
	// create cleaners to remove orphaned pull secret copies
	platformSecrets := append([]corev1.LocalObjectReference{}, helmValues.ImagePullSecrets...)
	platformSecrets = append(platformSecrets, corev1.LocalObjectReference{Name: prefixedChartPullSecret}, corev1.LocalObjectReference{Name: flux.RemoteCredentialName})
	platformCleaner := flux.NewSecretCleaner(platformCluster, platformCluster.GetDefaultNamespace(), platformSecrets)
	secretsToKeep := append([]corev1.LocalObjectReference{}, helmValues.ImagePullSecrets...)
	if controllersOnPlatform {
		secretsToKeep = append(secretsToKeep, corev1.LocalObjectReference{Name: flux.RemoteCredentialName}, corev1.LocalObjectReference{Name: prefixedChartPullSecret})
	}
	controllerSecretCleaner := flux.NewSecretCleaner(controllerCluster, controllerCluster.GetDefaultNamespace(), secretsToKeep)

	configMapsToKeep := []corev1.LocalObjectReference{}
	if hasCA {
		configMapsToKeep = append(configMapsToKeep, corev1.LocalObjectReference{Name: flux.CustomCABundleConfigMapName})
	}

	controllerConfigMapCleaner := flux.NewConfigMapCleaner(controllerCluster, controllerCluster.GetDefaultNamespace(), configMapsToKeep)

	mgr.AddCleaner(platformCleaner)
	mgr.AddCleaner(controllerSecretCleaner)
	mgr.AddCleaner(controllerConfigMapCleaner)

}

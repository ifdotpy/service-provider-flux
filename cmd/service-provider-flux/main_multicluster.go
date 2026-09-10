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

package main

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	"github.com/kcp-dev/multicluster-provider/apiexport"
	kcpapisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	kcpcorev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/controller-utils/pkg/logging"
	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider"
	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider/clusteraccess"

	fluxsv1alpha1 "github.com/openmcp-project/service-provider-flux/api/v1alpha1"
	"github.com/openmcp-project/service-provider-flux/internal/controller"
)

// runMulticluster runs the Flux service provider in the multicluster (kcp)
// deployment mode: instead of watching a single onboarding cluster, the
// controller consumes the APIExport virtual workspace named by
// --kcp-endpoint-slice and reconciles Flux objects in place, in every kcp
// workspace that bound the export. The provider seam (FluxReconciler) is
// unchanged.
func runMulticluster(
	log logging.Logger,
	platformCluster *clusters.Cluster,
	car clusteraccess.AdvancedProvider,
	podNamespace, providerName, kcpEndpointSlice, kcpKubeconfig, probeAddr string,
	metricsServerOptions metricsserver.Options,
) error {
	log.Info("Multicluster (kcp) mode", "endpointSlice", kcpEndpointSlice)

	scheme := runtime.NewScheme()
	utilruntime.Must(fluxsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(kcpapisv1alpha1.AddToScheme(scheme))
	utilruntime.Must(kcpcorev1alpha1.AddToScheme(scheme))

	cfg, err := clientcmd.BuildConfigFromFlags("", kcpKubeconfig)
	if err != nil {
		return fmt.Errorf("unable to load kcp kubeconfig: %w", err)
	}

	logr := log.Logr()
	provider, err := apiexport.New(cfg, kcpEndpointSlice, apiexport.Options{
		Scheme: scheme,
		Log:    &logr,
	})
	if err != nil {
		return fmt.Errorf("unable to construct apiexport provider: %w", err)
	}

	mcMgr, err := mcmanager.New(cfg, provider, manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		// Leader election intentionally off in the first multicluster
		// increment; run a single replica.
	})
	if err != nil {
		return fmt.Errorf("unable to create multicluster manager: %w", err)
	}

	if err := mcMgr.GetLocalManager().Add(platformCluster.Cluster()); err != nil {
		return fmt.Errorf("unable to add platform cluster to manager: %w", err)
	}

	spr := serviceprovider.NewAPIReconcilerBuilder[*fluxsv1alpha1.Flux, *fluxsv1alpha1.ProviderConfig]().
		EmptyObjectProvider(func() *fluxsv1alpha1.Flux { return &fluxsv1alpha1.Flux{} }).
		EmptyConfigProvider(func() *fluxsv1alpha1.ProviderConfig { return &fluxsv1alpha1.ProviderConfig{} }).
		PlatformCluster(platformCluster).
		Reconciler(&controller.FluxReconciler{
			PlatformCluster: platformCluster,
			PodNamespace:    podNamespace,
		}).
		AdvancedClusterAccessReconciler(car).
		MustBuildMulticluster()

	if err := spr.SetupWithMulticlusterManager(mcMgr, providerName); err != nil {
		return fmt.Errorf("unable to create controller: %w", err)
	}

	if err := mcMgr.GetLocalManager().AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mcMgr.GetLocalManager().AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	log.Info("starting multicluster manager")
	if err := mcMgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("problem running multicluster manager: %w", err)
	}
	return nil
}

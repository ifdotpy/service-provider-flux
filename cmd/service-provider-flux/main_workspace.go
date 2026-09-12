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
	"context"
	"encoding/json"
	"fmt"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	"github.com/kcp-dev/multicluster-provider/apiexport"
	kcpapisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	kcpcorev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"github.com/openmcp-project/controller-utils/pkg/logging"
	"github.com/openmcp-project/opencontrolplane-runtime/pkg/serviceprovider/workspace"

	fluxsv1alpha1 "github.com/openmcp-project/service-provider-flux/api/v1alpha1"
)

// runWorkspaceTarget runs the provider with the tenant workspace itself as the
// target: enabling the service in an account (APIBinding) installs a Flux
// instance for that account. The Flux controllers run on the platform cluster,
// in a namespace per workspace, and act on the workspace through a kubeconfig
// minted there (ServiceAccount token). The workspace binds the Flux APIs
// (GitRepository, Kustomization, HelmRelease, ...) through the same APIExport,
// so users work with the real Flux API in their account.
func runWorkspaceTarget(
	log logging.Logger,
	platformCluster *clusters.Cluster,
	podNamespace, providerName, kcpEndpointSlice, kcpKubeconfig, probeAddr string,
	metricsServerOptions metricsserver.Options,
) error {
	log.Info("Workspace-target (kcp) mode", "endpointSlice", kcpEndpointSlice)

	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(kcpapisv1alpha1.AddToScheme(scheme))
	utilruntime.Must(kcpcorev1alpha1.AddToScheme(scheme))
	// rbac + apiextensions are needed to mint the token and to read the bound APIs
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))

	cfg, err := clientcmd.BuildConfigFromFlags("", kcpKubeconfig)
	if err != nil {
		return fmt.Errorf("unable to load kcp kubeconfig: %w", err)
	}
	logr := log.Logr()
	provider, err := apiexport.New(cfg, kcpEndpointSlice, apiexport.Options{Scheme: scheme, Log: &logr})
	if err != nil {
		return fmt.Errorf("unable to construct apiexport provider: %w", err)
	}
	mcMgr, err := mcmanager.New(cfg, provider, manager.Options{
		Scheme: scheme, Metrics: metricsServerOptions, HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		return fmt.Errorf("unable to create multicluster manager: %w", err)
	}
	if err := mcMgr.GetLocalManager().Add(platformCluster.Cluster()); err != nil {
		return fmt.Errorf("unable to add platform cluster to manager: %w", err)
	}

	handler := &fluxWorkspaceHandler{
		platform:     platformCluster.Client(),
		providerCfg:  cfg,
		podNamespace: podNamespace,
		providerName: providerName,
		log:          log,
	}
	if err := mcMgr.Add(&workspace.Runner{Handler: handler, Log: log}); err != nil {
		return fmt.Errorf("unable to add workspace runner: %w", err)
	}
	if err := mcMgr.GetLocalManager().AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mcMgr.GetLocalManager().AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	log.Info("starting multicluster manager (workspace target)")
	return mcMgr.Start(ctrl.SetupSignalHandler())
}

// fluxWorkspaceHandler installs one Flux instance per workspace on the platform cluster.
type fluxWorkspaceHandler struct {
	platform     client.Client
	providerCfg  *rest.Config
	podNamespace string
	providerName string
	log          logging.Logger
}

const (
	wsFluxNamespace = "flux-system" // in the workspace: SA, token, Flux objects' home
	wsFluxSA        = "flux"
	kubeconfigKey   = "kubeconfig"
	kubeconfigMount = "/etc/kcp"
)

func (h *fluxWorkspaceHandler) platformNamespace(ws workspace.Workspace) string {
	return "flux-ws-" + ws.Name
}

func (h *fluxWorkspaceHandler) Ensure(ctx context.Context, ws workspace.Workspace) error {
	// 1. credential for the workspace
	kubeconfig, err := workspace.MintKubeconfig(ctx, ws, h.providerCfg, workspace.TokenSpec{
		Namespace: wsFluxNamespace, ServiceAccountName: wsFluxSA, ClusterRole: "cluster-admin",
	})
	if err != nil {
		return fmt.Errorf("minting workspace kubeconfig: %w", err)
	}
	// The controllers keep their leader-election Leases in the namespace they
	// run in (read from the mounted ServiceAccount, not configurable), so the
	// instance namespace also exists in the workspace.
	nsName := h.platformNamespace(ws)
	if err := ws.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("instance namespace in workspace: %w", err)
	}

	// 2. chart reference from the ProviderConfig
	pc := &fluxsv1alpha1.ProviderConfig{}
	if err := h.platform.Get(ctx, client.ObjectKey{Name: h.providerName}, pc); err != nil {
		return fmt.Errorf("reading ProviderConfig %q: %w", h.providerName, err)
	}
	if len(pc.Spec.Versions) == 0 {
		return fmt.Errorf("ProviderConfig %q has no versions", h.providerName)
	}
	ver := pc.Spec.Versions[0]
	chartURL := "oci://ghcr.io/fluxcd-community/charts/flux2"
	if ver.ChartURL != nil && *ver.ChartURL != "" {
		chartURL = *ver.ChartURL
	}

	// 3. platform side: namespace, kubeconfig secret, chart source, release
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName,
		Labels: map[string]string{"open-control-plane.io/workspace": ws.Name, "open-control-plane.io/service": "flux"}}}
	if err := h.platform.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("platform namespace: %w", err)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "workspace-kubeconfig", Namespace: nsName}}
	if _, err := ctrl.CreateOrUpdate(ctx, h.platform, sec, func() error {
		sec.Data = map[string][]byte{kubeconfigKey: kubeconfig}
		return nil
	}); err != nil {
		return fmt.Errorf("kubeconfig secret: %w", err)
	}
	repo := &sourcev1.OCIRepository{ObjectMeta: metav1.ObjectMeta{Name: "flux2", Namespace: nsName}}
	if _, err := ctrl.CreateOrUpdate(ctx, h.platform, repo, func() error {
		repo.Spec = sourcev1.OCIRepositorySpec{
			URL:       chartURL,
			Reference: &sourcev1.OCIRepositoryRef{Tag: ver.ChartVersion},
			Interval:  metav1.Duration{Duration: 10 * time.Minute},
			LayerSelector: &sourcev1.OCILayerSelector{
				MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip", Operation: sourcev1.OCILayerExtract},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("chart source: %w", err)
	}
	values, err := json.Marshal(fluxWorkspaceValues())
	if err != nil {
		return err
	}
	hr := &helmv2.HelmRelease{ObjectMeta: metav1.ObjectMeta{Name: "flux", Namespace: nsName}}
	if _, err := ctrl.CreateOrUpdate(ctx, h.platform, hr, func() error {
		hr.Spec = helmv2.HelmReleaseSpec{
			Interval: metav1.Duration{Duration: 10 * time.Minute},
			ChartRef: &helmv2.CrossNamespaceSourceReference{Kind: sourcev1.OCIRepositoryKind, Name: repo.Name, Namespace: nsName},
			Install:  &helmv2.Install{CreateNamespace: false, Remediation: &helmv2.InstallRemediation{Retries: 3}},
			Upgrade:  &helmv2.Upgrade{Remediation: &helmv2.UpgradeRemediation{Retries: 3}},
			Values:   &apiextensionsv1.JSON{Raw: values},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("helm release: %w", err)
	}
	_ = fluxmeta.ReadyCondition
	return nil
}

func (h *fluxWorkspaceHandler) Remove(ctx context.Context, ws workspace.Workspace) error {
	// Revoke the workspace credential first (the workspace client of ws no
	// longer works after a disengagement; Revoke uses the provider identity).
	if err := workspace.Revoke(ctx, h.providerCfg, ws.Name, workspace.TokenSpec{
		Namespace: wsFluxNamespace, ServiceAccountName: wsFluxSA, ClusterRole: "cluster-admin"}, h.platformNamespace(ws)); err != nil {
		h.log.Error(err, "revoking workspace credential", "workspace", ws.Name)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: h.platformNamespace(ws)}}
	// Deleting the HelmRelease first lets helm-controller uninstall cleanly.
	hr := &helmv2.HelmRelease{ObjectMeta: metav1.ObjectMeta{Name: "flux", Namespace: ns.Name}}
	if err := h.platform.Delete(ctx, hr); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting helm release: %w", err)
	}
	if err := h.platform.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting platform namespace: %w", err)
	}
	return nil
}

// fluxWorkspaceValues renders the flux2 chart values for a controller set that
// runs on the platform cluster but acts on the workspace: CRDs come from the
// APIBinding, RBAC on the platform is not needed, and every controller gets
// the workspace kubeconfig.
func fluxWorkspaceValues() map[string]any {
	remote := map[string]any{
		"create":       true,
		"extraEnv":     []map[string]any{{"name": "KUBECONFIG", "value": kubeconfigMount + "/" + kubeconfigKey}},
		"volumes":      []map[string]any{{"name": "workspace-kubeconfig", "secret": map[string]any{"secretName": "workspace-kubeconfig"}}},
		"volumeMounts": []map[string]any{{"name": "workspace-kubeconfig", "mountPath": kubeconfigMount, "readOnly": true}},
	}
	off := map[string]any{"create": false}
	return map[string]any{
		"installCRDs":               false,
		"rbac":                      map[string]any{"create": false},
		"watchAllNamespaces":        true,
		"sourceController":          remote,
		"kustomizeController":       remote,
		"helmController":            remote,
		"notificationController":    off,
		"imageReflectionController": off,
		"imageAutomationController": off,
	}
}

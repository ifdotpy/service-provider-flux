package onboarding

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/providers/kubeconfig"
)

// RegisteredNamespaceAccessKey validates the globally unique onboarding namespace
// against the registration Secret name.
// Reject objects outside that namespace before resolving any platform credentials.
func RegisteredNamespaceAccessKey(clusterName string, key client.ObjectKey) (client.ObjectKey, error) {
	if clusterName == "" || key.Namespace != clusterName {
		return client.ObjectKey{}, fmt.Errorf("object namespace %q does not match registered onboarding namespace %q", key.Namespace, clusterName)
	}
	return key, nil
}

// NewManagers selects single-cluster or Secret-based multicluster onboarding.
func NewManagers(platform, onboarding *clusters.Cluster, namespace, label string, options manager.Options, onboardingScheme *runtime.Scheme) (manager.Manager, mcmanager.Manager, error) {
	if label == "" {
		mgr, err := manager.New(onboarding.RESTConfig(), options)
		return mgr, nil, err
	}
	provider := kubeconfig.New(kubeconfig.Options{
		Namespace:             namespace,
		KubeconfigSecretLabel: label,
		ClusterOptions:        []cluster.Option{func(o *cluster.Options) { o.Scheme = onboardingScheme }},
	})
	options.LeaderElectionNamespace = namespace
	options.Cache.DefaultNamespaces = map[string]cache.Config{namespace: {}}
	mgr, err := mcmanager.New(platform.RESTConfig(), provider, options)
	if err != nil {
		return nil, nil, err
	}
	if err := provider.SetupWithManager(context.Background(), mgr); err != nil {
		return nil, nil, err
	}
	return mgr.GetLocalManager(), mgr, nil
}

package onboarding

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRegisteredNamespaceAccessKey(t *testing.T) {
	for _, tc := range []struct {
		cluster, namespace string
		allowed            bool
	}{
		{"tenant-a", "tenant-a", true}, {"tenant-a", "tenant-b", false}, {"", "", false},
	} {
		key := client.ObjectKey{Namespace: tc.namespace, Name: "default"}
		got, err := RegisteredNamespaceAccessKey(tc.cluster, key)
		if (err == nil) != tc.allowed {
			t.Fatalf("%+v: error = %v", tc, err)
		}
		if tc.allowed && got != key {
			t.Fatalf("existing identity changed: %v", got)
		}
	}
}

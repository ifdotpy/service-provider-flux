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
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const (
	// customCaVolumeName is the name of the custom ca volume and volume mount
	customCaVolumeName = "custom-ca-bundle"

	// customCaPath is the path the ca bundle will be mounted into
	customCaPath = "/etc/open-control-plane/custom-ca"

	// CustomCABundleConfigMapName is the fixed name for the copied CA bundle ConfigMap on the MCP cluster.
	CustomCABundleConfigMapName = "custom-ca-bundle"

	remoteMCPKubeconfigVolume = "mcp-kubeconfig"
	remoteMCPKubeconfigPath   = "/etc/open-control-plane/mcp/kubeconfig"
	credentialHashAnnotation  = "open-control-plane.io/mcp-credential-hash"
)

// certDirectories contains a list of places where the default system certs are stored in addition to caBundleMountDir
// from x509 go lib (https://github.com/golang/go/blob/015343854b5d9e2829481df30dbcae2ca6682d25/src/crypto/x509/root_linux.go)
var certDirectories = []string{
	"/etc/ssl/certs",
	"/etc/pki/tls/certs",
}

// ConfigureRemoteMCPControllers configures the Flux chart to run its controller
// pods on the Helm release cluster while they reconcile a remote MCP API.
// ProviderConfig values are retained except for settings required by this mode.
func ConfigureRemoteMCPControllers(values *apiextensionsv1.JSON, credentialSecret, namespace, credentialHash string) (*apiextensionsv1.JSON, error) {
	if credentialSecret == "" {
		return nil, errors.New("MCP credential secret name must be set")
	}
	if namespace == "" {
		return nil, errors.New("controller namespace must be set")
	}

	root, err := decodeRemoteValues(values)
	if err != nil {
		return nil, err
	}
	volume := corev1.Volume{
		Name: remoteMCPKubeconfigVolume,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: credentialSecret,
			Items:      []corev1.KeyToPath{{Key: "kubeconfig", Path: "kubeconfig"}},
		}},
	}
	mount := corev1.VolumeMount{Name: remoteMCPKubeconfigVolume, MountPath: "/etc/open-control-plane/mcp", ReadOnly: true}
	env := corev1.EnvVar{Name: "KUBECONFIG", Value: remoteMCPKubeconfigPath}
	for _, name := range fluxControllers {
		if err := addPodConfigToController(root, name, volume, mount, env); err != nil {
			return nil, err
		}
		if err := setCredentialAnnotation(root, name, credentialHash); err != nil {
			return nil, err
		}
	}
	root["installCRDs"] = json.RawMessage("false")
	root["namespaceOverride"], _ = json.Marshal(namespace)
	rbac := map[string]json.RawMessage{}
	if err := unmarshalIfPresent(root, "rbac", &rbac); err != nil {
		return nil, err
	}
	if rbac == nil {
		rbac = map[string]json.RawMessage{}
	}
	rbac["create"] = json.RawMessage("false")
	rbac["createAggregation"] = json.RawMessage("false")
	root["rbac"], _ = json.Marshal(rbac)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal helm values: %w", err)
	}
	return &apiextensionsv1.JSON{Raw: out}, nil
}

// nolint:goconst
var fluxControllers = []string{
	"helmController",
	"imageAutomationController",
	"imageReflectionController",
	"kustomizeController",
	"notificationController",
	"sourceController",
	"sourceWatcher",
}

// HelmValues defines the Helm values that are explicitly processed during reconciliation.
// The Flux Helm chart uses imagePullSecrets at the top level, not under global.
type HelmValues struct {
	// NamespaceOverride overrides the default flux-system namespace for the Flux deployment.
	NamespaceOverride string `json:"namespaceOverride,omitempty"`
	// ImagePullSecrets is a list of references to secrets used for pulling Flux controller images.
	// These secrets will be copied from the service provider's namespace to the flux-system namespace
	// on the ManagedControlPlane.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// ExtractHelmValues extracts Helm values required for processing from the ProviderConfig.
func ExtractHelmValues(values *apiextensionsv1.JSON) (*HelmValues, error) {
	if values == nil || len(values.Raw) == 0 {
		return &HelmValues{}, nil
	}

	vals := &HelmValues{}
	if err := json.Unmarshal(values.Raw, vals); err != nil {
		return nil, err
	}

	return vals, nil
}

// AddCAToHelmValues removes conflicting volumes, volumeMounts and envVars (matching by name and/or mountPath) and
// adds a volume, volumeMount and envVar on all Flux controller helm values sections to import the custom CA certificate.
// nolint:gocyclo
func AddCAToHelmValues(values *apiextensionsv1.JSON, configMap *corev1.ConfigMapKeySelector) (*apiextensionsv1.JSON, error) {
	if configMap == nil {
		return nil, errors.New("cannot add custom CA to Helm values: ConfigMapKeySelector is nil")
	}

	if configMap.Name == "" {
		return nil, errors.New("cannot add custom CA to Helm values: caBundleRef.Name must be set")
	}

	if configMap.Key == "" {
		return nil, errors.New("cannot add custom CA to Helm values: caBundleRef.Key must be set")
	}

	var root = map[string]json.RawMessage{}

	caVolume := corev1.Volume{
		Name: customCaVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: CustomCABundleConfigMapName,
				},
				Items: []corev1.KeyToPath{
					{
						Key:  configMap.Key,
						Path: configMap.Key,
					},
				},
			},
		},
	}

	caVolumeMount := corev1.VolumeMount{
		Name:      customCaVolumeName,
		ReadOnly:  true,
		MountPath: customCaPath,
	}

	caEnvVar := corev1.EnvVar{
		Name:  "SSL_CERT_DIR",
		Value: strings.Join(append(certDirectories, customCaPath), ":"),
	}

	if values != nil && len(values.Raw) > 0 {
		if err := json.Unmarshal(values.Raw, &root); err != nil {
			return nil, fmt.Errorf("failed to unmarshal helm values: %w", err)
		}
		if root == nil {
			root = make(map[string]json.RawMessage)
		}
	}

	for _, controller := range fluxControllers {
		if err := addPodConfigToController(root, controller, caVolume, caVolumeMount, caEnvVar); err != nil {
			return nil, err
		}
	}

	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal helm values: %w", err)
	}

	return &apiextensionsv1.JSON{Raw: out}, nil
}

func addPodConfigToController(
	root map[string]json.RawMessage,
	controller string,
	caVolume corev1.Volume,
	caVolumeMount corev1.VolumeMount,
	caEnvVar corev1.EnvVar,
) error {
	var controllerValues map[string]json.RawMessage
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount
	var envVars []corev1.EnvVar

	if err := unmarshalIfPresent(root, controller, &controllerValues); err != nil {
		return fmt.Errorf("failed to unmarshal %s: %w", controller, err)
	}

	if controllerValues == nil {
		controllerValues = map[string]json.RawMessage{}
	}

	if err := unmarshalIfPresent(controllerValues, "volumes", &volumes); err != nil {
		return fmt.Errorf("failed to unmarshal %s.volumes: %w", controller, err)
	}

	if err := unmarshalIfPresent(controllerValues, "volumeMounts", &volumeMounts); err != nil {
		return fmt.Errorf("failed to unmarshal %s.volumeMounts: %w", controller, err)
	}

	if err := unmarshalIfPresent(controllerValues, "extraEnv", &envVars); err != nil {
		return fmt.Errorf("failed to unmarshal %s.extraEnv: %w", controller, err)
	}

	volumes = removeConflictingVolumesAndAppend(volumes, caVolume)
	volumeMounts = removeConflictingVolumeMountsAndAppend(volumeMounts, caVolumeMount)
	envVars = removeConflictingEnvVarsAndAppend(envVars, caEnvVar)

	volumesRaw, err := json.Marshal(volumes)
	if err != nil {
		return fmt.Errorf("failed to marshal %s.volumes: %w", controller, err)
	}

	volumeMountsRaw, err := json.Marshal(volumeMounts)
	if err != nil {
		return fmt.Errorf("failed to marshal %s.volumeMounts: %w", controller, err)
	}

	envVarsRaw, err := json.Marshal(envVars)
	if err != nil {
		return fmt.Errorf("failed to marshal %s.extraEnv: %w", controller, err)
	}

	controllerValues["volumes"] = volumesRaw
	controllerValues["volumeMounts"] = volumeMountsRaw
	controllerValues["extraEnv"] = envVarsRaw

	controllerRaw, err := json.Marshal(controllerValues)
	if err != nil {
		return fmt.Errorf("failed to marshal %s: %w", controller, err)
	}

	root[controller] = controllerRaw
	return nil
}

func removeConflictingVolumesAndAppend(volumes []corev1.Volume, caVolume corev1.Volume) []corev1.Volume {
	updated := []corev1.Volume{}
	for _, volume := range volumes {
		if volume.Name != caVolume.Name {
			updated = append(updated, volume)
		}
	}
	updated = append(updated, caVolume)
	return updated
}

func removeConflictingVolumeMountsAndAppend(volumeMounts []corev1.VolumeMount, caVolumeMount corev1.VolumeMount) []corev1.VolumeMount {
	updated := []corev1.VolumeMount{}
	for _, volumeMount := range volumeMounts {
		if volumeMount.MountPath != caVolumeMount.MountPath && volumeMount.Name != caVolumeMount.Name {
			updated = append(updated, volumeMount)
		}
	}
	updated = append(updated, caVolumeMount)
	return updated
}

func removeConflictingEnvVarsAndAppend(envVars []corev1.EnvVar, caEnvVar corev1.EnvVar) []corev1.EnvVar {
	updated := []corev1.EnvVar{}
	for _, envVar := range envVars {
		if envVar.Name != caEnvVar.Name {
			updated = append(updated, envVar)
		}
	}
	updated = append(updated, caEnvVar)
	return updated
}

func unmarshalIfPresent(obj map[string]json.RawMessage, key string, out any) error {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid %s JSON: %w", key, err)
	}
	return nil
}

func setCredentialAnnotation(root map[string]json.RawMessage, name, credentialHash string) error {
	var controller map[string]json.RawMessage
	if err := json.Unmarshal(root[name], &controller); err != nil {
		return err
	}
	annotations := map[string]string{}
	if err := unmarshalIfPresent(controller, "annotations", &annotations); err != nil {
		return fmt.Errorf("invalid %s.annotations: %w", name, err)
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[credentialHashAnnotation] = credentialHash
	controller["annotations"], _ = json.Marshal(annotations)
	root[name], _ = json.Marshal(controller)
	return nil
}

func decodeRemoteValues(values *apiextensionsv1.JSON) (map[string]json.RawMessage, error) {
	root := map[string]json.RawMessage{}
	if values != nil && len(values.Raw) > 0 {
		if err := json.Unmarshal(values.Raw, &root); err != nil {
			return nil, fmt.Errorf("failed to unmarshal helm values: %w", err)
		}
	}
	if root == nil {
		root = map[string]json.RawMessage{}
	}
	return root, nil
}

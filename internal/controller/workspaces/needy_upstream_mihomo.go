/*
Copyright 2026.

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

package workspaces

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/pkg/needy"
)

const (
	// mihomoUser runs the managed upstream; it needs no privileges.
	mihomoUser           = int64(65534)
	mihomoConfigDir      = "/etc/mihomo"
	mihomoStateDir       = "/var/lib/mihomo"
	mihomoContainer      = "mihomo"
	mihomoSocksPortName  = "socks"
	mihomoConfigVolume   = "config"
	mihomoStateVolume    = "state"
	mihomoStateSizeLimit = "16Mi"
)

func needyUpstreamSelector() map[string]string {
	return map[string]string{
		workspacesv1alpha1.NeedyComponentLabel: needyComponentUpstream,
		appNameLabel:                           "mihomo",
		"app.kubernetes.io/instance":           needyUpstreamName,
	}
}

// MihomoUpstream runs a filtering Mihomo SOCKS5 proxy as a namespace's
// managed upstream. It resolves socks5h names itself and refuses the
// cluster's ranges, the special-purpose ranges, and the metadata range by
// the address a name resolves to.
type MihomoUpstream struct {
	client.Client
	Scheme *runtime.Scheme
	Config NeedyConfig
	// Image is the Mihomo image.
	Image string
}

var _ UpstreamProvider = (*MihomoUpstream)(nil)

// Ensure implements UpstreamProvider.
func (r *MihomoUpstream) Ensure(ctx context.Context, namespace string, needed bool) error {
	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: needyUpstreamName}, &existing)
	if apierrors.IsNotFound(err) && !needed {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get needy upstream Deployment: %w", err)
	}
	password, err := r.ensureUpstreamCredentials(ctx, namespace)
	if err != nil {
		return err
	}
	config, err := mihomoConfig(password, r.Config.GatewayBlockCIDRs)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: needyUpstreamConfigSecretName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentUpstream}
		secret.Data = map[string][]byte{needyUpstreamConfigKey: config}

		return nil
	}); err != nil {
		return fmt.Errorf("ensure needy upstream config: %w", err)
	}
	replicas := int32(0)
	if needed {
		replicas = 1
	}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: needyUpstreamName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentUpstream}
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: needyUpstreamSelector()}
		deployment.Spec.Template = needyUpstreamTemplate(r.Image, config)

		return nil
	}); err != nil {
		return fmt.Errorf("ensure needy upstream Deployment: %w", err)
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: needyUpstreamName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Labels = map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentUpstream}
		service.Spec.Selector = needyUpstreamSelector()
		service.Spec.Ports = []corev1.ServicePort{
			{
				Name:       mihomoSocksPortName,
				Port:       needyUpstreamPort,
				TargetPort: intstr.FromString(mihomoSocksPortName),
			},
		}

		return nil
	}); err != nil {
		return fmt.Errorf("ensure needy upstream Service: %w", err)
	}
	// The policy is part of the proxy's filtering, whether or not the manager
	// keeps the other needy policies: Mihomo matches its rules on one
	// resolved address but dials every address of a name.
	desired := needyUpstreamPolicy(r.Config, namespace)
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: desired.Name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Labels = desired.Labels
		policy.Spec = desired.Spec

		return nil
	}); err != nil {
		return fmt.Errorf("ensure needy upstream NetworkPolicy: %w", err)
	}

	return nil
}

// ensureUpstreamCredentials keeps the actor user's password, as the line that
// actor runners read, and returns the password.
func (r *MihomoUpstream) ensureUpstreamCredentials(ctx context.Context, namespace string) (string, error) {
	prefix := needyUpstreamActorUser + ":"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: needyUpstreamCredentialsSecretName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentUpstream}
		if !strings.HasPrefix(string(secret.Data[actorUpstreamCredentialsFile]), prefix) {
			password, err := newNeedyUpstreamPassword()
			if err != nil {
				return err
			}
			secret.Data = map[string][]byte{actorUpstreamCredentialsFile: []byte(prefix + password)}
		}

		return nil
	}); err != nil {
		return "", fmt.Errorf("ensure needy upstream credentials: %w", err)
	}

	return strings.TrimPrefix(string(secret.Data[actorUpstreamCredentialsFile]), prefix), nil
}

// needyUpstreamTemplate runs Mihomo unprivileged with a read-only root. The
// config's hash restarts it when the config changes. Its probes run inside
// the container: a policy on its port may refuse the node's TCP probes.
func needyUpstreamTemplate(image string, config []byte) corev1.PodTemplateSpec {
	check := []string{
		"nc",
		"-z",
		"-w",
		"2",
		"127.0.0.1",
		strconv.Itoa(needyUpstreamPort),
	}
	automount := false
	sizeLimit := resource.MustParse(mihomoStateSizeLimit)

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      needyUpstreamSelector(),
			Annotations: map[string]string{"rc.ayaka.io/config-hash": needy.Hash(string(config))},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &automount,
			EnableServiceLinks:           new(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   new(true),
				RunAsUser:      new(mihomoUser),
				RunAsGroup:     new(mihomoUser),
				FSGroup:        new(mihomoUser),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{
				{
					Name:  mihomoContainer,
					Image: image,
					Args: []string{
						"-d",
						mihomoStateDir,
						"-f",
						mihomoConfigDir + "/" + needyUpstreamConfigKey,
					},
					Ports: []corev1.ContainerPort{
						{
							Name:          mihomoSocksPortName,
							ContainerPort: needyUpstreamPort,
						},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: check}},
						PeriodSeconds: 5,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: check}},
						PeriodSeconds:    10,
						FailureThreshold: 3,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("20m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: new(false),
						ReadOnlyRootFilesystem:   new(true),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{corev1.Capability("ALL")}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      mihomoConfigVolume,
							MountPath: mihomoConfigDir,
							ReadOnly:  true,
						},
						{
							Name:      mihomoStateVolume,
							MountPath: mihomoStateDir,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: mihomoConfigVolume,
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName:  needyUpstreamConfigSecretName,
						DefaultMode: new(int32(0o440)),
					}},
				},
				{
					Name:         mihomoStateVolume,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &sizeLimit}},
				},
			},
		},
	}
}

// needyUpstreamPolicy admits the namespace's actor runners to the upstream
// and keeps the upstream from dialing the ranges it refuses: Mihomo matches
// its rules on one resolved address but dials every address of a name. It
// resolves through public resolvers, which this rule covers on port 53.
func needyUpstreamPolicy(config NeedyConfig, namespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      needyUpstreamName,
			Labels:    map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentUpstream},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: needyUpstreamSelector()},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentActor}}}},
					Ports: []networkingv1.NetworkPolicyPort{tcpOrUDP(corev1.ProtocolTCP, needyUpstreamPort)},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{
						CIDR:   "0.0.0.0/0",
						Except: needyUpstreamBlockedRanges(config.GatewayBlockCIDRs),
					}}},
				},
			},
		},
	}
}

// Endpoint implements UpstreamProvider: the proxy by its ClusterIP, so the
// runner needs no DNS, once it serves.
func (r *MihomoUpstream) Endpoint(ctx context.Context, namespace string) (*workspacesv1alpha1.ActorUpstream, string, error) {
	key := types.NamespacedName{Namespace: namespace, Name: needyUpstreamName}
	var deployment appsv1.Deployment
	err := r.Get(ctx, key, &deployment)
	if apierrors.IsNotFound(err) || (err == nil && deployment.Status.AvailableReplicas == 0) {
		return nil, "Waiting for the namespace's upstream proxy", nil
	}
	if err != nil {
		return nil, "", err
	}
	var service corev1.Service
	if err := r.Get(ctx, key, &service); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "Waiting for the namespace's upstream proxy", nil
		}
		return nil, "", err
	}
	if service.Spec.ClusterIP == "" || service.Spec.ClusterIP == corev1.ClusterIPNone {
		return nil, "The upstream proxy Service has no ClusterIP", nil
	}

	return &workspacesv1alpha1.ActorUpstream{
		URL: fmt.Sprintf("socks5h://%s:%d", service.Spec.ClusterIP, needyUpstreamPort),
		CredentialsSecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: needyUpstreamCredentialsSecretName},
			Key:                  actorUpstreamCredentialsFile,
		},
		PodSelector: &metav1.LabelSelector{MatchLabels: needyUpstreamSelector()},
	}, "", nil
}

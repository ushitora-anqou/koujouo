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

package resources

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	koujouoanqounetv1 "github.com/ushitora-anqou/koujouo/api/v1"
)

const (
	// agentRPCPortNumber is the TCP port the agent serves Cap'n Proto RPC on.
	agentRPCPortNumber = 9550

	// namespaceNameLabelKey is the label the API server puts on every
	// namespace with the namespace name as the value.
	namespaceNameLabelKey = "kubernetes.io/metadata.name"

	// factorioServerKind is the kind that owns the agent objects.
	factorioServerKind = "FactorioServer"
)

// BuildNetworkPolicy returns the desired NetworkPolicy of the agent Pod. The
// agent serves an unauthenticated Cap'n Proto RPC in plain text, so the RPC
// port is admitted from the manager namespace only; the game port stays open
// to every source, because the players connect from anywhere. Egress is left
// unrestricted: the agent downloads the game, uploads the saves to S3 and
// posts to Discord. The NetworkPolicy is owned by server, so the garbage
// collector removes it together with the FactorioServer.
func BuildNetworkPolicy(
	server *koujouoanqounetv1.FactorioServer,
	managerNamespace string,
) *networkingv1.NetworkPolicy {
	labels := map[string]string{FactorioServerLabelKey: server.Name}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      server.Name + "-agent",
			Namespace: server.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(
				server,
				schema.GroupVersionKind{
					Group:   koujouoanqounetv1.GroupVersion.Group,
					Version: koujouoanqounetv1.GroupVersion.Version,
					Kind:    factorioServerKind,
				},
			)},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// The game traffic of the players, from any source.
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: new(corev1.ProtocolUDP),
						Port:     new(intstr.FromInt32(gamePortNumber)),
					}},
				},
				{
					// The RPC of the manager, which runs in the namespace
					// named here and dials the pod IP.
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{namespaceNameLabelKey: managerNamespace},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: new(corev1.ProtocolTCP),
						Port:     new(intstr.FromInt32(agentRPCPortNumber)),
					}},
				},
			},
		},
	}
}

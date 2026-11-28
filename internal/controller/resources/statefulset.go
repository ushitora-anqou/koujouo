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

// Package resources builds the child Kubernetes objects of a FactorioServer.
package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	koujouoanqounetv1 "github.com/ushitora-anqou/koujouo/api/v1"
)

const (
	// FactorioServerLabelKey labels every object that belongs to one
	// FactorioServer, with the resource name as the value.
	FactorioServerLabelKey = "koujouo.anqou.net/factorioserver"

	// TemplateHashAnnotation holds the hash of the pod template fields the
	// controller owns, so it can tell a Pod of an outdated template from a
	// current one. The agent container image is left out of the hash: the
	// agent replaces its own binary in place, so an image change must not
	// recreate the Pod.
	TemplateHashAnnotation = "koujouo.anqou.net/template-hash"

	// AgentContainerName is the name of the agent container.
	AgentContainerName = "agent"

	// SavesVolumeName mounts the existing save data PersistentVolumeClaim.
	SavesVolumeName = "saves"

	// GameVolumeName holds the downloaded game binary as a generic
	// ephemeral volume.
	GameVolumeName = "game"

	// AgentTokenVolumeName mounts the Secret holding the RPC token of the
	// agent.
	AgentTokenVolumeName = "agent-token"

	// SavesMountPath is where the save PersistentVolumeClaim is mounted.
	SavesMountPath = "/var/lib/koujouo"

	// GameMountPath is where the ephemeral game volume is mounted.
	GameMountPath = "/opt/koujouo"

	// AgentTokenMountPath is where the RPC token Secret is mounted, so the
	// token file is <AgentTokenMountPath>/<AgentTokenKey>.
	AgentTokenMountPath = "/var/run/koujouo" //nolint:gosec // A directory, not a credential.

	// AgentTokenKey is the key of the RPC token in its Secret, and the name
	// of the file the agent reads the token from.
	AgentTokenKey = "token"

	// agentTokenSecretSuffix is appended to the FactorioServer name to name
	// the Secret holding the RPC token; see AgentTokenSecretName.
	agentTokenSecretSuffix = "-agent-token"

	// agentTokenFileMode is the mode of the mounted token file. The agent
	// runs as uid 65532 and reads the file as the group of fsGroup 65532.
	agentTokenFileMode int32 = 0o440

	// GamePortName is the name of the Factorio game UDP port.
	GamePortName = "game"

	// AgentRPCPortName is the name of the TCP port the agent serves the
	// Cap'n Proto RPC on. The liveness probe and the NetworkPolicy refer
	// to the port by this name.
	AgentRPCPortName = "rpc"

	// AgentCommand is the PID 1 command of the agent container.
	AgentCommand = "/kojouo/agent"

	// gamePortNumber is the UDP port Factorio serves game traffic on.
	gamePortNumber = 34197

	// agentRPCPortNumber is the TCP port the agent serves Cap'n Proto RPC
	// on.
	agentRPCPortNumber = 9550

	// livenessProbeInitialDelay, livenessProbePeriod and
	// livenessProbeTimeout bound the agent liveness probe. The delay
	// leaves the first RPC calls to the controller, which retries a
	// connection the agent is not ready for.
	livenessProbeInitialDelay = 10
	livenessProbePeriod       = 20
	livenessProbeTimeout      = 5

	// livenessProbeFailureThreshold is how many probes in a row must
	// fail before the kubelet restarts the container. The agent answers
	// the RPC even during a drain, so a restart means a stuck agent.
	livenessProbeFailureThreshold = 3

	// replicaCount is the fixed replica count of the agent StatefulSet.
	replicaCount = 1

	// terminationGracePeriod gives the agent the leave timeout (300s at
	// most) plus the save write time (the agent waits up to 5 minutes)
	// to complete during pod termination, with a margin for the kubelet.
	terminationGracePeriod int64 = 900

	// agentUID is the uid of the agent container user. It matches the
	// nonroot user of the controller image.
	agentUID int64 = 65532

	// defaultEphemeralStorage is the size the controller defaults the
	// game ephemeral volume to when the spec omits it.
	defaultEphemeralStorage = "2Gi"
)

// AgentTokenSecretName returns the name of the Secret that holds the RPC
// token of the agent of server. The controller creates the Secret and the
// StatefulSet builder mounts it, so both name it here.
func AgentTokenSecretName(server *koujouoanqounetv1.FactorioServer) string {
	return server.Name + agentTokenSecretSuffix
}

// BuildStatefulSet returns the desired StatefulSet of server. The agent
// runs as PID 1 with the same image as the controller; the save data is
// mounted from the existing PersistentVolumeClaim and the game binary is
// downloaded by the agent into a generic ephemeral volume. No Service is
// created.
func BuildStatefulSet(
	server *koujouoanqounetv1.FactorioServer,
	controllerImage string,
	scheme *runtime.Scheme,
) (*appsv1.StatefulSet, error) {
	labels := map[string]string{FactorioServerLabelKey: server.Name}

	podSpec := agentPodSpec(server, controllerImage)

	templateHash, err := podTemplateHash(podSpec, labels)
	if err != nil {
		return nil, err
	}

	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      server.Name,
			Namespace: server.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    new(int32(replicaCount)),
			ServiceName: server.Name,
			// The controller upgrades the agent binary in place with
			// execve(2). A rollout would stop the game, so the pod is
			// replaced only when it is deleted.
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{TemplateHashAnnotation: templateHash},
				},
				Spec: podSpec,
			},
		},
	}

	err = controllerutil.SetControllerReference(server, statefulSet, scheme)
	if err != nil {
		return nil, fmt.Errorf("failed to set controller reference on StatefulSet: %w", err)
	}

	return statefulSet, nil
}

// podTemplateHash returns the hash of the pod template fields the
// controller owns: the labels of the template and its spec, with the agent
// container image blanked out. The volumes and the volume mounts are part
// of the spec, so the token Secret volume and its mount change the hash as
// well. The agent replaces its own binary in place, so an image change
// alone must not recreate the Pod; every other owned field reaches the Pod
// only through a new Pod, because the StatefulSet update strategy is
// OnDelete.
func podTemplateHash(podSpec corev1.PodSpec, labels map[string]string) (string, error) {
	hashed := podSpec.DeepCopy()

	for i := range hashed.Containers {
		hashed.Containers[i].Image = ""
	}

	payload, err := json.Marshal(struct {
		Labels map[string]string `json:"labels"`
		Spec   corev1.PodSpec    `json:"spec"`
	}{Labels: labels, Spec: *hashed})
	if err != nil {
		return "", fmt.Errorf("failed to marshal the pod template for its hash: %w", err)
	}

	sum := sha256.Sum256(payload)

	return hex.EncodeToString(sum[:]), nil
}

// agentPodSpec returns the pod spec of the agent pod.
func agentPodSpec(server *koujouoanqounetv1.FactorioServer, controllerImage string) corev1.PodSpec {
	podSpec := corev1.PodSpec{
		// The agent never talks to the API server, so it carries no
		// ServiceAccount token.
		AutomountServiceAccountToken:  new(false),
		TerminationGracePeriodSeconds: new(terminationGracePeriod),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: new(true),
			// fsGroup makes the save volume writable by the agent
			// on storage provisioners that hand out root-owned volumes.
			FSGroup: new(agentUID),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{agentContainer(server, controllerImage)},
		Volumes:    agentVolumes(server),
	}

	if server.Spec.Agent != nil {
		podSpec.ImagePullSecrets = append([]corev1.LocalObjectReference(nil), server.Spec.Agent.ImagePullSecrets...)
	}

	return podSpec
}

// agentContainer returns the agent container of the agent pod.
func agentContainer(server *koujouoanqounetv1.FactorioServer, controllerImage string) corev1.Container {
	container := corev1.Container{
		Name:    AgentContainerName,
		Image:   controllerImage,
		Command: []string{AgentCommand},
		Ports: []corev1.ContainerPort{
			{
				Name:          GamePortName,
				ContainerPort: gamePortNumber,
				Protocol:      corev1.ProtocolUDP,
			},
			{
				Name:          AgentRPCPortName,
				ContainerPort: agentRPCPortNumber,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		// The agent serves its RPC from the moment it listens, which
		// happens before the game starts and stays up during a drain, so
		// a refused connection means a stuck agent.
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromString(AgentRPCPortName),
				},
			},
			InitialDelaySeconds: livenessProbeInitialDelay,
			PeriodSeconds:       livenessProbePeriod,
			TimeoutSeconds:      livenessProbeTimeout,
			FailureThreshold:    livenessProbeFailureThreshold,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: SavesVolumeName, MountPath: SavesMountPath},
			{Name: GameVolumeName, MountPath: GameMountPath},
			{Name: AgentTokenVolumeName, MountPath: AgentTokenMountPath, ReadOnly: true},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			RunAsNonRoot:             new(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}

	if server.Spec.Agent != nil && server.Spec.Agent.Resources != nil {
		container.Resources = *server.Spec.Agent.Resources.DeepCopy()
	}

	return container
}

// agentVolumes returns the volumes of the agent pod.
func agentVolumes(server *koujouoanqounetv1.FactorioServer) []corev1.Volume {
	return []corev1.Volume{
		{
			Name: SavesVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: server.Spec.SavePersistentVolumeClaimName,
				},
			},
		},
		{
			Name: GameVolumeName,
			VolumeSource: corev1.VolumeSource{
				Ephemeral: ephemeralVolumeSource(server.Spec.EphemeralVolume),
			},
		},
		{
			// The token Secret belongs to the controller: unlike the
			// Secrets the spec refers to, it is delivered to the agent as a
			// file and never over the RPC.
			Name: AgentTokenVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  AgentTokenSecretName(server),
					DefaultMode: new(agentTokenFileMode),
				},
			},
		},
	}
}

// ephemeralVolumeSource defaults the generic ephemeral volume source of
// the spec: access modes default to ReadWriteOnce and the storage request
// defaults to 2Gi when unset.
func ephemeralVolumeSource(source *corev1.EphemeralVolumeSource) *corev1.EphemeralVolumeSource {
	template := &corev1.PersistentVolumeClaimTemplate{}
	if source != nil && source.VolumeClaimTemplate != nil {
		template = source.VolumeClaimTemplate.DeepCopy()
	}

	claimSpec := &template.Spec
	if len(claimSpec.AccessModes) == 0 {
		claimSpec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}

	if claimSpec.Resources.Requests.Storage().IsZero() {
		if claimSpec.Resources.Requests == nil {
			claimSpec.Resources.Requests = corev1.ResourceList{}
		}

		claimSpec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(defaultEphemeralStorage)
	}

	return &corev1.EphemeralVolumeSource{VolumeClaimTemplate: template}
}

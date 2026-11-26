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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	koujouoanqounetv1 "koujouo.anqou.net/project/api/v1"
)

const (
	// FactorioServerLabelKey labels every object that belongs to one
	// FactorioServer, with the resource name as the value.
	FactorioServerLabelKey = "koujouo.anqou.net/factorioserver"

	// AgentContainerName is the name of the agent container.
	AgentContainerName = "agent"

	// SavesVolumeName mounts the existing save data PersistentVolumeClaim.
	SavesVolumeName = "saves"

	// GameVolumeName holds the downloaded game binary as a generic
	// ephemeral volume.
	GameVolumeName = "game"

	// SavesMountPath is where the save PersistentVolumeClaim is mounted.
	SavesMountPath = "/var/lib/koujouo"

	// GameMountPath is where the ephemeral game volume is mounted.
	GameMountPath = "/opt/koujouo"

	// GamePortName is the name of the Factorio game UDP port.
	GamePortName = "game"

	// AgentCommand is the PID 1 command of the agent container.
	AgentCommand = "/kojouo/agent"

	// gamePortNumber is the UDP port Factorio serves game traffic on.
	gamePortNumber = 34197

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
			// exec(2). A rollout would stop the game, so the pod is
			// replaced only when it is deleted.
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: agentPodSpec(server, controllerImage),
			},
		},
	}

	err := controllerutil.SetControllerReference(server, statefulSet, scheme)
	if err != nil {
		return nil, fmt.Errorf("failed to set controller reference on StatefulSet: %w", err)
	}

	return statefulSet, nil
}

// agentPodSpec returns the pod spec of the agent pod.
func agentPodSpec(server *koujouoanqounetv1.FactorioServer, controllerImage string) corev1.PodSpec {
	return corev1.PodSpec{
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
}

// agentContainer returns the agent container of the agent pod.
func agentContainer(server *koujouoanqounetv1.FactorioServer, controllerImage string) corev1.Container {
	container := corev1.Container{
		Name:    AgentContainerName,
		Image:   controllerImage,
		Command: []string{AgentCommand},
		Ports: []corev1.ContainerPort{{
			Name:          GamePortName,
			ContainerPort: gamePortNumber,
			Protocol:      corev1.ProtocolUDP,
		}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: SavesVolumeName, MountPath: SavesMountPath},
			{Name: GameVolumeName, MountPath: GameMountPath},
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

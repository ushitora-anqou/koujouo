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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// FactorioServerSpec defines the desired state of FactorioServer.
type FactorioServerSpec struct {
	// gameVersion pins the Factorio release to run, for example "2.1.24".
	// The agent downloads the headless build of this version.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	GameVersion string `json:"gameVersion"`

	// savePersistentVolumeClaimName is the name of the existing
	// PersistentVolumeClaim that stores the save data. It must exist
	// before the FactorioServer is created; the controller never
	// creates or deletes it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SavePersistentVolumeClaimName string `json:"savePersistentVolumeClaimName"`

	// suspend gracefully stops the game (drain, save) and deletes the
	// StatefulSet. The PersistentVolumeClaim is kept. Defaults to false.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// leaveTimeoutSeconds is how long the agent waits for players to
	// leave after warning them before it forces a save and shutdown.
	// Defaults to 60. The maximum leaves room for the save write inside
	// the 900 second termination grace period of the agent Pod.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=300
	// +kubebuilder:default=60
	LeaveTimeoutSeconds *int32 `json:"leaveTimeoutSeconds,omitempty"`

	// ephemeralVolume is the generic ephemeral volume that holds the
	// downloaded game binary. When set, it overrides the default
	// volumeClaimTemplate. Unset fields (storage class, size) are
	// defaulted by the controller (2Gi).
	// +optional
	EphemeralVolume *corev1.EphemeralVolumeSource `json:"ephemeralVolume,omitempty"`

	// agent configures the agent container of the StatefulSet.
	// +optional
	Agent *AgentSpec `json:"agent,omitempty"`

	// gameConfig points to the ConfigMap keys that hold config.ini and
	// the server whitelist. Their contents are sent to the agent over
	// RPC, not mounted as volumes.
	// +optional
	GameConfig *GameConfigSpec `json:"gameConfig,omitempty"`

	// notification configures Discord notifications for player
	// join/leave events.
	// +optional
	Notification *NotificationSpec `json:"notification,omitempty"`

	// backup configures the periodic upload of the save file to an
	// S3-compatible object storage. Old objects are never deleted.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`

	// downloadCredentials references the Secret keys holding the
	// Factorio account username and token, required to download
	// experimental and older builds. Stable builds need no credentials.
	// +optional
	DownloadCredentials *DownloadCredentialsSpec `json:"downloadCredentials,omitempty"`
}

// AgentSpec configures the agent container.
type AgentSpec struct {
	// resources for the agent container (requests/limits).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// GameConfigSpec selects the game configuration files from ConfigMaps.
type GameConfigSpec struct {
	// config selects the ConfigMap key holding config.ini.
	// +kubebuilder:validation:Required
	Config *corev1.ConfigMapKeySelector `json:"config"`

	// serverWhitelist selects the ConfigMap key holding the JSON server
	// whitelist. When set, the game starts with --use-server-whitelist.
	// +optional
	ServerWhitelist *corev1.ConfigMapKeySelector `json:"serverWhitelist,omitempty"`
}

// NotificationSpec configures Discord notifications.
type NotificationSpec struct {
	// discordWebhookURL selects the Secret key holding the Discord
	// webhook URL. Notifications are disabled when unset.
	// +optional
	DiscordWebhookURL *corev1.SecretKeySelector `json:"discordWebhookURL,omitempty"` //nolint:tagliatelle,lll // URL stays uppercase per K8s API conventions.
}

// BackupSpec configures save uploads to an S3-compatible bucket.
type BackupSpec struct {
	// endpointURL is the S3 API endpoint, for example
	// "https://s3.us-west-00.backblazeb2.com". Path-style addressing
	// is always used.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://.+$`
	EndpointURL string `json:"endpointURL"` //nolint:tagliatelle // URL stays uppercase per Kubernetes API conventions.

	// bucket is the destination bucket name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// credentials select the Secret keys holding the S3 access key id
	// and secret access key.
	// +kubebuilder:validation:Required
	Credentials *S3Credentials `json:"credentials"`
}

// S3Credentials references the static S3 credentials.
type S3Credentials struct {
	// accessKeyId selects the Secret key holding the access key id.
	// +kubebuilder:validation:Required
	AccessKeyID *corev1.SecretKeySelector `json:"accessKeyId"`

	// secretAccessKey selects the Secret key holding the secret access key.
	// +kubebuilder:validation:Required
	SecretAccessKey *corev1.SecretKeySelector `json:"secretAccessKey"`
}

// DownloadCredentialsSpec references the Factorio account credentials.
type DownloadCredentialsSpec struct {
	// username selects the Secret key holding the Factorio username.
	// +kubebuilder:validation:Required
	Username *corev1.SecretKeySelector `json:"username"`

	// token selects the Secret key holding the Factorio token.
	// +kubebuilder:validation:Required
	Token *corev1.SecretKeySelector `json:"token"`
}

// FactorioServerStatus defines the observed state of FactorioServer.
type FactorioServerStatus struct {
	// phase is a human-readable summary of the current state.
	// Machine consumers should use conditions instead.
	//
	// +optional
	Phase FactorioServerPhase `json:"phase,omitempty"`

	// currentGameVersion is the game version the agent reports running.
	// +optional
	CurrentGameVersion string `json:"currentGameVersion,omitempty"`

	// onlinePlayers is the number of players currently connected,
	// as reported by the agent.
	// +optional
	OnlinePlayers int32 `json:"onlinePlayers,omitempty"`

	// observedGeneration is the .metadata.generation the status was
	// last reconciled from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the FactorioServer.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FactorioServerPhase describes the operator-level phase.
// +kubebuilder:validation:Enum=Pending;Downloading;Running;Draining;Saving;Suspended;Failed
type FactorioServerPhase string

// Phases of a FactorioServer, from creation through suspension.
const (
	PhasePending     FactorioServerPhase = "Pending"
	PhaseDownloading FactorioServerPhase = "Downloading"
	PhaseRunning     FactorioServerPhase = "Running"
	PhaseDraining    FactorioServerPhase = "Draining"
	PhaseSaving      FactorioServerPhase = "Saving"
	PhaseSuspended   FactorioServerPhase = "Suspended"
	PhaseFailed      FactorioServerPhase = "Failed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fsrv
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.currentGameVersion`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.onlinePlayers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FactorioServer is the Schema for the factorioservers API.
type FactorioServer struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of FactorioServer
	// +required
	Spec FactorioServerSpec `json:"spec"`

	// status defines the observed state of FactorioServer
	// +optional
	Status FactorioServerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FactorioServerList contains a list of FactorioServer.
type FactorioServerList struct {
	metav1.TypeMeta `json:",inline"`

	metav1.ListMeta `json:"metadata,omitzero"`

	Items []FactorioServer `json:"items"`
}

func init() { //nolint:gochecknoinits // Registers the API types with the scheme builder.
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(
			SchemeGroupVersion,
			&FactorioServer{},     //nolint:exhaustruct_v5 // Registration only needs the type.
			&FactorioServerList{}, //nolint:exhaustruct_v5 // Registration only needs the type.
		)

		return nil
	})
}

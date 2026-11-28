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

package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	koujouoanqounetv1 "github.com/ushitora-anqou/koujouo/api/v1"
	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

// Types of the conditions on the FactorioServer status.
const (
	conditionReady       = "Ready"
	conditionProgressing = "Progressing"
	conditionSuspended   = "Suspended"
)

// Reasons of the conditions on the FactorioServer status.
const (
	reasonRunning           = "Running"
	reasonDownloading       = "Downloading"
	reasonDraining          = "Draining"
	reasonSaving            = "Saving"
	reasonSuspended         = "Suspended"
	reasonFailed            = "Failed"
	reasonAgentPending      = "AgentPending"
	reasonAgentUnavailable  = "AgentUnavailable"
	reasonReferencedMissing = "ReferencedResourceMissing"
)

// agentStatus is the state of one agent as reported by its status RPC.
type agentStatus struct {
	phase       rpcapi.Phase
	gameVersion string
	players     uint16
	// agentVersion is the build of the running agent binary, empty for
	// an agent that predates the field.
	agentVersion string
}

// syncAgentStatus reflects the agent-reported state in the status.
// Conditions are the source of truth; the phase is derived from them.
func syncAgentStatus(status *koujouoanqounetv1.FactorioServerStatus, generation int64, reported *agentStatus) {
	reason := reasonForPhase(reported.phase)
	message := fmt.Sprintf("Agent reports phase %s", reported.phase)

	setCondition(status, conditionReady, reported.phase == rpcapi.Phase_running, reason, message, generation)
	setCondition(status, conditionProgressing, isProgressingPhase(reported.phase), reason, message, generation)
	setCondition(status, conditionSuspended, reported.phase == rpcapi.Phase_suspended, reason, message, generation)

	status.CurrentGameVersion = reported.gameVersion
	status.OnlinePlayers = int32(reported.players)
	status.ObservedGeneration = generation
	status.Phase = derivePhase(status.Conditions)
}

// syncUnavailableStatus reflects an agent that cannot be reached yet,
// for example while its pod is starting. The server is not suspended: the
// condition is cleared so that resuming one reports Pending instead of
// keeping the phase of the suspension.
func syncUnavailableStatus(status *koujouoanqounetv1.FactorioServerStatus, generation int64, message string) {
	setCondition(status, conditionReady, false, reasonAgentUnavailable, message, generation)
	setCondition(status, conditionProgressing, true, reasonAgentUnavailable, message, generation)
	setCondition(status, conditionSuspended, false, reasonAgentUnavailable, message, generation)

	status.Phase = derivePhase(status.Conditions)
}

// syncReferenceErrorStatus reflects a spec reference (ConfigMap or Secret)
// that could not be resolved. Like syncUnavailableStatus, it clears the
// Suspended condition of a previous suspension.
func syncReferenceErrorStatus(status *koujouoanqounetv1.FactorioServerStatus, generation int64, message string) {
	setCondition(status, conditionReady, false, reasonReferencedMissing, message, generation)
	setCondition(status, conditionProgressing, false, reasonReferencedMissing, message, generation)
	setCondition(status, conditionSuspended, false, reasonReferencedMissing, message, generation)

	status.Phase = derivePhase(status.Conditions)
}

// syncSuspendedStatus reflects a suspended FactorioServer whose
// StatefulSet has been deleted and whose save PVC is kept.
func syncSuspendedStatus(status *koujouoanqounetv1.FactorioServerStatus, generation int64) {
	message := "Game stopped and StatefulSet deleted; the save PersistentVolumeClaim is kept"

	setCondition(status, conditionReady, false, reasonSuspended, message, generation)
	setCondition(status, conditionProgressing, false, reasonSuspended, message, generation)
	setCondition(status, conditionSuspended, true, reasonSuspended, message, generation)

	status.OnlinePlayers = 0
	status.ObservedGeneration = generation
	status.Phase = derivePhase(status.Conditions)
}

// derivePhase computes the human-readable phase from the conditions,
// which are the single source of truth.
func derivePhase(conditions []metav1.Condition) koujouoanqounetv1.FactorioServerPhase {
	if conditionIsTrue(conditions, conditionSuspended) {
		return koujouoanqounetv1.PhaseSuspended
	}

	if conditionIsTrue(conditions, conditionReady) {
		return koujouoanqounetv1.PhaseRunning
	}

	if conditionIsTrue(conditions, conditionProgressing) {
		progressing := meta.FindStatusCondition(conditions, conditionProgressing)
		if progressing == nil {
			return koujouoanqounetv1.PhasePending
		}

		return progressingPhase(progressing.Reason)
	}

	return koujouoanqounetv1.PhaseFailed
}

// progressingPhase maps a Progressing reason to a phase.
func progressingPhase(reason string) koujouoanqounetv1.FactorioServerPhase {
	switch reason {
	case reasonDownloading:
		return koujouoanqounetv1.PhaseDownloading
	case reasonDraining:
		return koujouoanqounetv1.PhaseDraining
	case reasonSaving:
		return koujouoanqounetv1.PhaseSaving
	case reasonAgentPending, reasonAgentUnavailable:
		return koujouoanqounetv1.PhasePending
	default:
		return koujouoanqounetv1.PhasePending
	}
}

// reasonForPhase returns the condition reason of an agent phase.
func reasonForPhase(phase rpcapi.Phase) string {
	switch phase {
	case rpcapi.Phase_pending:
		return reasonAgentPending
	case rpcapi.Phase_downloading:
		return reasonDownloading
	case rpcapi.Phase_running:
		return reasonRunning
	case rpcapi.Phase_draining:
		return reasonDraining
	case rpcapi.Phase_saving:
		return reasonSaving
	case rpcapi.Phase_suspended:
		return reasonSuspended
	case rpcapi.Phase_failed:
		return reasonFailed
	default:
		return reasonAgentPending
	}
}

// isProgressingPhase reports whether the agent is moving towards
// serving players.
func isProgressingPhase(phase rpcapi.Phase) bool {
	switch phase {
	case rpcapi.Phase_pending, rpcapi.Phase_downloading, rpcapi.Phase_draining, rpcapi.Phase_saving:
		return true
	case rpcapi.Phase_running, rpcapi.Phase_suspended, rpcapi.Phase_failed:
		return false
	default:
		return false
	}
}

// setCondition upserts one status condition with its observed generation.
func setCondition(
	status *koujouoanqounetv1.FactorioServerStatus,
	conditionType string,
	conditionStatus bool,
	reason, message string,
	generation int64,
) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatusOf(conditionStatus),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// conditionStatusOf converts a bool to a condition status value.
func conditionStatusOf(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}

	return metav1.ConditionFalse
}

// conditionIsTrue reports whether the named condition exists and is true.
func conditionIsTrue(conditions []metav1.Condition, conditionType string) bool {
	condition := meta.FindStatusCondition(conditions, conditionType)

	return condition != nil && condition.Status == metav1.ConditionTrue
}

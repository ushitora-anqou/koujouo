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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	koujouoanqounetv1 "github.com/ushitora-anqou/koujouo/api/v1"
)

// testSuspendedGeneration and testResumedGeneration are the spec
// generations of the suspension and of the report that follows it.
const (
	testSuspendedGeneration = 1
	testResumedGeneration   = 2
)

// TestSyncStatusClearsSuspended verifies that the statuses of a server that
// is not suspended any more drop the Suspended condition, so that the phase
// derived from the conditions is no longer Suspended. A resumed server whose
// agent is not reachable yet must read Pending, and a broken spec reference
// must read Failed.
func TestSyncStatusClearsSuspended(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		sync   func(status *koujouoanqounetv1.FactorioServerStatus, generation int64, message string)
		reason string
		want   koujouoanqounetv1.FactorioServerPhase
	}{
		{
			name:   "agent unavailable",
			sync:   syncUnavailableStatus,
			reason: reasonAgentUnavailable,
			want:   koujouoanqounetv1.PhasePending,
		},
		{
			name:   "referenced resource missing",
			sync:   syncReferenceErrorStatus,
			reason: reasonReferencedMissing,
			want:   koujouoanqounetv1.PhaseFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			status := &koujouoanqounetv1.FactorioServerStatus{}

			syncSuspendedStatus(status, testSuspendedGeneration)

			if status.Phase != koujouoanqounetv1.PhaseSuspended {
				t.Fatalf("phase of a suspended server = %q, want %q", status.Phase, koujouoanqounetv1.PhaseSuspended)
			}

			test.sync(status, testResumedGeneration, "message")

			if status.Phase != test.want {
				t.Errorf("phase after the report = %q, want %q", status.Phase, test.want)
			}

			if conditionIsTrue(status.Conditions, conditionSuspended) {
				t.Errorf("the %s condition of %s is still true", conditionSuspended, test.name)
			}

			condition := meta.FindStatusCondition(status.Conditions, conditionSuspended)
			if condition == nil {
				t.Fatalf("the %s condition of %s is missing", conditionSuspended, test.name)
			}

			if condition.Status != metav1.ConditionFalse {
				t.Errorf("status of the %s condition of %s = %q, want %q",
					conditionSuspended, test.name, condition.Status, metav1.ConditionFalse)
			}

			if condition.Reason != test.reason {
				t.Errorf("reason of the %s condition of %s = %q, want %q",
					conditionSuspended, test.name, condition.Reason, test.reason)
			}
		})
	}
}

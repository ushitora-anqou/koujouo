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

package controller_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo DSL imports are the framework convention.
	. "github.com/onsi/gomega"    //nolint:revive // Gomega DSL imports are the framework convention.

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	koujouoanqounetv1 "github.com/ushitora-anqou/koujouo/api/v1"
	"github.com/ushitora-anqou/koujouo/internal/controller"
	"github.com/ushitora-anqou/koujouo/internal/controller/resources"
	"github.com/ushitora-anqou/koujouo/internal/version"
	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

const (
	testNamespace       = "default"
	testControllerImage = "koujouo-test/controller:v0.0.1"
	testPollInterval    = 30 * time.Second
	testRetryInterval   = 5 * time.Second

	// testManagerNamespace is the namespace the test reconciler runs in, as
	// POD_NAMESPACE of the manager Deployment names it in a cluster.
	testManagerNamespace = "koujouo-system"

	// testProgressInterval is short so a test observes a configure in
	// flight without waiting for the production poll interval.
	testProgressInterval = 10 * time.Millisecond

	expectedGracePeriodSeconds = 900
	expectedGameUDPPort        = 34197
	expectedAgentRPCPort       = 9550
	expectedEphemeralStorage   = "2Gi"

	testGameVersion     = "2.0.0"
	testNextGameVersion = "2.1.0"
	configINIKey        = "config.ini"
)

var _ = Describe("FactorioServer Controller", func() {
	var (
		fakeAgent  *fakeAgentServer
		reconciler *controller.FactorioServerReconciler
	)

	BeforeEach(func() {
		fakeAgent = newFakeAgentServer()
		reconciler = newTestReconciler(fakeAgent)
	})

	Context("When the agent Pod is not reachable yet", func() {
		const serverName = "fsrv-wait"

		BeforeEach(func() {
			createTestPVC(ctx, serverName+"-saves")
			createMinimalServer(ctx, serverName, serverName+"-saves")
		})

		AfterEach(func() {
			deleteTestObjects(ctx, serverName)
		})

		It("should create the StatefulSet and requeue while the Pod is missing", func() {
			result, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(testRetryInterval))

			expectAgentStatefulSet(ctx, serverName, serverName+"-saves")

			server := fetchServer(ctx, serverName)
			Expect(server.Status.Phase).To(Equal(koujouoanqounetv1.PhasePending))
			expectCondition(server, "Ready", metav1.ConditionFalse, "AgentUnavailable")
			expectCondition(server, "Progressing", metav1.ConditionTrue, "AgentUnavailable")
		})

		It("should requeue when dialing the agent fails", func() {
			createReadyAgentPod(ctx, serverName)

			reconciler = newTestReconciler(nil)

			result, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(testRetryInterval))

			server := fetchServer(ctx, serverName)
			Expect(server.Status.Phase).To(Equal(koujouoanqounetv1.PhasePending))
			expectCondition(server, "Progressing", metav1.ConditionTrue, "AgentUnavailable")
		})

		It("should not write the StatefulSet again while nothing drifts", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			created := expectAgentStatefulSet(ctx, serverName, serverName+"-saves")

			// The second reconcile finds the StatefulSet as desired, so
			// CreateOrUpdate must leave it untouched: a write would bump the
			// resource version, which is set to a new value on every update.
			_, err = reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			Expect(
				expectAgentStatefulSet(ctx, serverName, serverName+"-saves").ResourceVersion,
			).To(Equal(created.ResourceVersion))
		})
	})

	Context("When the agent is reachable", func() {
		const serverName = "fsrv-run"

		BeforeEach(func() {
			fakeAgent.setPlayers(2)

			createTestPVC(ctx, serverName+"-saves")
			createTestConfigMap(ctx, serverName+"-config")
			createTestSecret(ctx, serverName+"-secrets")
			createFullServer(ctx, serverName)
			createReadyAgentPod(ctx, serverName)
		})

		AfterEach(func() {
			deleteTestObjects(ctx, serverName)
		})

		It("should create a StatefulSet reflecting the spec", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			statefulSet := expectAgentStatefulSet(ctx, serverName, serverName+"-saves")

			container := statefulSet.Spec.Template.Spec.Containers[0]
			Expect(container.Resources.Requests.Cpu().String()).To(Equal("100m"))
			Expect(container.Resources.Requests.Memory().String()).To(Equal("256Mi"))
		})

		It("should keep the game running across controller image upgrades", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			statefulSet := expectAgentStatefulSet(ctx, serverName, serverName+"-saves")

			// The agent binary is replaced in place with execve(2), so the
			// pod must not be rolled when the controller image changes.
			Expect(statefulSet.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))

			// A storage class that hands out root-owned volumes must stay
			// writable by the nonroot agent.
			podSecurityContext := statefulSet.Spec.Template.Spec.SecurityContext
			Expect(podSecurityContext).NotTo(BeNil())
			Expect(podSecurityContext.FSGroup).NotTo(BeNil())
			Expect(*podSecurityContext.FSGroup).To(Equal(int64(65532)))
			Expect(podSecurityContext.RunAsNonRoot).NotTo(BeNil())
			Expect(*podSecurityContext.RunAsNonRoot).To(BeTrue())
		})

		It("should repair the update strategy and the pod security context on an existing StatefulSet", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			// Simulate a StatefulSet written before these fields were owned.
			existing := &appsv1.StatefulSet{}
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: serverName}, existing)
			Expect(err).NotTo(HaveOccurred())

			existing.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			}
			existing.Spec.Template.Spec.SecurityContext = nil
			existing.Spec.Template.Spec.AutomountServiceAccountToken = nil
			existing.Spec.Template.Spec.Containers[0].SecurityContext = nil

			err = k8sClient.Update(ctx, existing)
			Expect(err).NotTo(HaveOccurred())

			_, err = reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			statefulSet := expectAgentStatefulSet(ctx, serverName, serverName+"-saves")
			Expect(statefulSet.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))

			podSecurityContext := statefulSet.Spec.Template.Spec.SecurityContext
			Expect(podSecurityContext).NotTo(BeNil())
			Expect(podSecurityContext.FSGroup).NotTo(BeNil())
			Expect(*podSecurityContext.FSGroup).To(Equal(int64(65532)))

			containerSecurityContext := statefulSet.Spec.Template.Spec.Containers[0].SecurityContext
			Expect(containerSecurityContext).NotTo(BeNil())
			Expect(containerSecurityContext.ReadOnlyRootFilesystem).NotTo(BeNil())
			Expect(*containerSecurityContext.ReadOnlyRootFilesystem).To(BeTrue())

			// The agent never talks to the API server, so the ServiceAccount
			// token must not come back either.
			Expect(*statefulSet.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())
		})

		It("should configure the agent and report its status", func() {
			result, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(testPollInterval))

			configurations := fakeAgent.configurations()
			Expect(configurations).To(HaveLen(1))

			sent := configurations[0]
			Expect(sent.gameVersion).To(Equal(testGameVersion))
			Expect(sent.fileNames).To(ConsistOf("config.ini", "server-whitelist.json"))
			Expect(sent.leaveTimeoutSeconds).To(Equal(int32(90)))
			Expect(sent.discordWebhookURL).To(Equal("https://discord.example.com/webhook/test"))
			Expect(sent.backupEndpointURL).To(Equal("https://s3.us-west-00.backblazeb2.com"))
			Expect(sent.backupBucket).To(Equal("test-bucket"))
			Expect(sent.backupAccessKeyID).To(Equal("test-access-key-id"))
			Expect(sent.backupSecretAccessKey).To(Equal("test-secret-access-key"))
			Expect(sent.downloadUsername).To(Equal("test-username"))
			Expect(sent.downloadToken).To(Equal("test-token"))

			server := fetchServer(ctx, serverName)
			Expect(server.Status.Phase).To(Equal(koujouoanqounetv1.PhaseRunning))
			Expect(server.Status.CurrentGameVersion).To(Equal(testGameVersion))
			Expect(server.Status.OnlinePlayers).To(Equal(int32(2)))
			Expect(server.Status.ObservedGeneration).To(Equal(server.Generation))
			expectCondition(server, "Ready", metav1.ConditionTrue, "Running")
			expectCondition(server, "Progressing", metav1.ConditionFalse, "Running")
			expectCondition(server, "Suspended", metav1.ConditionFalse, "Running")
		})

		It("should not reconfigure an unchanged server", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			result, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(testPollInterval))
			Expect(fakeAgent.configurations()).To(HaveLen(1))
		})

		It("should reconfigure on a game version change", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			updateServerSpec(ctx, serverName, func(spec *koujouoanqounetv1.FactorioServerSpec) {
				spec.GameVersion = testNextGameVersion
			})

			result, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(testPollInterval))

			configurations := fakeAgent.configurations()
			Expect(configurations).To(HaveLen(2))
			Expect(configurations[1].gameVersion).To(Equal(testNextGameVersion))

			server := fetchServer(ctx, serverName)
			Expect(server.Status.CurrentGameVersion).To(Equal(testNextGameVersion))
		})

		It("should reconfigure on a reported version drift", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			fakeAgent.setGameVersion("1.0.0")

			_, err = reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			configurations := fakeAgent.configurations()
			Expect(configurations).To(HaveLen(2))
			Expect(configurations[1].gameVersion).To(Equal(testGameVersion))
		})

		It("should report draining and saving while the agent applies a version change", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			// The fake agent walks through the phases of a real version
			// change. It holds each one until the test has seen it on the
			// FactorioServer status, which proves the controller keeps
			// reporting while its configure call is still in flight.
			proceed := make(chan struct{})

			fakeAgent.setOnConfigure(func(f *fakeAgentServer, _ *recordedConfig) {
				for _, phase := range []rpcapi.Phase{rpcapi.Phase_draining, rpcapi.Phase_saving} {
					f.setPhase(phase)
					<-proceed
				}
			})

			updateServerSpec(ctx, serverName, func(spec *koujouoanqounetv1.FactorioServerSpec) {
				spec.GameVersion = testNextGameVersion
			})

			// The reconcile runs until the walk is over; its result is read
			// once at the end, so the poller must not consume it.
			done := make(chan error, 1)
			reconcileFinished := make(chan struct{})

			go func() {
				defer GinkgoRecover()
				defer close(reconcileFinished)

				_, err := reconcileOnce(ctx, reconciler, serverName)
				done <- err
			}()

			// Poll the status like a kubectl user would and record every
			// phase change in order. A phase that lasts a few polls is
			// reported once; no phase can be skipped over.
			seen := make(chan koujouoanqounetv1.FactorioServerPhase, 16)

			go func() {
				defer GinkgoRecover()
				defer close(seen)

				last := koujouoanqounetv1.FactorioServerPhase("")

				for {
					select {
					case <-reconcileFinished:
						return
					default:
					}

					phase := fetchServerQuietly(ctx, serverName).Status.Phase
					if phase != last {
						last = phase
						seen <- phase
					}

					time.Sleep(testProgressInterval / 2)
				}
			}()

			Eventually(seen).Should(Receive(Equal(koujouoanqounetv1.PhaseDraining)))

			// The configure call is still in flight: the agent holds the
			// draining phase until released below.
			Consistently(reconcileFinished, testProgressInterval*20).ShouldNot(BeClosed())

			proceed <- struct{}{}

			// Draining is reported first; Saving follows it.
			Eventually(seen).Should(Receive(Equal(koujouoanqounetv1.PhaseSaving)))

			proceed <- struct{}{}

			Eventually(done).Should(Receive(BeNil()))

			server := fetchServer(ctx, serverName)
			Expect(server.Status.Phase).To(Equal(koujouoanqounetv1.PhaseRunning))
			Expect(server.Status.CurrentGameVersion).To(Equal(testNextGameVersion))
			expectCondition(server, "Ready", metav1.ConditionTrue, "Running")
		})
	})

	Context("When reconciling the agent NetworkPolicy", func() {
		const serverName = "fsrv-netpol"

		BeforeEach(func() {
			createTestPVC(ctx, serverName+"-saves")
			createMinimalServer(ctx, serverName, serverName+"-saves")
		})

		AfterEach(func() {
			deleteTestObjects(ctx, serverName)
		})

		It("should admit the game port from anywhere and the RPC port from the manager only", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			networkPolicy := expectAgentNetworkPolicy(ctx, serverName)

			Expect(networkPolicy.Spec.PodSelector.MatchLabels).To(
				HaveKeyWithValue(resources.FactorioServerLabelKey, serverName))
			// Egress stays open: the agent downloads the game, uploads the
			// saves and posts to Discord.
			Expect(networkPolicy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))

			ingress := networkPolicy.Spec.Ingress
			Expect(ingress).To(HaveLen(2))

			Expect(ingress[0].From).To(BeEmpty(), "the game port must stay open to every source")
			Expect(ingress[0].Ports).To(HaveLen(1))
			Expect(*ingress[0].Ports[0].Protocol).To(Equal(corev1.ProtocolUDP))
			Expect(ingress[0].Ports[0].Port.IntVal).To(Equal(int32(expectedGameUDPPort)))

			Expect(ingress[1].Ports).To(HaveLen(1))
			Expect(*ingress[1].Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
			Expect(ingress[1].Ports[0].Port.IntVal).To(Equal(int32(expectedAgentRPCPort)))

			Expect(ingress[1].From).To(HaveLen(1))
			namespaceSelector := ingress[1].From[0].NamespaceSelector
			Expect(namespaceSelector).NotTo(BeNil())
			Expect(namespaceSelector.MatchLabels).To(
				HaveKeyWithValue("kubernetes.io/metadata.name", testManagerNamespace))
			Expect(ingress[1].From[0].PodSelector).To(BeNil(), "the RPC port is open to the manager namespace only")

			By("keeping the NetworkPolicy when the server is suspended")

			updateServerSpec(ctx, serverName, func(spec *koujouoanqounetv1.FactorioServerSpec) {
				spec.Suspend = true
			})

			_, err = reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			// The owner reference removes the policy together with the
			// FactorioServer, so the suspension leaves it in place.
			expectAgentNetworkPolicy(ctx, serverName)
		})

		It("should not create a NetworkPolicy without the manager namespace", func() {
			reconciler.ManagerNamespace = ""

			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())

			key := types.NamespacedName{Namespace: testNamespace, Name: serverName + "-agent"}

			lookupErr := k8sClient.Get(ctx, key, &networkingv1.NetworkPolicy{})
			Expect(errors.IsNotFound(lookupErr)).To(BeTrue())
		})
	})

	Context("When suspending the server", func() {
		const serverName = "fsrv-suspend"

		BeforeEach(func() {
			createTestPVC(ctx, serverName+"-saves")
			createTestConfigMap(ctx, serverName+"-config")
			createTestSecret(ctx, serverName+"-secrets")
			createFullServer(ctx, serverName)
			createReadyAgentPod(ctx, serverName)
		})

		AfterEach(func() {
			deleteTestObjects(ctx, serverName)
		})

		It("should stop the agent, delete the StatefulSet and keep the PVC", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeAgent.configurations()).To(HaveLen(1))

			updateServerSpec(ctx, serverName, func(spec *koujouoanqounetv1.FactorioServerSpec) {
				spec.Suspend = true
			})

			result, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			Expect(fakeAgent.stopCount()).To(Equal(1))

			stsKey := types.NamespacedName{Namespace: testNamespace, Name: serverName}
			stsLookupErr := k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{})
			Expect(errors.IsNotFound(stsLookupErr)).To(BeTrue())

			savesKey := types.NamespacedName{Namespace: testNamespace, Name: serverName + "-saves"}
			savesLookupErr := k8sClient.Get(ctx, savesKey, &corev1.PersistentVolumeClaim{})
			Expect(savesLookupErr).NotTo(HaveOccurred())

			server := fetchServer(ctx, serverName)
			Expect(server.Status.Phase).To(Equal(koujouoanqounetv1.PhaseSuspended))
			Expect(server.Status.ObservedGeneration).To(Equal(server.Generation))
			expectCondition(server, "Ready", metav1.ConditionFalse, "Suspended")
			expectCondition(server, "Progressing", metav1.ConditionFalse, "Suspended")
			expectCondition(server, "Suspended", metav1.ConditionTrue, "Suspended")

			By("not recreating the StatefulSet while suspended")

			result, err = reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			stsLookupErr = k8sClient.Get(ctx, stsKey, &appsv1.StatefulSet{})
			Expect(errors.IsNotFound(stsLookupErr)).To(BeTrue())
			Expect(fakeAgent.stopCount()).To(Equal(1))
		})
	})

	Context("When the agent runs another version", func() {
		const (
			serverName           = "fsrv-upgrade"
			outdatedAgentVersion = "0.0.0-outdated"

			// expectedAgentChunkSize is the size of the chunks the
			// controller streams the agent binary in.
			expectedAgentChunkSize = 512 * 1024

			// testAgentBinaryTail is the size of the last chunk of the
			// test agent binary.
			testAgentBinaryTail = 12345
		)

		var (
			dialer *fakeAgentDialer
			binary []byte
		)

		BeforeEach(func() {
			fakeAgent.setAgentVersion(outdatedAgentVersion)

			createTestPVC(ctx, serverName+"-saves")
			createMinimalServer(ctx, serverName, serverName+"-saves")
			createReadyAgentPod(ctx, serverName)

			binary = make([]byte, 3*expectedAgentChunkSize+testAgentBinaryTail)
			for i := range binary {
				binary[i] = byte(i*7 + i/256)
			}

			reconciler.AgentBinaryPath = writeTestAgentBinary(binary)

			dialer = newRPCFakeAgentDialer(fakeAgent)
			reconciler.Dialer = dialer
		})

		AfterEach(func() {
			dialer.close()
			deleteTestObjects(ctx, serverName)
		})

		It("should stream the agent binary and continue with the upgraded agent", func() {
			result, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(testPollInterval))

			upgrades := fakeAgent.agentUpgrades()
			Expect(upgrades).To(HaveLen(1))

			upgrade := upgrades[0]
			Expect(upgrade.binary).To(Equal(binary))
			Expect(upgrade.sizes).To(Equal([]int{
				expectedAgentChunkSize, expectedAgentChunkSize, expectedAgentChunkSize, testAgentBinaryTail,
			}))

			sum := sha256.Sum256(binary)
			Expect(upgrade.sha256).To(Equal(sum[:]))

			By("dialing the upgraded agent again after the exec dropped the connection")

			Expect(dialer.dialCount()).To(Equal(2))
			Expect(fakeAgent.currentAgentVersion()).To(Equal(version.Agent))

			By("continuing with the configuration of the upgraded agent")

			Expect(fakeAgent.configurations()).To(HaveLen(1))

			server := fetchServer(ctx, serverName)
			Expect(server.Status.Phase).To(Equal(koujouoanqounetv1.PhaseRunning))
			expectCondition(server, "Ready", metav1.ConditionTrue, "Running")

			By("not upgrading an agent of the current version again")

			_, err = reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeAgent.agentUpgrades()).To(HaveLen(1))
			Expect(dialer.dialCount()).To(Equal(3))
		})

		It("should fail the reconcile when the agent rejects the binary", func() {
			fakeAgent.dropFirstChunk = true

			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).To(MatchError(ContainSubstring("did not accept the agent binary")))

			Expect(dialer.dialCount()).To(Equal(1))
			Expect(fakeAgent.configurations()).To(BeEmpty())
			Expect(fakeAgent.currentAgentVersion()).To(Equal(outdatedAgentVersion))
		})

		It("should fail the reconcile when the agent still runs another version after the upgrade", func() {
			fakeAgent.keepAgentVersion = true

			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).To(MatchError(ContainSubstring("still reports another version")))

			Expect(dialer.dialCount()).To(Equal(2))
			Expect(fakeAgent.configurations()).To(BeEmpty())
		})

		It("should not upgrade the agent of a terminating Pod", func() {
			markPodTerminating(ctx, serverName)

			result, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(testPollInterval))

			Expect(fakeAgent.agentUpgrades()).To(BeEmpty())
			Expect(dialer.dialCount()).To(Equal(1))
			Expect(fakeAgent.configurations()).To(HaveLen(1))
		})
	})

	Context("When a spec reference cannot be resolved", func() {
		const serverName = "fsrv-badref"

		BeforeEach(func() {
			createTestPVC(ctx, serverName+"-saves")
			createMinimalServer(ctx, serverName, serverName+"-saves")
			createReadyAgentPod(ctx, serverName)

			updateServerSpec(ctx, serverName, func(spec *koujouoanqounetv1.FactorioServerSpec) {
				spec.GameConfig = &koujouoanqounetv1.GameConfigSpec{
					Config: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: serverName + "-missing"},
						Key:                  configINIKey,
					},
					ServerWhitelist: nil,
				}
			})
		})

		AfterEach(func() {
			deleteTestObjects(ctx, serverName)
		})

		It("should report the missing reference and fail the reconcile", func() {
			_, err := reconcileOnce(ctx, reconciler, serverName)
			Expect(err).To(HaveOccurred())

			server := fetchServer(ctx, serverName)
			Expect(server.Status.Phase).To(Equal(koujouoanqounetv1.PhaseFailed))
			expectCondition(server, "Ready", metav1.ConditionFalse, "ReferencedResourceMissing")
			expectCondition(server, "Progressing", metav1.ConditionFalse, "ReferencedResourceMissing")
		})
	})

	Context("When the leave timeout is set", func() {
		const serverName = "fsrv-leave-timeout"

		BeforeEach(func() {
			createTestPVC(ctx, serverName+"-saves")
			createMinimalServer(ctx, serverName, serverName+"-saves")
		})

		AfterEach(func() {
			deleteTestObjects(ctx, serverName)
		})

		It("should reject a value above the maximum", func() {
			// The API server validates the CRD schema, so the write itself
			// has to fail: a drain that long could not finish inside the
			// termination grace period of the agent Pod.
			tooLong := int32(301)
			server := fetchServer(ctx, serverName)
			server.Spec.LeaveTimeoutSeconds = &tooLong

			err := k8sClient.Update(ctx, server)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsInvalid(err)).To(BeTrue(), "expected an invalid value error, got: %v", err)
			Expect(fetchServer(ctx, serverName).Spec.LeaveTimeoutSeconds).NotTo(Equal(&tooLong))
		})

		It("should accept the maximum and default an omitted value", func() {
			atMaximum := int32(300)

			updateServerSpec(ctx, serverName, func(spec *koujouoanqounetv1.FactorioServerSpec) {
				spec.LeaveTimeoutSeconds = &atMaximum
			})

			Expect(fetchServer(ctx, serverName).Spec.LeaveTimeoutSeconds).To(Equal(&atMaximum))

			updateServerSpec(ctx, serverName, func(spec *koujouoanqounetv1.FactorioServerSpec) {
				spec.LeaveTimeoutSeconds = nil
			})

			server := fetchServer(ctx, serverName)
			Expect(server.Spec.LeaveTimeoutSeconds).NotTo(BeNil())
			Expect(*server.Spec.LeaveTimeoutSeconds).To(Equal(int32(60)))
		})
	})
})

// newTestReconciler returns a reconciler wired to the envtest client
// and the fake agent.
func newTestReconciler(agent *fakeAgentServer) *controller.FactorioServerReconciler {
	return &controller.FactorioServerReconciler{
		Client:    k8sClient,
		APIReader: k8sClient,
		Scheme:    k8sClient.Scheme(),
		// testManagerNamespace stands in for the namespace of the manager
		// Deployment; envtest runs the reconciler in the test process.
		ManagerNamespace: testManagerNamespace,
		ControllerImage:  testControllerImage,
		AgentBinaryPath:  "",
		Dialer: &fakeAgentDialer{
			agent: agent,
		},
		Recorder:                  newFakeEventRecorder(),
		PollInterval:              testPollInterval,
		RetryInterval:             testRetryInterval,
		ConfigureProgressInterval: testProgressInterval,
	}
}

// reconcileOnce reconciles the named FactorioServer once.
func reconcileOnce(
	ctx context.Context,
	reconciler *controller.FactorioServerReconciler,
	name string,
) (ctrl.Result, error) {
	result, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
	if err != nil {
		return result, fmt.Errorf("reconcile of %s failed: %w", name, err)
	}

	return result, nil
}

// writeTestAgentBinary writes the stand-in of the controller's agent
// binary to a temporary file and returns its path. The file is removed
// when the test ends.
func writeTestAgentBinary(binary []byte) string {
	file, err := os.CreateTemp("", "koujouo-test-agent-*")
	Expect(err).NotTo(HaveOccurred())

	_, err = file.Write(binary)
	Expect(err).NotTo(HaveOccurred())
	Expect(file.Close()).To(Succeed())

	DeferCleanup(func() { Expect(os.Remove(file.Name())).To(Succeed()) })

	return file.Name()
}

// markPodTerminating gives the agent Pod of server a deletion timestamp.
// A finalizer keeps the Pod from disappearing, since no controller runs
// in envtest to remove it; it is removed when the test ends.
func markPodTerminating(ctx context.Context, serverName string) {
	key := types.NamespacedName{Namespace: testNamespace, Name: serverName + "-0"}

	pod := &corev1.Pod{}
	Expect(k8sClient.Get(ctx, key, pod)).To(Succeed())

	pod.Finalizers = []string{"koujouo.anqou.net/test"}
	Expect(k8sClient.Update(ctx, pod)).To(Succeed())

	Expect(k8sClient.Delete(ctx, pod)).To(Succeed())

	Expect(k8sClient.Get(ctx, key, pod)).To(Succeed())
	Expect(pod.DeletionTimestamp).NotTo(BeNil())

	DeferCleanup(func() {
		kept := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, key, kept)).To(Succeed())

		kept.Finalizers = nil
		Expect(k8sClient.Update(ctx, kept)).To(Succeed())
	})
}

// fetchServer returns the named FactorioServer.
func fetchServer(ctx context.Context, name string) *koujouoanqounetv1.FactorioServer {
	server := &koujouoanqounetv1.FactorioServer{}

	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, server)).To(Succeed())

	return server
}

// updateServerSpec updates the spec of the named FactorioServer.
func updateServerSpec(
	ctx context.Context,
	name string,
	mutate func(*koujouoanqounetv1.FactorioServerSpec),
) {
	server := fetchServer(ctx, name)
	mutate(&server.Spec)
	Expect(k8sClient.Update(ctx, server)).To(Succeed())
}

// createTestPVC creates the save data PersistentVolumeClaim.
func createTestPVC(ctx context.Context, name string) {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
			},
		},
	}

	Expect(k8sClient.Create(ctx, claim)).To(Succeed())
}

// createTestConfigMap creates the game config ConfigMap.
func createTestConfigMap(ctx context.Context, name string) {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Data: map[string]string{
			"config.ini":            "[path]\nwrite-data=/var/lib/koujouo/write-data\n",
			"server-whitelist.json": `["player1"]`,
		},
	}

	Expect(k8sClient.Create(ctx, configMap)).To(Succeed())
}

// createTestSecret creates the referenced Secret.
func createTestSecret(ctx context.Context, name string) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		StringData: map[string]string{
			"discordWebhookURL": "https://discord.example.com/webhook/test",
			"s3AccessKeyId":     "test-access-key-id",
			"s3SecretAccessKey": "test-secret-access-key",
			"factorioUsername":  "test-username",
			"factorioToken":     "test-token",
		},
	}

	Expect(k8sClient.Create(ctx, secret)).To(Succeed())
}

// createMinimalServer creates a FactorioServer with only the required
// spec fields.
func createMinimalServer(ctx context.Context, name, pvcName string) {
	server := &koujouoanqounetv1.FactorioServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Spec:       minimalServerSpec(testGameVersion, pvcName),
	}

	Expect(k8sClient.Create(ctx, server)).To(Succeed())
}

// minimalServerSpec returns a spec with only the required fields.
func minimalServerSpec(gameVersion, pvcName string) koujouoanqounetv1.FactorioServerSpec {
	return koujouoanqounetv1.FactorioServerSpec{
		GameVersion:                   gameVersion,
		SavePersistentVolumeClaimName: pvcName,
		Suspend:                       false,
		LeaveTimeoutSeconds:           nil,
		EphemeralVolume:               nil,
		Agent:                         nil,
		GameConfig:                    nil,
		Notification:                  nil,
		Backup:                        nil,
		DownloadCredentials:           nil,
	}
}

// createFullServer creates a FactorioServer exercising every optional
// spec section. It expects the test ConfigMap and Secret to exist.
func createFullServer(ctx context.Context, name string) {
	server := &koujouoanqounetv1.FactorioServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name},
		Spec:       fullServerSpec(name),
	}

	Expect(k8sClient.Create(ctx, server)).To(Succeed())
}

// fullServerSpec returns a spec exercising every optional section.
func fullServerSpec(name string) koujouoanqounetv1.FactorioServerSpec {
	configName := name + "-config"
	secretName := name + "-secrets"

	return koujouoanqounetv1.FactorioServerSpec{
		GameVersion:                   testGameVersion,
		SavePersistentVolumeClaimName: name + "-saves",
		Suspend:                       false,
		LeaveTimeoutSeconds:           new(int32(90)),
		EphemeralVolume:               nil,
		Agent: &koujouoanqounetv1.AgentSpec{
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		},
		GameConfig: &koujouoanqounetv1.GameConfigSpec{
			Config: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: configName},
				Key:                  configINIKey,
			},
			ServerWhitelist: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: configName},
				Key:                  "server-whitelist.json",
			},
		},
		Notification: &koujouoanqounetv1.NotificationSpec{
			DiscordWebhookURL: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "discordWebhookURL",
			},
		},
		Backup: &koujouoanqounetv1.BackupSpec{
			EndpointURL: "https://s3.us-west-00.backblazeb2.com",
			Bucket:      "test-bucket",
			Credentials: &koujouoanqounetv1.S3Credentials{
				AccessKeyID: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "s3AccessKeyId",
				},
				SecretAccessKey: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "s3SecretAccessKey",
				},
			},
		},
		DownloadCredentials: &koujouoanqounetv1.DownloadCredentialsSpec{
			Username: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "factorioUsername",
			},
			Token: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "factorioToken",
			},
		},
	}
}

// createReadyAgentPod creates the agent Pod of server with a ready
// status, standing in for the StatefulSet controller.
func createReadyAgentPod(ctx context.Context, serverName string) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      serverName + "-0",
			Labels:    map[string]string{resources.FactorioServerLabelKey: serverName},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: testControllerImage}},
		},
	}

	Expect(k8sClient.Create(ctx, pod)).To(Succeed())

	pod.Status.PodIP = "10.42.0.10"
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

// deleteTestObjects removes the objects of one test server. envtest has
// no garbage collector, so children are deleted explicitly; deletes of
// absent objects are ignored.
func deleteTestObjects(ctx context.Context, serverName string) {
	_ = k8sClient.Delete(ctx, fetchServerQuietly(ctx, serverName))
	_ = k8sClient.Delete(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: serverName},
	})
	_ = k8sClient.Delete(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: serverName + "-0"},
	})
	_ = k8sClient.Delete(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: serverName + "-agent"},
	})
	deleteTestPVC(ctx, serverName+"-saves")
	_ = k8sClient.Delete(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: serverName + "-config"},
	})
	_ = k8sClient.Delete(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: serverName + "-secrets"},
	})
}

// deleteTestPVC deletes the test PVC. The API server adds the
// pvc-protection finalizer, and envtest runs no controller to remove
// it, so the finalizer is stripped before deleting.
func deleteTestPVC(ctx context.Context, name string) {
	claim := &corev1.PersistentVolumeClaim{}

	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, claim)
	if err != nil {
		return
	}

	if len(claim.Finalizers) > 0 {
		claim.Finalizers = nil
		Expect(k8sClient.Update(ctx, claim)).To(Succeed())
	}

	_ = k8sClient.Delete(ctx, claim)
}

// fetchServerQuietly returns the named FactorioServer or an empty shell.
func fetchServerQuietly(ctx context.Context, name string) *koujouoanqounetv1.FactorioServer {
	server := &koujouoanqounetv1.FactorioServer{}

	_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, server)

	return server
}

// expectAgentStatefulSet asserts the shape of the agent StatefulSet and
// returns it.
func expectAgentStatefulSet(ctx context.Context, serverName, pvcName string) *appsv1.StatefulSet {
	statefulSet := &appsv1.StatefulSet{}

	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: serverName}, statefulSet)
	Expect(err).NotTo(HaveOccurred())

	Expect(*statefulSet.Spec.Replicas).To(Equal(int32(1)))
	Expect(statefulSet.Spec.ServiceName).To(Equal(serverName))
	Expect(statefulSet.Spec.Selector.MatchLabels).To(HaveKeyWithValue(resources.FactorioServerLabelKey, serverName))
	Expect(statefulSet.Spec.Template.Labels).To(HaveKeyWithValue(resources.FactorioServerLabelKey, serverName))

	podSpec := statefulSet.Spec.Template.Spec
	Expect(*podSpec.TerminationGracePeriodSeconds).To(Equal(int64(expectedGracePeriodSeconds)))
	Expect(podSpec.AutomountServiceAccountToken).NotTo(BeNil())
	Expect(*podSpec.AutomountServiceAccountToken).To(BeFalse())
	Expect(podSpec.Containers).To(HaveLen(1))

	container := podSpec.Containers[0]
	Expect(container.Name).To(Equal(resources.AgentContainerName))
	Expect(container.Image).To(Equal(testControllerImage))
	Expect(container.Command).To(ConsistOf(resources.AgentCommand))
	Expect(container.Ports).To(HaveLen(1))
	Expect(container.Ports[0].Name).To(Equal(resources.GamePortName))
	Expect(container.Ports[0].ContainerPort).To(Equal(int32(expectedGameUDPPort)))
	Expect(container.Ports[0].Protocol).To(Equal(corev1.ProtocolUDP))

	mountPaths := make([]string, 0, len(container.VolumeMounts))
	for _, mount := range container.VolumeMounts {
		mountPaths = append(mountPaths, mount.MountPath)
	}

	Expect(mountPaths).To(ConsistOf(resources.SavesMountPath, resources.GameMountPath))

	savesVolume := volumeByName(podSpec.Volumes, resources.SavesVolumeName)
	Expect(savesVolume.PersistentVolumeClaim).NotTo(BeNil())
	Expect(savesVolume.PersistentVolumeClaim.ClaimName).To(Equal(pvcName))

	gameVolume := volumeByName(podSpec.Volumes, resources.GameVolumeName)
	Expect(gameVolume.Ephemeral).NotTo(BeNil())

	storage := gameVolume.Ephemeral.VolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage]
	Expect(storage.String()).To(Equal(expectedEphemeralStorage))

	owner := metav1.GetControllerOf(statefulSet)
	Expect(owner).NotTo(BeNil())
	Expect(owner.Kind).To(Equal("FactorioServer"))
	Expect(owner.Name).To(Equal(serverName))

	return statefulSet
}

// expectAgentNetworkPolicy asserts the presence and the ownership of the
// agent NetworkPolicy of server and returns it.
func expectAgentNetworkPolicy(ctx context.Context, serverName string) *networkingv1.NetworkPolicy {
	networkPolicy := &networkingv1.NetworkPolicy{}

	key := types.NamespacedName{Namespace: testNamespace, Name: serverName + "-agent"}
	Expect(k8sClient.Get(ctx, key, networkPolicy)).To(Succeed())

	owner := metav1.GetControllerOf(networkPolicy)
	Expect(owner).NotTo(BeNil())
	Expect(owner.Kind).To(Equal("FactorioServer"))
	Expect(owner.Name).To(Equal(serverName))

	return networkPolicy
}

// expectCondition asserts the state and reason of one status condition.
func expectCondition(
	server *koujouoanqounetv1.FactorioServer,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
) {
	condition := meta.FindStatusCondition(server.Status.Conditions, conditionType)
	ExpectWithOffset(1, condition).NotTo(BeNil())
	ExpectWithOffset(1, condition.Status).To(Equal(status))
	ExpectWithOffset(1, condition.Reason).To(Equal(reason))
}

// volumeByName returns the volume with the given name.
func volumeByName(volumes []corev1.Volume, name string) corev1.Volume {
	for _, volume := range volumes {
		if volume.Name == name {
			return volume
		}
	}

	Fail("no volume named " + name)

	return corev1.Volume{}
}

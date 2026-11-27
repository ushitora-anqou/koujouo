//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	koujouoanqounetv1 "github.com/ushitora-anqou/koujouo/api/v1"
	"github.com/ushitora-anqou/koujouo/internal/controller/resources"
	"github.com/ushitora-anqou/koujouo/test/utils"
)

// The FactorioServer scenarios create one FactorioServer in their own
// namespace and follow it through the lifecycle of design.md section 7.3:
// a fresh start, a game version change, a suspension and a resume.
const (
	factorioNamespace = "koujouo-e2e-factorio"
	serverName        = "factorio-e2e"
	claimName         = "factorio-e2e-saves"
	configMapName     = "factorio-e2e-config"
	secretName        = "factorio-e2e-secrets"

	// agentPodName is the Pod of the agent StatefulSet. A StatefulSet
	// recreates it under the same name, so the name also holds for the
	// Pod of a resumed FactorioServer.
	agentPodName = serverName + "-0"

	// agentSelector selects the objects of the agent StatefulSet.
	agentSelector = resources.FactorioServerLabelKey + "=" + serverName

	// configINI is the config.ini the agent receives. The agent rewrites
	// write-data to the save volume; read-data must point at the data
	// directory of the downloaded game.
	configINI = `[path]
read-data=__PATH__executable__/../../data
write-data=/var/lib/koujouo

[other]
autosave-interval=5
autosave-slots=3

[multiplayer-lobby]
name=koujouo-e2e
max-players=0
only-admins-can-pause=true
autosave-only-on-server=true
non-blocking-saving=true
verify-user-identity=true
`

	// serverWhitelist is the server whitelist the agent receives.
	serverWhitelist = `["koujouo-e2e-player"]`

	// inspectScript lists the files of the save volume and prints the
	// size, the modification time and the sha256 of the save the game
	// runs. It never prints file contents: the agent state holds the
	// webhook URL.
	inspectScript = `set -eu
find /mnt -maxdepth 2 -type f | sort
ls -la /mnt
ls -la /mnt/saves
stat -c 'STAT %s %Y' /mnt/saves/map.zip
echo "SHA256 $(sha256sum /mnt/saves/map.zip | cut -d ' ' -f1)"
`

	// The game versions of the scenarios. Both are stable releases, which
	// download without credentials.
	firstGameVersion  = "2.1.24"
	secondGameVersion = "2.1.25"

	// Agent and Factorio log lines the scenarios assert on. The two
	// Factorio lines show that the game bound the game port (UDP 34197)
	// and the RCON interface on the loopback interface.
	logHosting    = "Hosting game at IP ADDR:({0.0.0.0:34197})"
	logRCON       = "Starting RCON interface at IP ADDR:({127.0.0.1:55555})"
	logNoSave     = "No save found, creating one"
	logStarted    = "Started Factorio"
	logNoPlayers  = "No players are online"
	logSaving     = "Saving map as"
	logGoodbye    = "Goodbye"
	logExtracted  = "Extracted the game archive"
	logSignal     = "Received signal, stopping the game gracefully"
	logStopped    = "Stopped agent"
	logCrashed    = "Factorio exited unexpectedly"
	logNotifyFail = "Could not notify Discord"
	logWebhookErr = "webhook returned"

	// The agent logs of an in-place upgrade: the old agent execs the
	// binary the manager streamed, the new one adopts the running game.
	logExecing        = "Execing the new agent binary"
	logResumedUpgrade = "Resumed the game after an agent upgrade"

	// Timeouts of the scenarios. A game start includes the download and
	// the extraction of the headless build, so it gets the longest one;
	// upgradeTimeout bounds the wait for the new manager Deployment and
	// for the agent binary it streams to the running agent.
	pollInterval     = 5 * time.Second
	resourceTimeout  = 5 * time.Minute
	gameStartTimeout = 15 * time.Minute
	logTimeout       = 2 * time.Minute
	exitTimeout      = 5 * time.Minute
	upgradeTimeout   = 10 * time.Minute

	// phaseTraceInterval is the period of the status phase trace.
	phaseTraceInterval = 3 * time.Second
)

// saveFingerprint identifies the content of a save file.
type saveFingerprint struct {
	size   int64
	mtime  int64
	sha256 string
}

var _ = Describe("FactorioServer", Ordered, func() {
	var (
		ctx       context.Context
		k8sClient client.Client
		clientset *kubernetes.Clientset

		// agentLogs follows the logs of the current agent Pod.
		agentLogs *utils.PodLogFollower

		// discordWebhookURL is the webhook of the DISCORD_WEBHOOK_URL
		// environment variable, empty when notifications are not tested.
		discordWebhookURL string

		serverKey = client.ObjectKey{Namespace: factorioNamespace, Name: serverName}
		podKey    = types.NamespacedName{Namespace: factorioNamespace, Name: agentPodName}

		// The save file as it was after the first run and at the
		// suspension.
		firstRun  saveFingerprint
		suspended saveFingerprint

		// logOffset is the length of the agent log when the game version
		// changed.
		logOffset int

		// phaseTrace records the status phases the FactorioServer went
		// through, for the diagnostics.
		phaseTraceMu     sync.Mutex
		phaseTrace       []string
		phaseTraceCancel context.CancelFunc
	)

	// getServer returns the FactorioServer of the scenarios.
	getServer := func(g Gomega) *koujouoanqounetv1.FactorioServer {
		server := &koujouoanqounetv1.FactorioServer{}
		g.ExpectWithOffset(1, k8sClient.Get(ctx, serverKey, server)).To(Succeed())

		return server
	}

	// describeStatus renders the status of server for assertion messages.
	describeStatus := func(server *koujouoanqounetv1.FactorioServer) string {
		conditions := make([]string, 0, len(server.Status.Conditions))

		for _, condition := range server.Status.Conditions {
			conditions = append(conditions, fmt.Sprintf("%s=%s (%s: %s)",
				condition.Type, condition.Status, condition.Reason, condition.Message))
		}

		return fmt.Sprintf("phase=%s currentGameVersion=%q onlinePlayers=%d observedGeneration=%d of %d; conditions: %s",
			server.Status.Phase, server.Status.CurrentGameVersion, server.Status.OnlinePlayers,
			server.Status.ObservedGeneration, server.Generation, strings.Join(conditions, "; "))
	}

	// describePod renders the state of the containers of pod.
	describePod := func(pod *corev1.Pod) string {
		states := make([]string, 0, len(pod.Status.ContainerStatuses))

		for _, status := range pod.Status.ContainerStatuses {
			states = append(states, fmt.Sprintf("%s: ready=%t restarts=%d state=%+v",
				status.Name, status.Ready, status.RestartCount, status.State))
		}

		return fmt.Sprintf("phase=%s podIP=%q conditions=%v containers: %s",
			pod.Status.Phase, pod.Status.PodIP, pod.Status.Conditions, strings.Join(states, "; "))
	}

	// expectRunning waits until the FactorioServer reports the Running
	// phase for gameVersion.
	expectRunning := func(gameVersion string, timeout time.Duration) {
		Eventually(func(g Gomega) {
			server := getServer(g)

			g.Expect(server.Status.Phase).To(Equal(koujouoanqounetv1.PhaseRunning), describeStatus(server))
			g.Expect(server.Status.CurrentGameVersion).To(Equal(gameVersion), describeStatus(server))
			g.Expect(server.Status.ObservedGeneration).To(Equal(server.Generation), describeStatus(server))
			// onlinePlayers is the count the agent polls over RCON; the
			// field is omitted while it is zero.
			g.Expect(server.Status.OnlinePlayers).To(BeNumerically(">=", 0), describeStatus(server))
			g.Expect(server.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", "Ready"),
				HaveField("Status", metav1.ConditionTrue),
			)), describeStatus(server))
		}, timeout, pollInterval).Should(Succeed())
	}

	// expectAgentPodRunning waits until the agent Pod runs.
	expectAgentPodRunning := func(timeout time.Duration) {
		Eventually(func(g Gomega) {
			pod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())

			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), describePod(pod))
			g.Expect(pod.Status.PodIP).NotTo(BeEmpty(), describePod(pod))
		}, timeout, pollInterval).Should(Succeed())
	}

	// followAgentLogs follows the logs of the agent Pod, which must run.
	followAgentLogs := func() {
		var err error

		agentLogs, err = utils.FollowPodLogs(ctx, factorioNamespace, agentPodName)
		Expect(err).NotTo(HaveOccurred(), "Failed to follow the agent Pod logs")
	}

	// expectGameHosted waits until text, the agent log, shows that
	// Factorio hosts the game and opened its RCON interface.
	expectGameHosted := func(text func() string) {
		Eventually(text, logTimeout, pollInterval).Should(SatisfyAll(
			ContainSubstring(logHosting),
			ContainSubstring(logRCON),
		), "the agent log does not show that Factorio hosts the game")
	}

	// expectHealthyAgentLog asserts that the agent log text holds no
	// crash and, with a webhook configured, no failed notification.
	expectHealthyAgentLog := func(text string) {
		Expect(text).NotTo(ContainSubstring(logCrashed), "the agent log reports a crash")
		Expect(text).NotTo(Or(
			ContainSubstring(logNotifyFail),
			ContainSubstring(logWebhookErr),
		), "the agent log reports a failed Discord notification")
	}

	// patchSpec applies mutate to the spec of the FactorioServer.
	patchSpec := func(mutate func(*koujouoanqounetv1.FactorioServerSpec)) {
		fresh := &koujouoanqounetv1.FactorioServer{}
		Expect(k8sClient.Get(ctx, serverKey, fresh)).To(Succeed())

		patch := client.MergeFrom(fresh.DeepCopy())
		mutate(&fresh.Spec)
		Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
	}

	// inspectSave lists the save volume from a short-lived Pod and
	// returns the fingerprint of the save file.
	inspectSave := func(inspector string) saveFingerprint {
		By("inspecting the save volume with a " + inspector + " Pod")

		output, err := utils.RunVolumeInspector(ctx, clientset, factorioNamespace, claimName,
			"inspect-"+inspector, inspectScript)
		Expect(err).NotTo(HaveOccurred(), "Failed to inspect the save volume")

		_, _ = fmt.Fprintf(GinkgoWriter, "save volume (%s):\n%s\n", inspector, output)

		for _, path := range []string{
			"/mnt/agent-state.json", "/mnt/config.ini", "/mnt/server-whitelist.json", "/mnt/saves/map.zip",
		} {
			Expect(output).To(ContainSubstring(path), "the save volume lacks %s", path)
		}

		return parseSaveFingerprint(output)
	}

	// recordPhaseTrace polls the status phase of the FactorioServer and
	// records every change, until the scenarios end.
	startPhaseTrace := func() {
		traceCtx, cancel := context.WithCancel(ctx)
		phaseTraceCancel = cancel

		go func() {
			ticker := time.NewTicker(phaseTraceInterval)
			defer ticker.Stop()

			previous := ""

			for {
				select {
				case <-traceCtx.Done():
					return
				case <-ticker.C:
					server := &koujouoanqounetv1.FactorioServer{}
					if k8sClient.Get(traceCtx, serverKey, server) != nil {
						continue
					}

					current := fmt.Sprintf("%s (%s)", server.Status.Phase, server.Status.CurrentGameVersion)
					if current == previous {
						continue
					}

					previous = current

					phaseTraceMu.Lock()
					phaseTrace = append(phaseTrace, current)
					phaseTraceMu.Unlock()
				}
			}
		}()
	}

	// phaseTraceText returns the recorded status phases in order.
	phaseTraceText := func() string {
		phaseTraceMu.Lock()
		defer phaseTraceMu.Unlock()

		return strings.Join(phaseTrace, " -> ")
	}

	// dump prints the output of one read-only kubectl command.
	dump := func(args ...string) {
		output, err := utils.Kubectl(ctx, args...)
		_, _ = fmt.Fprintf(GinkgoWriter, "$ kubectl %s\n%s\n", strings.Join(args, " "), output)

		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "(the command failed: %v)\n", err)
		}
	}

	// dumpDiagnostics collects the state of the FactorioServer namespace
	// and of the controller, for debugging a failed scenario.
	dumpDiagnostics := func() {
		By("dumping the status phase trace")
		_, _ = fmt.Fprintf(GinkgoWriter, "status phases: %s\n", phaseTraceText())

		By("dumping the diagnostics of the FactorioServer namespace")
		dump("get", "factorioservers", "-n", factorioNamespace, "-o", "yaml")
		dump("describe", "statefulset", serverName, "-n", factorioNamespace)
		dump("get", "pods", "-o", "wide", "-n", factorioNamespace)
		dump("describe", "pods", "-l", agentSelector, "-n", factorioNamespace)
		dump("logs", "-l", agentSelector, "-n", factorioNamespace, "--tail=-1")
		dump("logs", "-l", agentSelector, "-n", factorioNamespace, "--previous", "--tail=-1")
		dump("describe", "persistentvolumeclaims", "-n", factorioNamespace)
		dump("get", "persistentvolumes")
		dump("get", "events", "-n", factorioNamespace, "--sort-by=.lastTimestamp")
		dump("logs", "deployment/"+managerDeploymentName, "-n", namespace)

		if agentLogs != nil {
			By("dumping the followed agent logs")
			_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", agentLogs.Text())
		}
	}

	BeforeAll(func() {
		ctx = context.Background()
		discordWebhookURL = strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL"))

		if discordWebhookURL == "" {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"DISCORD_WEBHOOK_URL is not set: the FactorioServer runs without Discord notifications\n")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"DISCORD_WEBHOOK_URL is set: the FactorioServer runs with Discord notifications\n")
		}

		By("connecting to the cluster")
		config, err := ctrl.GetConfig()
		Expect(err).NotTo(HaveOccurred(), "Failed to load the kubeconfig")

		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(koujouoanqounetv1.AddToScheme(scheme)).To(Succeed())

		k8sClient, err = client.New(config, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred(), "Failed to create the Kubernetes client")

		clientset, err = kubernetes.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the Kubernetes clientset")

		By("creating the FactorioServer namespace with the restricted security policy")
		namespaceObject := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: factorioNamespace,
				Labels: map[string]string{
					"pod-security.kubernetes.io/enforce": "restricted",
				},
			},
		}
		// Tolerate a namespace a failed run left behind, like the manager
		// namespace of the suite.
		err = k8sClient.Get(ctx, client.ObjectKey{Name: factorioNamespace}, &corev1.Namespace{})
		if apierrors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, namespaceObject)).To(Succeed(), "Failed to create the namespace")
		} else {
			Expect(err).NotTo(HaveOccurred(), "Failed to look up the namespace")
			_, _ = fmt.Fprintf(GinkgoWriter, "Namespace %q already exists, reusing it\n", factorioNamespace)
		}

		By("creating the save PersistentVolumeClaim")
		claim := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: factorioNamespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
				},
			},
		}
		Expect(k8sClient.Create(ctx, claim)).To(Succeed(), "Failed to create the save PersistentVolumeClaim")

		By("creating the game configuration ConfigMap")
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: factorioNamespace},
			Data:       map[string]string{"config.ini": configINI, "server-whitelist.json": serverWhitelist},
		}
		Expect(k8sClient.Create(ctx, configMap)).To(Succeed(), "Failed to create the ConfigMap")

		server := newFactorioServer()

		if discordWebhookURL != "" {
			By("creating the Discord webhook Secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: factorioNamespace},
				StringData: map[string]string{"discordWebhookURL": discordWebhookURL},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed(), "Failed to create the webhook Secret")

			server.Spec.Notification = &koujouoanqounetv1.NotificationSpec{
				DiscordWebhookURL: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "discordWebhookURL",
				},
			}
		}

		By("creating the FactorioServer")
		Expect(k8sClient.Create(ctx, server)).To(Succeed(), "Failed to create the FactorioServer")

		startPhaseTrace()
	})

	AfterAll(func() {
		if phaseTraceCancel != nil {
			phaseTraceCancel()
		}

		_, _ = fmt.Fprintf(GinkgoWriter, "status phases: %s\n", phaseTraceText())

		if agentLogs != nil {
			agentLogs.Stop()
		}

		By("deleting the FactorioServer namespace")
		output, err := utils.Kubectl(ctx, "delete", "namespace", factorioNamespace, "--wait=true", "--timeout=5m")
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Failed to delete the namespace: %v: %s\n", err, output)
		}
	})

	// After each scenario, collect the state of the objects and the logs
	// of the pods for debugging on failure.
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics()
		}
	})

	It("downloads the game and hosts the world", func() {
		By("waiting for the agent StatefulSet")
		Eventually(func(g Gomega) {
			statefulSet := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, serverKey, statefulSet)).To(Succeed())

			g.Expect(statefulSet.Spec.Replicas).To(HaveValue(BeEquivalentTo(int32(1))))
			g.Expect(statefulSet.Spec.ServiceName).To(Equal(serverName))
			g.Expect(statefulSet.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))

			podSpec := &statefulSet.Spec.Template.Spec
			g.Expect(podSpec.SecurityContext.FSGroup).To(HaveValue(BeEquivalentTo(int64(65532))))

			agent := podSpec.Containers[0]
			// The release starts with the image of the previous build; the
			// upgrade scenario moves it to managerImage.
			g.Expect(agent.Image).To(Equal(oldManagerImage))
			g.Expect(agent.Command).To(Equal([]string{resources.AgentCommand}))
			g.Expect(agent.VolumeMounts).To(ConsistOf(
				HaveField("Name", resources.SavesVolumeName),
				HaveField("Name", resources.GameVolumeName),
			))
			g.Expect(podSpec.Volumes).To(ContainElement(HaveField(
				"PersistentVolumeClaim.ClaimName", claimName)))
		}, resourceTimeout, pollInterval).Should(Succeed())

		By("waiting for the agent Pod to run")
		expectAgentPodRunning(resourceTimeout)
		followAgentLogs()

		By("waiting for the game to run") // includes the download and the extraction of the headless build
		expectRunning(firstGameVersion, gameStartTimeout)

		By("checking that Factorio hosts the game")
		expectGameHosted(agentLogs.Text)

		Expect(agentLogs.Text()).To(SatisfyAll(
			ContainSubstring(logExtracted),
			// A fresh save volume: the agent creates the save file.
			ContainSubstring(logNoSave),
			ContainSubstring(logStarted),
		))
		expectHealthyAgentLog(agentLogs.Text())
	})

	It("stores the save file and the agent state on the save volume", func() {
		firstRun = inspectSave("first-run")
	})

	// The suite installed the release with oldManagerImage, so the agent
	// of the running game still runs the binary of the previous build.
	// Upgrading the release rolls the manager Deployment only: the
	// StatefulSet is OnDelete, so the agent Pod stays. The new manager
	// streams the agent binary of its image to the agent, which
	// execve(2)s it and adopts the running game.
	It("upgrades the agent without stopping the game", func() {
		var (
			podUID   types.UID
			restarts int32
		)

		By("recording the uid and the restart count of the agent Pod")
		Eventually(func(g Gomega) {
			pod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			// A Pod reports its containers only once the kubelet started
			// them, which the kubelet does after the Pod reached the API
			// server.
			g.Expect(pod.Status.ContainerStatuses).NotTo(BeEmpty(), describePod(pod))

			podUID = pod.UID
			restarts = pod.Status.ContainerStatuses[0].RestartCount
		}, resourceTimeout, pollInterval).Should(Succeed())

		// The scenarios after this one compare the log against logOffset,
		// which stays the offset of the game version change: the upgrade
		// logs are only of interest here.
		upgradeOffset := len(agentLogs.Text())

		By("upgrading the Helm release to the image of this build")
		_, tag, err := splitManagerImage(managerImage)
		Expect(err).NotTo(HaveOccurred(), "Failed to split the manager image")

		cmd := exec.Command("helm", "upgrade", helmReleaseName, chartPath,
			"--namespace", namespace,
			// The repository stays the one of the release: only the tag
			// of the image moves to this build.
			"--reuse-values",
			"--set", "image.tag="+tag,
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to upgrade the Helm release")

		By("waiting for the manager Deployment to roll out the new build")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/"+managerDeploymentName,
			"-n", namespace, "--timeout=5m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to wait for the new manager Deployment")

		By("waiting for the new manager to upgrade the agent in place")
		Eventually(func(g Gomega) {
			logs := agentLogs.Text()[upgradeOffset:]

			g.Expect(logs).To(ContainSubstring(logExecing),
				"the agent did not exec the binary the manager streamed")
			g.Expect(logs).To(ContainSubstring(logResumedUpgrade),
				"the agent did not adopt the running game after the upgrade")
			// The agent replaces its own process inside the container: the
			// log stream of the container ends only when it stops.
			g.Expect(agentLogs.Done()).NotTo(BeClosed(), "the agent container stopped")
		}, upgradeTimeout, pollInterval).Should(Succeed())

		expectHealthyAgentLog(agentLogs.Text()[upgradeOffset:])

		By("checking that the upgrade did not restart the game")
		Expect(agentLogs.Text()[upgradeOffset:]).NotTo(ContainSubstring(logStarted),
			"the upgrade started the game again")
		Expect(agentLogs.Text()[upgradeOffset:]).NotTo(ContainSubstring(logExtracted),
			"the upgrade downloaded the game again")
		Expect(strings.Count(agentLogs.Text(), logStarted)).To(Equal(1),
			"the game started more than once across the upgrade")

		By("checking that the agent Pod kept its identity")
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
		Expect(pod.UID).To(Equal(podUID), "the upgrade replaced the agent Pod")
		Expect(pod.DeletionTimestamp).To(BeNil(), "the agent Pod is being deleted")
		Expect(pod.Status.ContainerStatuses).NotTo(BeEmpty(), describePod(pod))
		Expect(pod.Status.ContainerStatuses[0].RestartCount).To(Equal(restarts),
			"the upgrade restarted the agent container")

		By("checking that the FactorioServer is still running the game")
		expectRunning(firstGameVersion, resourceTimeout)

		By("waiting for the StatefulSet to name the image of the new manager")
		Eventually(func(g Gomega) {
			statefulSet := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, serverKey, statefulSet)).To(Succeed())
			g.Expect(statefulSet.Spec.Template.Spec.Containers[0].Image).To(Equal(managerImage),
				"the StatefulSet still names the image of the previous build")
		}, resourceTimeout, pollInterval).Should(Succeed())
	})

	It("moves the game to another version", func() {
		logOffset = len(agentLogs.Text())

		By("changing spec.gameVersion")
		patchSpec(func(spec *koujouoanqounetv1.FactorioServerSpec) {
			spec.GameVersion = secondGameVersion
		})

		By("waiting for the game of the new version to run")
		expectRunning(secondGameVersion, gameStartTimeout)
		expectGameHosted(func() string { return agentLogs.Text()[logOffset:] })

		By("checking that the agent saved the old version and drained over RCON")
		Expect(agentLogs.Text()[logOffset:]).To(SatisfyAll(
			// The drain asks the game for the online players over RCON.
			ContainSubstring(logNoPlayers),
			ContainSubstring(logSaving),
			ContainSubstring(logGoodbye),
			ContainSubstring(logExtracted),
			ContainSubstring(logStarted),
		))
		Expect(agentLogs.Text()[logOffset:]).NotTo(ContainSubstring(logNoSave),
			"the agent created a new save file for the new version")
		expectHealthyAgentLog(agentLogs.Text()[logOffset:])
	})

	It("suspends the game and keeps the save volume", func() {
		By("setting spec.suspend")
		patchSpec(func(spec *koujouoanqounetv1.FactorioServerSpec) {
			spec.Suspend = true
		})

		By("waiting for the agent to terminate")
		Eventually(agentLogs.Done, exitTimeout, pollInterval).Should(BeClosed(),
			"the agent did not terminate after the suspension")

		By("waiting for the StatefulSet and its Pod to be deleted")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, serverKey, &appsv1.StatefulSet{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the StatefulSet still exists")

			err = k8sClient.Get(ctx, podKey, &corev1.Pod{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the agent Pod still exists")
		}, resourceTimeout, pollInterval).Should(Succeed())

		By("checking that the status reports the suspension")
		Eventually(func(g Gomega) {
			server := getServer(g)

			g.Expect(server.Status.Phase).To(Equal(koujouoanqounetv1.PhaseSuspended), describeStatus(server))
			g.Expect(server.Status.ObservedGeneration).To(Equal(server.Generation), describeStatus(server))
			g.Expect(server.Status.OnlinePlayers).To(BeZero(), describeStatus(server))
			g.Expect(server.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", "Suspended"),
				HaveField("Status", metav1.ConditionTrue),
			)), describeStatus(server))
		}, resourceTimeout, pollInterval).Should(Succeed())

		By("checking that the agent saved the game before it stopped")
		Expect(agentLogs.Text()).To(SatisfyAll(
			ContainSubstring(logNoPlayers),
			ContainSubstring(logSaving),
			ContainSubstring(logGoodbye),
			ContainSubstring(logSignal),
			ContainSubstring(logStopped),
		))
		expectHealthyAgentLog(agentLogs.Text())

		By("checking that the save PersistentVolumeClaim is kept")
		claim := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: factorioNamespace, Name: claimName,
		}, claim)).To(Succeed())
		Expect(claim.Status.Phase).To(Equal(corev1.ClaimBound), "phase %s", claim.Status.Phase)
		Expect(claim.DeletionTimestamp).To(BeNil())

		suspended = inspectSave("suspended")

		By("checking that the graceful stop rewrote the save file")
		Expect(suspended.mtime).To(BeNumerically(">", firstRun.mtime),
			"save file: first run %+v, suspended %+v", firstRun, suspended)
	})

	It("resumes the game from the existing save", func() {
		By("clearing spec.suspend")
		patchSpec(func(spec *koujouoanqounetv1.FactorioServerSpec) {
			spec.Suspend = false
		})

		By("waiting for the agent Pod to run")
		expectAgentPodRunning(resourceTimeout)
		followAgentLogs()

		By("waiting for the game to run") // includes the download of the headless build again
		expectRunning(secondGameVersion, gameStartTimeout)
		expectGameHosted(agentLogs.Text)

		By("checking that the agent reused the save file")
		Expect(agentLogs.Text()).To(SatisfyAll(
			ContainSubstring(logExtracted),
			ContainSubstring(logStarted),
		))
		Expect(agentLogs.Text()).NotTo(ContainSubstring(logNoSave),
			"the agent created a new save file instead of resuming the existing one")
		expectHealthyAgentLog(agentLogs.Text())

		resumed := inspectSave("resumed")

		Expect(resumed).To(Equal(suspended),
			"save file: suspended %+v, resumed %+v", suspended, resumed)
	})
})

// newFactorioServer returns the FactorioServer of the scenarios, without
// the optional Discord notification.
func newFactorioServer() *koujouoanqounetv1.FactorioServer {
	return &koujouoanqounetv1.FactorioServer{
		ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: factorioNamespace},
		Spec: koujouoanqounetv1.FactorioServerSpec{
			GameVersion:                   firstGameVersion,
			SavePersistentVolumeClaimName: claimName,
			GameConfig: &koujouoanqounetv1.GameConfigSpec{
				Config: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
					Key:                  "config.ini",
				},
				ServerWhitelist: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
					Key:                  "server-whitelist.json",
				},
			},
		},
	}
}

// parseSaveFingerprint extracts the fingerprint of the save file from the
// output of inspectScript.
func parseSaveFingerprint(output string) saveFingerprint {
	fingerprint := saveFingerprint{}

	for line := range strings.SplitSeq(output, "\n") {
		switch {
		case strings.HasPrefix(line, "STAT "):
			_, err := fmt.Sscanf(line, "STAT %d %d", &fingerprint.size, &fingerprint.mtime)
			Expect(err).NotTo(HaveOccurred(), "Failed to parse the stat line %q", line)
		case strings.HasPrefix(line, "SHA256 "):
			fingerprint.sha256 = strings.TrimPrefix(line, "SHA256 ")
		}
	}

	Expect(fingerprint.sha256).NotTo(BeEmpty(), "the inspector output holds no sha256:\n%s", output)

	return fingerprint
}

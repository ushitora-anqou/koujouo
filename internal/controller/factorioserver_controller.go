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
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	koujouoanqounetv1 "github.com/ushitora-anqou/koujouo/api/v1"
	"github.com/ushitora-anqou/koujouo/internal/controller/resources"
	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

var errControllerImageUnset = errors.New("controllerImage of the manager configuration is not set")

const (
	// defaultPollInterval is how often the agent status is polled.
	defaultPollInterval = 30 * time.Second

	// defaultRetryInterval is the requeue delay while the agent pod is
	// not reachable yet.
	defaultRetryInterval = 5 * time.Second

	// eventAction is the action recorded on FactorioServer events.
	eventAction = "Reconcile"

	// defaultMaxConcurrentReconciles is how many FactorioServers are
	// reconciled at the same time.
	defaultMaxConcurrentReconciles = 8
)

// FactorioServerReconciler reconciles a FactorioServer object.
type FactorioServerReconciler struct {
	client.Client

	// APIReader reads with the API server directly instead of through the
	// cache. It reads the Secrets and ConfigMaps the spec refers to, which
	// are few and need no cluster-wide watch. A nil APIReader falls back to
	// Client.
	APIReader client.Reader

	Scheme *runtime.Scheme

	// ManagerNamespace is the namespace the manager runs in. It is the only
	// source the NetworkPolicy of an agent Pod admits the agent RPC port
	// from. Empty skips the NetworkPolicy, which leaves the RPC reachable
	// from the whole Pod network.
	ManagerNamespace string

	// ControllerImage is the container image deployed for the agent
	// containers. It is the controllerImage key of the manager
	// configuration, so it normally holds the image of the manager itself.
	ControllerImage string

	// AgentBinaryPath is the agent binary of the controller image that
	// agents of another version receive. Defaults to /kojouo/agent.
	AgentBinaryPath string

	// Dialer connects to the agent pods. Defaults to &TCPAgentDialer{}.
	Dialer AgentDialer

	// Recorder emits events about FactorioServers. Defaults to the
	// recorder of the manager.
	Recorder events.EventRecorder

	// PollInterval is the period between agent status polls.
	// Defaults to 30s.
	PollInterval time.Duration

	// RetryInterval is the requeue delay while the agent is not
	// reachable yet. Defaults to 5s.
	RetryInterval time.Duration

	// ConfigureProgressInterval is how often the agent phase is polled
	// while a configure call is in flight. Defaults to 2s.
	ConfigureProgressInterval time.Duration

	// managerNamespaceWarned records that the missing ManagerNamespace was
	// already reported, so the warning is logged once instead of once per
	// reconcile.
	managerNamespaceWarned atomic.Bool
}

// +kubebuilder:rbac:groups=koujouo.anqou.net,resources=factorioservers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=koujouo.anqou.net,resources=factorioservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=koujouo.anqou.net,resources=factorioservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile moves the cluster state closer to the state specified by the
// FactorioServer object: it ensures the agent StatefulSet, sends the
// desired configuration to the agent over Cap'n Proto RPC and reflects
// the agent-reported state on the status.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/reconcile
func (r *FactorioServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("factorioserver", req.String())

	server := &koujouoanqounetv1.FactorioServer{} //nolint:exhaustruct_v5 // Populated by the API server.

	err := r.Get(ctx, req.NamespacedName, server)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil //nolint:exhaustruct_v5 // The server is gone; nothing to do.
		}

		return ctrl.Result{}, fmt.Errorf("could not get FactorioServer: %w", err)
	}

	if server.Spec.Suspend {
		return r.reconcileSuspended(ctx, log, server)
	}

	return r.reconcileActive(ctx, log, server)
}

// SetupWithManager sets up the controller with the Manager.
func (r *FactorioServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Dialer == nil {
		r.Dialer = &TCPAgentDialer{}
	}

	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("factorioserver")
	}

	err := ctrl.NewControllerManagedBy(mgr).
		// A configure call takes minutes: it drains the players and
		// downloads the game, and the reconcile waits for the agent to
		// return. A single worker would hold back the reconciles of every
		// other FactorioServer for that long. The workqueue never runs the
		// same key concurrently, so one FactorioServer stays serial.
		WithOptions(controller.Options{MaxConcurrentReconciles: defaultMaxConcurrentReconciles}).
		// The controller writes the status itself, so a status-only update
		// must not enqueue the next reconcile: the agent is polled through
		// the requeue interval and through the Pod watch.
		For(
			&koujouoanqounetv1.FactorioServer{}, //nolint:exhaustruct_v5 // The builder only needs the type.
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// The rollout of the agent Pod shows in the StatefulSet status,
		// which the Pod watch already covers; only a spec change is
		// reconciled here.
		Owns(&appsv1.StatefulSet{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToFactorioServer),
			builder.WithPredicates(predicate.NewPredicateFuncs(hasFactorioServerLabel)),
		).
		Named("factorioserver").
		Complete(r)
	if err != nil {
		return fmt.Errorf("failed to build controller for FactorioServer: %w", err)
	}

	return nil
}

// podToFactorioServer maps an agent Pod to its FactorioServer request.
func (r *FactorioServerReconciler) podToFactorioServer(
	_ context.Context,
	pod client.Object,
) []reconcile.Request {
	name, ok := pod.GetLabels()[resources.FactorioServerLabelKey]
	if !ok || name == "" {
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: pod.GetNamespace(), Name: name},
	}}
}

// hasFactorioServerLabel reports whether the object carries the
// FactorioServer label.
func hasFactorioServerLabel(object client.Object) bool {
	_, ok := object.GetLabels()[resources.FactorioServerLabelKey]

	return ok
}

// reconcileActive reconciles a FactorioServer that should be running: it
// ensures the StatefulSet, configures the agent and polls its status.
func (r *FactorioServerReconciler) reconcileActive(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
) (ctrl.Result, error) {
	templateHash, err := r.ensureAgentObjects(ctx, log, server)
	if err != nil {
		return ctrl.Result{}, err
	}

	pod, found, err := r.findAgentPod(ctx, server)
	if err != nil {
		return ctrl.Result{}, err
	}

	result, waiting, err := r.waitForAgentPod(ctx, log, server, pod, found, templateHash)
	if waiting {
		return result, err
	}

	agent, err := r.Dialer.Dial(ctx, pod)
	if err != nil {
		log.Error(err, "Could not dial agent", "pod", pod.Name)

		return r.requeueWaitingForAgent(ctx, server, fmt.Sprintf("Waiting for the agent RPC endpoint: %v", err))
	}
	defer agent.Release()

	reported, err := r.callAgentStatus(ctx, agent.Agent)
	if err != nil {
		return ctrl.Result{}, err
	}

	if needsAgentUpgrade(pod, reported) {
		// The deferred release above keeps covering the outdated agent.
		upgraded, status, upgradeErr := r.upgradeAgent(ctx, log, server, pod, agent.Agent, reported.agentVersion)
		if upgradeErr != nil {
			return ctrl.Result{}, upgradeErr
		}

		defer upgraded.Release()

		agent, reported = upgraded, status
	}

	reported, err = r.configureIfNeeded(ctx, log, server, agent.Agent, reported)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = r.patchStatus(ctx, server, func(status *koujouoanqounetv1.FactorioServerStatus) {
		syncAgentStatus(status, server.Generation, reported)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.pollInterval()}, nil //nolint:exhaustruct_v5 // Only the poll interval is set.
}

// configureIfNeeded sends the full configuration when the spec changed
// and returns the agent state to report.
func (r *FactorioServerReconciler) configureIfNeeded(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
	agent rpcapi.Agent,
	reported *agentStatus,
) (*agentStatus, error) {
	if !r.needsConfigure(server, reported) {
		return reported, nil
	}

	config, err := r.buildAgentConfig(ctx, server)
	if err != nil {
		message := fmt.Sprintf("Could not resolve the spec references: %v", err)

		patchErr := r.patchStatus(ctx, server, func(status *koujouoanqounetv1.FactorioServerStatus) {
			syncReferenceErrorStatus(status, server.Generation, message)
		})
		if patchErr != nil {
			return nil, errors.Join(err, patchErr)
		}

		return nil, fmt.Errorf("could not build the agent config: %w", err)
	}

	err = r.callAgentConfigure(ctx, agent, config, r.progressReporter(ctx, log, server))
	if err != nil {
		return nil, err
	}

	log.Info("Configured agent", "gameVersion", server.Spec.GameVersion)
	r.recordEvent(server, "AgentConfigured", "Sent configuration for game version "+server.Spec.GameVersion)

	return r.callAgentStatus(ctx, agent)
}

// requeueWaitingForAgent records the unavailable status and returns the
// short requeue used while the agent is not reachable yet.
func (r *FactorioServerReconciler) requeueWaitingForAgent(
	ctx context.Context,
	server *koujouoanqounetv1.FactorioServer,
	message string,
) (ctrl.Result, error) {
	err := r.patchStatus(ctx, server, func(status *koujouoanqounetv1.FactorioServerStatus) {
		syncUnavailableStatus(status, server.Generation, message)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.retryInterval()}, nil //nolint:exhaustruct_v5 // Only the retry interval is set.
}

// reconcileSuspended reconciles a suspended FactorioServer: it asks the
// agent to stop the game gracefully, deletes the StatefulSet and keeps
// the save PersistentVolumeClaim untouched.
func (r *FactorioServerReconciler) reconcileSuspended(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
) (ctrl.Result, error) {
	statefulSet := &appsv1.StatefulSet{}

	err := r.Get(ctx, client.ObjectKeyFromObject(server), statefulSet)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Already suspended; never recreate the StatefulSet here.
			err = r.patchStatus(ctx, server, func(status *koujouoanqounetv1.FactorioServerStatus) {
				syncSuspendedStatus(status, server.Generation)
			})

			return ctrl.Result{}, err
		}

		return ctrl.Result{}, fmt.Errorf("could not get StatefulSet: %w", err)
	}

	pod, found, err := r.findAgentPod(ctx, server)
	if err != nil {
		return ctrl.Result{}, err
	}

	if found {
		err = r.stopAgentOfPod(ctx, log, server, pod)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	err = r.Delete(ctx, statefulSet)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("could not delete StatefulSet: %w", err)
	}

	log.Info("Deleted StatefulSet", "name", server.Name)
	r.recordEvent(server, "StatefulSetDeleted", "Deleted StatefulSet "+server.Name+" for suspension")

	err = r.patchStatus(ctx, server, func(status *koujouoanqounetv1.FactorioServerStatus) {
		syncSuspendedStatus(status, server.Generation)
	})

	return ctrl.Result{}, err
}

// stopAgentOfPod asks the agent of pod to stop the game gracefully.
// When the pod is not reachable, the stop is skipped: the pod
// termination then signals the agent, which saves on SIGTERM.
func (r *FactorioServerReconciler) stopAgentOfPod(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
	pod *corev1.Pod,
) error {
	if pod.Status.PodIP == "" || !podIsReady(pod) {
		log.Info("Skipping graceful agent stop", "reason", "agent Pod is not ready")

		return nil
	}

	agent, err := r.Dialer.Dial(ctx, pod)
	if err != nil {
		return fmt.Errorf("could not dial agent to stop the game: %w", err)
	}
	defer agent.Release()

	err = r.callAgentStop(ctx, agent.Agent, r.progressReporter(ctx, log, server))
	if err != nil {
		return err
	}

	log.Info("Stopped agent", "name", server.Name)
	r.recordEvent(server, "AgentStopped", "Agent stopped the game and saved the world")

	return nil
}

// progressReporter returns the callback that mirrors each phase the
// agent goes through while a configure or stop call is in flight: a
// drain waits for the players to leave and a download fetches tens of
// megabytes, and both must stay visible on the FactorioServer status.
// The patch is not bound to the reconcile deadline for the same reason
// as the call it reports on.
func (r *FactorioServerReconciler) progressReporter(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
) func(reported *agentStatus) {
	return func(reported *agentStatus) {
		err := r.patchStatus(context.WithoutCancel(ctx), server, func(status *koujouoanqounetv1.FactorioServerStatus) {
			syncAgentStatus(status, server.Generation, reported)
		})
		if err != nil {
			log.Error(err, "Could not report the agent progress")
		}
	}
}

// needsConfigure reports whether the agent must receive a new full
// configuration: on a game version drift or whenever the spec generation
// changed since the last reconcile.
func (r *FactorioServerReconciler) needsConfigure(
	server *koujouoanqounetv1.FactorioServer,
	reported *agentStatus,
) bool {
	return reported.gameVersion != server.Spec.GameVersion ||
		server.Status.ObservedGeneration != server.Generation
}

// agentPodUnavailable reports whether the agent pod cannot be dialed
// yet because it is missing, has no IP or is not ready.
func agentPodUnavailable(pod *corev1.Pod, found bool) bool {
	return !found || pod.Status.PodIP == "" || !podIsReady(pod)
}

// podTemplateOutdated reports whether the agent pod runs a pod template
// other than the desired one. A pod that is already being deleted is not
// outdated again: its replacement is on the way.
func podTemplateOutdated(pod *corev1.Pod, found bool, templateHash string) bool {
	if !found || pod.DeletionTimestamp != nil {
		return false
	}

	return pod.Annotations[resources.TemplateHashAnnotation] != templateHash
}

// waitForAgentPod reports whether the reconcile has to wait for the agent
// Pod instead of dialing it: a Pod of an outdated pod template is deleted
// so that the StatefulSet replaces it, and a missing or unready Pod is
// waited for. The result and error are the ones of that requeue.
func (r *FactorioServerReconciler) waitForAgentPod(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
	pod *corev1.Pod,
	found bool,
	templateHash string,
) (ctrl.Result, bool, error) {
	if podTemplateOutdated(pod, found, templateHash) {
		result, err := r.recreateAgentPod(ctx, log, server, pod)

		return result, true, err
	}

	if agentPodUnavailable(pod, found) {
		result, err := r.requeueWaitingForAgent(ctx, server, "Waiting for the agent Pod to become ready")

		return result, true, err
	}

	return ctrl.Result{}, false, nil //nolint:exhaustruct_v5 // The Pod is dialable; no requeue.
}

// recreateAgentPod deletes the agent pod so that the StatefulSet replaces
// it with one of the current pod template, and returns the requeue that
// waits for the replacement. The kubelet sends the agent a SIGTERM, and the
// agent stops the game gracefully and saves the world before it exits.
func (r *FactorioServerReconciler) recreateAgentPod(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
	pod *corev1.Pod,
) (ctrl.Result, error) {
	err := r.Delete(ctx, pod)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("could not delete the agent Pod: %w", err)
	}

	log.Info("Deleted the agent Pod for the changed pod template", "pod", pod.Name)
	r.recordEvent(server, "AgentPodRecreated", "Deleted the agent Pod "+pod.Name+" for the changed pod template")

	return r.requeueWaitingForAgent(ctx, server, "Recreating the agent Pod for the changed pod template")
}

// ensureAgentObjects creates the StatefulSet and the NetworkPolicy of
// server, synchronizes both and returns the hash of the desired pod
// template.
func (r *FactorioServerReconciler) ensureAgentObjects(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
) (string, error) {
	templateHash, err := r.ensureStatefulSet(ctx, log, server)
	if err != nil {
		return "", err
	}

	err = r.ensureNetworkPolicy(ctx, log, server)
	if err != nil {
		return "", err
	}

	return templateHash, nil
}

// ensureStatefulSet creates the StatefulSet of server when it is missing
// and synchronizes the fields the controller owns on an existing one. It
// returns the hash of the desired pod template, which tells the agent Pod
// of an outdated template apart from a current one.
func (r *FactorioServerReconciler) ensureStatefulSet(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
) (string, error) {
	if r.ControllerImage == "" {
		return "", fmt.Errorf("could not build the agent StatefulSet: %w", errControllerImageUnset)
	}

	desired, err := resources.BuildStatefulSet(server, r.ControllerImage, r.Scheme)
	if err != nil {
		return "", fmt.Errorf("could not build the desired StatefulSet: %w", err)
	}

	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, statefulSet, func() error {
		// CreateOrUpdate hands over the empty shell above when the
		// StatefulSet is missing and the object it fetched when it exists,
		// so the ownership is (re)applied on both paths.
		ownerErr := controllerutil.SetControllerReference(server, statefulSet, r.Scheme)
		if ownerErr != nil {
			return fmt.Errorf("could not set the controller reference: %w", ownerErr)
		}

		applyStatefulSet(statefulSet, desired)

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("could not ensure the StatefulSet: %w", err)
	}

	if result == controllerutil.OperationResultCreated {
		log.Info("Created StatefulSet", "name", server.Name)
		r.recordEvent(server, "StatefulSetCreated", "Created StatefulSet "+server.Name)
	}

	if result == controllerutil.OperationResultUpdated {
		log.Info("Updated StatefulSet", "name", server.Name)
	}

	return desired.Spec.Template.Annotations[resources.TemplateHashAnnotation], nil
}

// ensureNetworkPolicy creates the NetworkPolicy of the agent Pod of server
// and synchronizes it. The agent RPC is unauthenticated and plain text, so
// the policy admits it from the manager namespace only. Without a manager
// namespace the policy would admit no source at all, which would cut the
// manager off from the agent; the controller then leaves the policy out and
// warns once.
func (r *FactorioServerReconciler) ensureNetworkPolicy(
	ctx context.Context,
	log logr.Logger,
	server *koujouoanqounetv1.FactorioServer,
) error {
	if r.ManagerNamespace == "" {
		r.warnManagerNamespaceUnset(log)

		return nil
	}

	desired := resources.BuildNetworkPolicy(server, r.ManagerNamespace)

	networkPolicy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, networkPolicy, func() error {
		// CreateOrUpdate hands over the empty shell above when the policy is
		// missing and the object it fetched when it exists. Both get the
		// owner reference of the desired policy, so the garbage collector
		// removes it together with the FactorioServer.
		networkPolicy.Labels = desired.Labels
		networkPolicy.OwnerReferences = desired.OwnerReferences
		networkPolicy.Spec = desired.Spec

		return nil
	})
	if err != nil {
		return fmt.Errorf("could not ensure the NetworkPolicy: %w", err)
	}

	if result == controllerutil.OperationResultCreated {
		log.Info("Created NetworkPolicy", "name", desired.Name)
	}

	if result == controllerutil.OperationResultUpdated {
		log.Info("Updated NetworkPolicy", "name", desired.Name)
	}

	return nil
}

// warnManagerNamespaceUnset reports the missing manager namespace once: the
// reconcile repeats every poll interval and the message is the same each time.
func (r *FactorioServerReconciler) warnManagerNamespaceUnset(log logr.Logger) {
	if !r.managerNamespaceWarned.CompareAndSwap(false, true) {
		return
	}

	log.Info("ManagerNamespace is not set; the agent RPC stays reachable from the whole Pod network")
}

// applyStatefulSet copies the desired state into the StatefulSet given to
// the mutate function of CreateOrUpdate. The empty shell of a StatefulSet
// that does not exist yet carries no creation timestamp: it gets the whole
// spec and the labels, while an existing one only receives the fields the
// controller owns, because selector and serviceName are immutable.
func applyStatefulSet(statefulSet, desired *appsv1.StatefulSet) {
	if !statefulSet.CreationTimestamp.IsZero() {
		syncStatefulSet(statefulSet, desired)

		return
	}

	statefulSet.Labels = desired.Labels
	desired.Spec.DeepCopyInto(&statefulSet.Spec)
}

// syncStatefulSet copies the fields the controller owns from desired into
// existing. Everything else keeps its live value; the immutable fields
// (selector, serviceName) are never touched.
func syncStatefulSet(existing, desired *appsv1.StatefulSet) {
	desiredContainer := &desired.Spec.Template.Spec.Containers[0]

	existingContainer := containerOf(existing, desiredContainer.Name)
	if existingContainer == nil {
		existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	} else if containerDrifted(existingContainer, desiredContainer) {
		syncContainer(existingContainer, desiredContainer)
	}

	if !equality.Semantic.DeepEqual(existing.Spec.UpdateStrategy, desired.Spec.UpdateStrategy) {
		existing.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
	}

	template := &existing.Spec.Template
	if !equality.Semantic.DeepEqual(template.Labels, desired.Spec.Template.Labels) {
		template.Labels = desired.Spec.Template.Labels
	}

	// Only the template hash is owned here: every other annotation of the
	// pod template belongs to the user.
	hash := desired.Spec.Template.Annotations[resources.TemplateHashAnnotation]
	if template.Annotations[resources.TemplateHashAnnotation] != hash {
		if template.Annotations == nil {
			template.Annotations = map[string]string{}
		}

		template.Annotations[resources.TemplateHashAnnotation] = hash
	}

	podSpec := &template.Spec
	desiredPodSpec := &desired.Spec.Template.Spec

	syncPodSpec(podSpec, desiredPodSpec)
}

// syncPodSpec copies the pod spec fields the controller owns.
func syncPodSpec(podSpec, desiredPodSpec *corev1.PodSpec) {
	if !equality.Semantic.DeepEqual(podSpec.SecurityContext, desiredPodSpec.SecurityContext) {
		podSpec.SecurityContext = desiredPodSpec.SecurityContext
	}

	if !equality.Semantic.DeepEqual(
		podSpec.TerminationGracePeriodSeconds, desiredPodSpec.TerminationGracePeriodSeconds,
	) {
		podSpec.TerminationGracePeriodSeconds = desiredPodSpec.TerminationGracePeriodSeconds
	}

	if !equality.Semantic.DeepEqual(
		podSpec.AutomountServiceAccountToken, desiredPodSpec.AutomountServiceAccountToken,
	) {
		podSpec.AutomountServiceAccountToken = desiredPodSpec.AutomountServiceAccountToken
	}

	if !equality.Semantic.DeepEqual(podSpec.ImagePullSecrets, desiredPodSpec.ImagePullSecrets) {
		podSpec.ImagePullSecrets = desiredPodSpec.ImagePullSecrets
	}

	if volumesDrifted(podSpec.Volumes, desiredPodSpec.Volumes) {
		podSpec.Volumes = desiredPodSpec.Volumes
	}
}

// syncContainer copies the container fields the controller owns.
func syncContainer(existing, desired *corev1.Container) {
	existing.Image = desired.Image
	existing.Command = desired.Command
	existing.Args = desired.Args
	existing.Ports = desired.Ports
	existing.LivenessProbe = desired.LivenessProbe
	existing.VolumeMounts = desired.VolumeMounts
	existing.Resources = desired.Resources
	existing.SecurityContext = desired.SecurityContext
}

// containerOf returns the container with the given name, or nil.
func containerOf(statefulSet *appsv1.StatefulSet, name string) *corev1.Container {
	for i := range statefulSet.Spec.Template.Spec.Containers {
		if statefulSet.Spec.Template.Spec.Containers[i].Name == name {
			return &statefulSet.Spec.Template.Spec.Containers[i]
		}
	}

	return nil
}

// containerDrifted compares the container fields the controller owns.
func containerDrifted(existing, desired *corev1.Container) bool {
	return existing.Image != desired.Image ||
		!equality.Semantic.DeepEqual(existing.Command, desired.Command) ||
		!equality.Semantic.DeepEqual(existing.Args, desired.Args) ||
		!equality.Semantic.DeepEqual(existing.Ports, desired.Ports) ||
		!equality.Semantic.DeepEqual(existing.LivenessProbe, desired.LivenessProbe) ||
		!equality.Semantic.DeepEqual(existing.VolumeMounts, desired.VolumeMounts) ||
		!equality.Semantic.DeepEqual(existing.Resources, desired.Resources) ||
		!equality.Semantic.DeepEqual(existing.SecurityContext, desired.SecurityContext)
}

// volumesDrifted compares the volume fields the controller owns. The
// ephemeral volume claim template is compared only on the fields the
// controller sets, because the API server defaults the rest.
func volumesDrifted(existing, desired []corev1.Volume) bool {
	existingSaves := volumeOf(existing, resources.SavesVolumeName)

	desiredSaves := volumeOf(desired, resources.SavesVolumeName)
	if claimNameOf(existingSaves) != claimNameOf(desiredSaves) {
		return true
	}

	existingGame := volumeOf(existing, resources.GameVolumeName)
	desiredGame := volumeOf(desired, resources.GameVolumeName)

	return ephemeralStorageOf(existingGame) != ephemeralStorageOf(desiredGame) ||
		!equality.Semantic.DeepEqual(ephemeralAccessModesOf(existingGame), ephemeralAccessModesOf(desiredGame))
}

// volumeOf returns the volume with the given name, or nil.
func volumeOf(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}

	return nil
}

// claimNameOf returns the save PVC claim name of the volume.
func claimNameOf(volume *corev1.Volume) string {
	if volume == nil || volume.PersistentVolumeClaim == nil {
		return ""
	}

	return volume.PersistentVolumeClaim.ClaimName
}

// ephemeralStorageOf returns the storage request of the generic
// ephemeral volume.
func ephemeralStorageOf(volume *corev1.Volume) resource.Quantity {
	template := ephemeralTemplateOf(volume)
	if template == nil {
		return resource.Quantity{}
	}

	return template.Spec.Resources.Requests[corev1.ResourceStorage]
}

// ephemeralAccessModesOf returns the access modes of the generic
// ephemeral volume.
func ephemeralAccessModesOf(volume *corev1.Volume) []corev1.PersistentVolumeAccessMode {
	template := ephemeralTemplateOf(volume)
	if template == nil {
		return nil
	}

	return template.Spec.AccessModes
}

// ephemeralTemplateOf returns the volume claim template of the generic
// ephemeral volume, or nil.
func ephemeralTemplateOf(volume *corev1.Volume) *corev1.PersistentVolumeClaimTemplate {
	if volume == nil || volume.Ephemeral == nil {
		return nil
	}

	return volume.Ephemeral.VolumeClaimTemplate
}

// findAgentPod returns the agent Pod of server and whether it exists.
func (r *FactorioServerReconciler) findAgentPod(
	ctx context.Context,
	server *koujouoanqounetv1.FactorioServer,
) (*corev1.Pod, bool, error) {
	pods := &corev1.PodList{}

	err := r.List(ctx, pods,
		client.InNamespace(server.Namespace),
		client.MatchingLabels{resources.FactorioServerLabelKey: server.Name})
	if err != nil {
		return nil, false, fmt.Errorf("could not list the agent Pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return nil, false, nil
	}

	for i := range pods.Items {
		if pods.Items[i].Status.PodIP != "" {
			return &pods.Items[i], true, nil
		}
	}

	return &pods.Items[0], true, nil
}

// podIsReady reports whether the pod reports the Ready condition.
func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

// patchStatus re-fetches server, applies mutate to its status and
// patches the status subresource when it changed.
func (r *FactorioServerReconciler) patchStatus(
	ctx context.Context,
	server *koujouoanqounetv1.FactorioServer,
	mutate func(*koujouoanqounetv1.FactorioServerStatus),
) error {
	fresh := &koujouoanqounetv1.FactorioServer{} //nolint:exhaustruct_v5 // Populated by the API server.

	err := r.Get(ctx, client.ObjectKeyFromObject(server), fresh)
	if err != nil {
		return fmt.Errorf("could not re-fetch FactorioServer: %w", err)
	}

	original := fresh.DeepCopy()
	mutate(&fresh.Status)

	if equality.Semantic.DeepEqual(original.Status, fresh.Status) {
		return nil
	}

	err = r.Status().Patch(ctx, fresh, client.MergeFrom(original))
	if err != nil {
		return fmt.Errorf("could not update FactorioServer status: %w", err)
	}

	return nil
}

// reader returns the reader of the referenced ConfigMaps and Secrets: the
// API server directly, so the manager caches no Secret. The tests and an
// incomplete wiring fall back to Client.
//
//nolint:ireturn // The fallback picks one of the two client.Reader fields.
func (r *FactorioServerReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}

	return r.Client
}

// recordEvent emits an event unless the recorder is unset.
func (r *FactorioServerReconciler) recordEvent(server *koujouoanqounetv1.FactorioServer, reason, message string) {
	if r.Recorder == nil {
		return
	}

	r.Recorder.Eventf(server, nil, corev1.EventTypeNormal, reason, eventAction, "%s", message)
}

// pollInterval returns the agent status poll interval.
func (r *FactorioServerReconciler) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}

	return defaultPollInterval
}

// configureProgressInterval returns the agent phase poll interval used
// while a configure call is in flight.
func (r *FactorioServerReconciler) configureProgressInterval() time.Duration {
	if r.ConfigureProgressInterval > 0 {
		return r.ConfigureProgressInterval
	}

	return defaultConfigureProgressInterval
}

// retryInterval returns the requeue delay used while the agent is not
// reachable yet.
func (r *FactorioServerReconciler) retryInterval() time.Duration {
	if r.RetryInterval > 0 {
		return r.RetryInterval
	}

	return defaultRetryInterval
}

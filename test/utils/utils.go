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

// Package utils provides helpers shared by the e2e test suite, such as
// running kubectl, following pod logs, inspecting volumes and managing
// the cert-manager installation.
package utils

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck // Ginkgo DSL import is the framework convention.
)

const (
	certmanagerVersion = "v1.20.2"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	defaultKindBinary  = "kind"
	defaultKindCluster = "kind"

	outputFilePermission = 0o644

	// inspectorImage runs the volume inspector. It is pinned so that the
	// suite does not depend on a moving tag.
	inspectorImage = "busybox:1.36"

	// inspectorContainer is the name of the container of the inspector Pod.
	inspectorContainer = "inspect"

	// inspectorUID is the user of the inspector Pod: the agent user, whose
	// files it reads. It matches the nonroot user of the controller image.
	inspectorUID = int64(65532)

	// inspectorVolume is the name of the volume of the inspector Pod and
	// inspectorMountPath is where it mounts the claim.
	inspectorVolume    = "volume"
	inspectorMountPath = "/mnt"

	// inspectorPoll and inspectorTimeout bound the wait for the
	// inspector Pod to finish; the timeout includes the image pull.
	inspectorPoll    = 2 * time.Second
	inspectorTimeout = 5 * time.Minute

	// inspectorDeleteTimeout bounds the deletion of the inspector Pod.
	inspectorDeleteTimeout = 1 * time.Minute
)

// errCodeNotFound is returned when the target snippet is missing from the file.
var errCodeNotFound = errors.New("unable to find the code to be uncommented")

// errInspectorFailed marks an inspector Pod whose script exited non-zero.
var errInspectorFailed = errors.New("the inspector Pod failed")

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context.
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	err := os.Chdir(cmd.Dir)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// UninstallCertManager uninstalls the cert manager.
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)

	//nolint:gosec // The URL is a fixed template.
	cmd := exec.CommandContext(context.TODO(), "kubectl", "delete", "-f", url)

	_, err := Run(cmd)
	if err != nil {
		warnError(err)
	}

	// Delete leftover leases in kube-system (not cleaned by default)
	kubeSystemLeases := []string{
		"cert-manager-cainjector-leader-election",
		"cert-manager-controller",
	}

	for _, lease := range kubeSystemLeases {
		//nolint:gosec // Lease names are fixed.
		cmd = exec.CommandContext(context.TODO(), "kubectl", "delete", "lease", lease,
			"-n", "kube-system", "--ignore-not-found", "--force", "--grace-period=0")

		_, err = Run(cmd)
		if err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)

	//nolint:gosec // The URL is a fixed template.
	cmd := exec.CommandContext(context.TODO(), "kubectl", "apply", "-f", url)

	_, err := Run(cmd)
	if err != nil {
		return fmt.Errorf("failed to install cert manager: %w", err)
	}

	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.CommandContext(context.TODO(), "kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err = Run(cmd)

	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.CommandContext(context.TODO(), "kubectl", "get", "crds")

	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster.
func LoadImageToKindClusterWithName(name string) error {
	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}

	kindOptions := []string{"load", "docker-image", name, "--name", cluster}

	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}

	//nolint:gosec // The binary and options come from known defaults or the environment.
	cmd := exec.CommandContext(context.TODO(), kindBinary, kindOptions...)

	_, err := Run(cmd)

	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string

	elements := strings.SplitSeq(output, "\n")

	for element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is.
func GetProjectDir() (string, error) {
	projectDir, err := os.Getwd()
	if err != nil {
		return projectDir, fmt.Errorf("failed to get current working directory: %w", err)
	}

	projectDir = strings.ReplaceAll(projectDir, "/test/e2e", "")

	return projectDir, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	//nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}

	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("%w: %q", errCodeNotFound, target)
	}

	out := new(bytes.Buffer)

	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	err = writeUncommentedTarget(out, target, prefix)
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	_, err = out.Write(content[idx+len(target):])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	err = os.WriteFile(filename, out.Bytes(), outputFilePermission)
	if err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}

// writeUncommentedTarget writes the target snippet to out with the comment
// prefix removed from each line.
func writeUncommentedTarget(out *bytes.Buffer, target, prefix string) error {
	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}

	for {
		_, err := out.WriteString(strings.TrimPrefix(scanner.Text(), prefix))
		if err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}

		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			return nil
		}

		_, err = out.WriteString("\n")
		if err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}
}

// Kubectl runs kubectl with args and returns its combined output. The
// command inherits the environment of the suite, so the kuberc setting
// of the suite applies to it.
func Kubectl(ctx context.Context, args ...string) (string, error) {
	//nolint:gosec // kubectl is a fixed binary; the arguments come from the suite itself.
	cmd := exec.CommandContext(ctx, "kubectl", args...)

	return Run(cmd)
}

// PodLogFollower streams the logs of a Pod into memory, from the first
// line on, until the container terminates or the stream is stopped. The
// text stays available after the Pod is gone, so the suite can assert on
// the logs of a Pod whose deletion it triggers itself.
type PodLogFollower struct {
	stop context.CancelFunc
	done chan struct{}
	mu   sync.Mutex
	text bytes.Buffer
}

// FollowPodLogs starts following the logs of the Pod namespace/name.
func FollowPodLogs(ctx context.Context, namespace, name string) (*PodLogFollower, error) {
	ctx, cancel := context.WithCancel(ctx)

	//nolint:gosec // kubectl is a fixed binary; the arguments come from the suite itself.
	cmd := exec.CommandContext(ctx, "kubectl", "logs", "--namespace", namespace, "--follow", name)

	follower := &PodLogFollower{
		stop: cancel,
		done: make(chan struct{}),
		mu:   sync.Mutex{},
		text: bytes.Buffer{},
	}
	cmd.Stdout = follower
	cmd.Stderr = follower

	err := cmd.Start()
	if err != nil {
		cancel()

		return nil, fmt.Errorf("could not follow the logs of Pod %s: %w", name, err)
	}

	go func() {
		// The exit of kubectl is not an error of the suite: the stream
		// ends with the container, and kubectl reports its own failures
		// on the captured standard error.
		_ = cmd.Wait()

		close(follower.done)
	}()

	return follower, nil
}

// Write appends to the captured log, which makes the follower the
// standard output and error of kubectl.
func (f *PodLogFollower) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	written, err := f.text.Write(p)
	if err != nil {
		return written, fmt.Errorf("could not capture the log output: %w", err)
	}

	return written, nil
}

// Text returns the logs captured so far.
func (f *PodLogFollower) Text() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.text.String()
}

// Done is closed once the log stream ended, which is when the container
// terminated or the follower was stopped.
func (f *PodLogFollower) Done() <-chan struct{} {
	return f.done
}

// Stop ends the log stream and waits until it ended.
func (f *PodLogFollower) Stop() {
	f.stop()
	<-f.done
}

// RunVolumeInspector runs a short-lived busybox Pod named name that
// mounts claim read-only at /mnt, runs script and returns the output of
// the script. The Pod is deleted before the function returns.
func RunVolumeInspector(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, claim, name, script string,
) (string, error) {
	pods := clientset.CoreV1().Pods(namespace)

	_, err := pods.Create(ctx, newInspectorPod(namespace, claim, name, script), metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("could not create the inspector Pod %s: %w", name, err)
	}

	defer func() {
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inspectorDeleteTimeout)
		defer cancel()

		err := pods.Delete(deleteCtx, name, metav1.DeleteOptions{})
		if err != nil {
			warnError(fmt.Errorf("could not delete the inspector Pod %s: %w", name, err))
		}
	}()

	err = wait.PollUntilContextTimeout(ctx, inspectorPoll, inspectorTimeout, true, inspectorFinished(pods, name))
	if err != nil {
		return "", fmt.Errorf("the inspector Pod %s did not finish: %w: %s", name, err, podOutput(ctx, pods, name))
	}

	err = inspectorResult(ctx, pods, name)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, podOutput(ctx, pods, name))
	}

	output, err := podLogs(ctx, pods, name)
	if err != nil {
		return "", fmt.Errorf("could not read the output of the inspector Pod %s: %w", name, err)
	}

	return output, nil
}

// inspectorResult checks that the inspector Pod name ran its script to
// completion.
func inspectorResult(ctx context.Context, pods typedcorev1.PodInterface, name string) error {
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("could not get the inspector Pod %s: %w", name, err)
	}

	if pod.Status.Phase != corev1.PodSucceeded {
		return fmt.Errorf("%w with phase %s", errInspectorFailed, pod.Status.Phase)
	}

	return nil
}

// inspectorFinished reports whether the inspector Pod name stopped
// running, successfully or not.
func inspectorFinished(pods typedcorev1.PodInterface, name string) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("could not get the inspector Pod %s: %w", name, err)
		}

		phase := pod.Status.Phase

		return phase == corev1.PodSucceeded || phase == corev1.PodFailed, nil
	}
}

// podLogs returns the logs of the Pod name.
func podLogs(ctx context.Context, pods typedcorev1.PodInterface, name string) (string, error) {
	logs, err := pods.GetLogs(name, &corev1.PodLogOptions{}).Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("could not read the logs of Pod %s: %w", name, err)
	}

	return string(logs), nil
}

// podOutput returns the logs of the Pod name, or the error of the log
// request, so that it can be embedded in an error message.
func podOutput(ctx context.Context, pods typedcorev1.PodInterface, name string) string {
	logs, err := podLogs(ctx, pods, name)
	if err != nil {
		return fmt.Sprintf("could not read the logs of the inspector Pod: %v", err)
	}

	return logs
}

// newInspectorPod returns a Pod that mounts claim read-only and runs
// script with busybox. The Pod satisfies the restricted Pod Security
// Standards and runs as the agent user, so that it can read the files
// the agent wrote.
func newInspectorPod(namespace, claim, name, script string) *corev1.Pod {
	runtimeDefault := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   new(true),
				RunAsUser:      new(inspectorUID),
				RunAsGroup:     new(inspectorUID),
				FSGroup:        new(inspectorUID),
				SeccompProfile: runtimeDefault,
			},
			Containers: []corev1.Container{{
				Name:    inspectorContainer,
				Image:   inspectorImage,
				Command: []string{"sh", "-c", script},
				VolumeMounts: []corev1.VolumeMount{
					{Name: inspectorVolume, MountPath: inspectorMountPath, ReadOnly: true},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: new(false),
					ReadOnlyRootFilesystem:   new(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: inspectorVolume,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: claim,
						ReadOnly:  true,
					},
				},
			}},
		},
	}
}

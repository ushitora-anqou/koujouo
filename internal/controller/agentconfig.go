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
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	koujouoanqounetv1 "github.com/ushitora-anqou/koujouo/api/v1"
	rpcapi "github.com/ushitora-anqou/koujouo/rpc"
)

var errKeyMissing = errors.New("key is missing")

var errTooManyFiles = errors.New("too many files")

const (
	// defaultLeaveTimeoutSeconds is applied when the spec omits it.
	defaultLeaveTimeoutSeconds = 60

	// configFileName and serverWhitelistFileName are the file names the
	// agent expects in Config.files.
	configFileName          = "config.ini"
	serverWhitelistFileName = "server-whitelist.json"
)

// agentFile is one file delivered to the agent, for example config.ini.
type agentFile struct {
	name    string
	content string
}

// backupSettings configures the periodic save upload. A nil pointer
// disables backups.
type backupSettings struct {
	endpointURL     string
	bucket          string
	accessKeyID     string
	secretAccessKey string
}

// agentConfig is the full desired state of one agent, assembled from the
// FactorioServer spec and sent verbatim on every configure call.
type agentConfig struct {
	gameVersion         string
	files               []agentFile
	discordWebhookURL   string
	leaveTimeoutSeconds int32
	backup              *backupSettings
	downloadUsername    string
	downloadToken       string
}

// callAgentConfigure sends the full desired state to the agent and waits
// for the return, which the agent issues after the state is applied (on a
// game version change: after drain, download and restart). While it
// waits, it polls the agent and reports each phase change through
// onProgress, so a drain or a download of several minutes stays visible
// on the FactorioServer status.
//
// The wait is not bound to the reconcile context: a drain waits for the
// leave timeout, which the user may set past the reconcile timeout of
// controller-runtime. The call still ends when the agent disconnects or
// when the controller shuts down (ctx cancelled); the reconcile then
// returns and the next one finds the agent already configured.
func (r *FactorioServerReconciler) callAgentConfigure(
	ctx context.Context,
	agent rpcapi.Agent,
	config *agentConfig,
	onProgress func(reported *agentStatus),
) error {
	future, release := agent.Configure(context.WithoutCancel(ctx), func(params rpcapi.Agent_configure_Params) error {
		return setAgentConfig(params, config)
	})
	defer release()

	done := make(chan error, 1)

	go func() {
		_, err := future.Struct()
		if err != nil {
			done <- fmt.Errorf("agent configure call failed: %w", err)

			return
		}

		done <- nil
	}()

	if onProgress == nil {
		return <-done
	}

	return r.awaitConfigure(ctx, agent, done, onProgress)
}

// defaultConfigureProgressInterval is how often the controller polls the
// agent phase while a configure call is in flight.
const defaultConfigureProgressInterval = 2 * time.Second

// awaitConfigure polls the agent status until the configure call returns.
// A failing poll is not fatal: the configure result decides the outcome.
// Status patches go on without a deadline as well, so a long drain is
// reported all the way to its end; only a controller shutdown ends the
// wait early.
func (r *FactorioServerReconciler) awaitConfigure(
	ctx context.Context,
	agent rpcapi.Agent,
	done <-chan error,
	onProgress func(reported *agentStatus),
) error {
	waitCtx := context.WithoutCancel(ctx)

	ticker := time.NewTicker(r.configureProgressInterval())
	defer ticker.Stop()

	last := (*agentStatus)(nil)

	for {
		select {
		case err := <-done:
			return err
		case <-waitCtx.Done():
			// Only a controller shutdown lands here; the configure call is
			// bound to the same context and settles with it.
			return <-done
		case <-ticker.C:
		}

		reported, err := r.callAgentStatus(waitCtx, agent)
		if err != nil || sameAgentPhase(last, reported) {
			continue
		}

		last = reported
		onProgress(reported)
	}
}

// sameAgentPhase reports whether two agent states differ in what the
// status shows during a configure: phase, game version and player count.
func sameAgentPhase(previous, current *agentStatus) bool {
	return previous != nil && previous.phase == current.phase &&
		previous.gameVersion == current.gameVersion &&
		previous.players == current.players
}

// setAgentConfig writes config into the configure call parameters.
func setAgentConfig(params rpcapi.Agent_configure_Params, config *agentConfig) error {
	out, err := params.NewCfg()
	if err != nil {
		return fmt.Errorf("could not allocate the Config struct: %w", err)
	}

	err = out.SetGameVersion(config.gameVersion)
	if err != nil {
		return fmt.Errorf("could not set the game version: %w", err)
	}

	err = writeAgentFiles(out, config.files)
	if err != nil {
		return err
	}

	err = out.SetDiscordWebhookURL(config.discordWebhookURL)
	if err != nil {
		return fmt.Errorf("could not set the Discord webhook URL: %w", err)
	}

	out.SetLeaveTimeoutSeconds(config.leaveTimeoutSeconds)

	if config.backup != nil {
		err = writeBackupSettings(out, config.backup)
		if err != nil {
			return err
		}
	}

	err = out.SetDownloadUsername(config.downloadUsername)
	if err != nil {
		return fmt.Errorf("could not set the download username: %w", err)
	}

	err = out.SetDownloadToken(config.downloadToken)
	if err != nil {
		return fmt.Errorf("could not set the download token: %w", err)
	}

	return nil
}

// writeAgentFiles writes the delivered files into the Config struct.
func writeAgentFiles(out rpcapi.Config, files []agentFile) error {
	count := len(files)
	if count > math.MaxInt32 {
		return fmt.Errorf("too many files: %d: %w", count, errTooManyFiles)
	}

	list, err := out.NewFiles(int32(count))
	if err != nil {
		return fmt.Errorf("could not allocate the file list: %w", err)
	}

	for i, file := range files {
		dst := list.At(i)

		err = dst.SetName(file.name)
		if err != nil {
			return fmt.Errorf("could not set the name of file %s: %w", file.name, err)
		}

		err = dst.SetContent([]byte(file.content))
		if err != nil {
			return fmt.Errorf("could not set the content of file %s: %w", file.name, err)
		}
	}

	return nil
}

// writeBackupSettings writes the backup settings into the Config struct.
func writeBackupSettings(out rpcapi.Config, backup *backupSettings) error {
	dst, err := out.NewBackup()
	if err != nil {
		return fmt.Errorf("could not allocate the BackupSettings struct: %w", err)
	}

	err = dst.SetEndpointURL(backup.endpointURL)
	if err != nil {
		return fmt.Errorf("could not set the backup endpoint URL: %w", err)
	}

	err = dst.SetBucket(backup.bucket)
	if err != nil {
		return fmt.Errorf("could not set the backup bucket: %w", err)
	}

	err = dst.SetAccessKeyID(backup.accessKeyID)
	if err != nil {
		return fmt.Errorf("could not set the backup access key id: %w", err)
	}

	err = dst.SetSecretAccessKey(backup.secretAccessKey)
	if err != nil {
		return fmt.Errorf("could not set the backup secret access key: %w", err)
	}

	return nil
}

// callAgentStatus polls the current state of the agent.
func (r *FactorioServerReconciler) callAgentStatus(ctx context.Context, agent rpcapi.Agent) (*agentStatus, error) {
	future, release := agent.Status(ctx, nil)
	defer release()

	result, err := future.Struct()
	if err != nil {
		return nil, fmt.Errorf("agent status call failed: %w", err)
	}

	reported, err := result.St()
	if err != nil {
		return nil, fmt.Errorf("could not read the agent status: %w", err)
	}

	gameVersion, err := reported.GameVersion()
	if err != nil {
		return nil, fmt.Errorf("could not read the reported game version: %w", err)
	}

	agentVersion, err := reported.AgentVersion()
	if err != nil {
		return nil, fmt.Errorf("could not read the reported agent version: %w", err)
	}

	return &agentStatus{
		phase:        reported.Phase(),
		gameVersion:  gameVersion,
		players:      reported.Players(),
		agentVersion: agentVersion,
	}, nil
}

// callAgentStop asks the agent to stop the game gracefully and waits
// for the return, which the agent issues after the save file has been
// written and the game process has exited. Like callAgentConfigure, the
// wait outlives the reconcile deadline and reports each phase change
// through onProgress.
func (r *FactorioServerReconciler) callAgentStop(
	ctx context.Context,
	agent rpcapi.Agent,
	onProgress func(reported *agentStatus),
) error {
	future, release := agent.Stop(context.WithoutCancel(ctx), nil)
	defer release()

	done := make(chan error, 1)

	go func() {
		_, err := future.Struct()
		if err != nil {
			done <- fmt.Errorf("agent stop call failed: %w", err)

			return
		}

		done <- nil
	}()

	if onProgress == nil {
		return <-done
	}

	return r.awaitConfigure(ctx, agent, done, onProgress)
}

// buildAgentConfig assembles the full desired state of the agent from
// the FactorioServer spec, resolving the referenced ConfigMap and Secret
// keys.
func (r *FactorioServerReconciler) buildAgentConfig(
	ctx context.Context,
	server *koujouoanqounetv1.FactorioServer,
) (*agentConfig, error) {
	config := &agentConfig{
		gameVersion:         server.Spec.GameVersion,
		files:               nil,
		discordWebhookURL:   "",
		leaveTimeoutSeconds: defaultLeaveTimeoutSeconds,
		backup:              nil,
		downloadUsername:    "",
		downloadToken:       "",
	}

	if server.Spec.LeaveTimeoutSeconds != nil {
		config.leaveTimeoutSeconds = *server.Spec.LeaveTimeoutSeconds
	}

	err := r.appendGameConfigFiles(ctx, server, config)
	if err != nil {
		return nil, err
	}

	err = r.appendNotification(ctx, server, config)
	if err != nil {
		return nil, err
	}

	err = r.appendBackup(ctx, server, config)
	if err != nil {
		return nil, err
	}

	err = r.appendDownloadCredentials(ctx, server, config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// appendGameConfigFiles resolves spec.gameConfig into the delivered files.
func (r *FactorioServerReconciler) appendGameConfigFiles(
	ctx context.Context,
	server *koujouoanqounetv1.FactorioServer,
	config *agentConfig,
) error {
	gameConfig := server.Spec.GameConfig
	if gameConfig == nil {
		return nil
	}

	if gameConfig.Config != nil {
		content, err := r.readConfigMapKey(ctx, server.Namespace, gameConfig.Config)
		if err != nil {
			return err
		}

		config.files = append(config.files, agentFile{name: configFileName, content: content})
	}

	if gameConfig.ServerWhitelist != nil {
		content, err := r.readConfigMapKey(ctx, server.Namespace, gameConfig.ServerWhitelist)
		if err != nil {
			return err
		}

		config.files = append(config.files, agentFile{name: serverWhitelistFileName, content: content})
	}

	return nil
}

// appendNotification resolves spec.notification into the config.
func (r *FactorioServerReconciler) appendNotification(
	ctx context.Context,
	server *koujouoanqounetv1.FactorioServer,
	config *agentConfig,
) error {
	notification := server.Spec.Notification
	if notification == nil || notification.DiscordWebhookURL == nil {
		return nil
	}

	webhookURL, err := r.readSecretKey(ctx, server.Namespace, notification.DiscordWebhookURL)
	if err != nil {
		return err
	}

	config.discordWebhookURL = webhookURL

	return nil
}

// appendBackup resolves spec.backup into the config.
func (r *FactorioServerReconciler) appendBackup(
	ctx context.Context,
	server *koujouoanqounetv1.FactorioServer,
	config *agentConfig,
) error {
	backup := server.Spec.Backup
	if backup == nil || backup.Credentials == nil {
		return nil
	}

	accessKeyID, err := r.readSecretKey(ctx, server.Namespace, backup.Credentials.AccessKeyID)
	if err != nil {
		return err
	}

	secretAccessKey, err := r.readSecretKey(ctx, server.Namespace, backup.Credentials.SecretAccessKey)
	if err != nil {
		return err
	}

	config.backup = &backupSettings{
		endpointURL:     backup.EndpointURL,
		bucket:          backup.Bucket,
		accessKeyID:     accessKeyID,
		secretAccessKey: secretAccessKey,
	}

	return nil
}

// appendDownloadCredentials resolves spec.downloadCredentials into the
// config.
func (r *FactorioServerReconciler) appendDownloadCredentials(
	ctx context.Context,
	server *koujouoanqounetv1.FactorioServer,
	config *agentConfig,
) error {
	credentials := server.Spec.DownloadCredentials
	if credentials == nil {
		return nil
	}

	username, err := r.readSecretKey(ctx, server.Namespace, credentials.Username)
	if err != nil {
		return err
	}

	token, err := r.readSecretKey(ctx, server.Namespace, credentials.Token)
	if err != nil {
		return err
	}

	config.downloadUsername = username
	config.downloadToken = token

	return nil
}

// readConfigMapKey returns the value of the selected ConfigMap key.
func (r *FactorioServerReconciler) readConfigMapKey(
	ctx context.Context,
	namespace string,
	selector *corev1.ConfigMapKeySelector,
) (string, error) {
	configMap := &corev1.ConfigMap{}

	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: selector.Name}, configMap)
	if err != nil {
		return "", fmt.Errorf("could not get ConfigMap %s: %w", selector.Name, err)
	}

	value, ok := configMap.Data[selector.Key]
	if !ok {
		return "", fmt.Errorf("could not read key %q of ConfigMap %s: %w", selector.Key, selector.Name, errKeyMissing)
	}

	return value, nil
}

// readSecretKey returns the value of the selected Secret key.
func (r *FactorioServerReconciler) readSecretKey(
	ctx context.Context,
	namespace string,
	selector *corev1.SecretKeySelector,
) (string, error) {
	secret := &corev1.Secret{}

	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: selector.Name}, secret)
	if err != nil {
		return "", fmt.Errorf("could not get Secret %s: %w", selector.Name, err)
	}

	value, ok := secret.Data[selector.Key]
	if !ok {
		return "", fmt.Errorf("could not read key %q of Secret %s: %w", selector.Key, selector.Name, errKeyMissing)
	}

	return string(value), nil
}

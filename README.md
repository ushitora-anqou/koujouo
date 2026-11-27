# koujouo

koujouo is a Kubernetes operator that hosts headless [Factorio](https://www.factorio.com/)
servers. It introduces the `FactorioServer` custom resource.

## Description

A `FactorioServer` runs exactly one Factorio headless server. For each one the
controller creates a StatefulSet with a single replica of an **agent**, a small
process that is shipped in the controller image. The agent and the controller
talk over Cap'n Proto RPC.

The agent:

- downloads the Factorio game binary of the pinned `spec.gameVersion` onto a
  generic ephemeral volume, so neither the agent image nor the cluster nodes
  carry the game binary (the image can be published to a public registry),
- starts the game as a child process and forwards its log,
- sends a Discord message when a player joins or leaves the game,
- stops the game gracefully: it warns the players over RCON, waits up to
  `spec.leaveTimeoutSeconds` for them to leave, then sends SIGINT so Factorio
  writes the save file,
- uploads the save file to S3-compatible storage,
- replaces its own binary with `execve(2)` when the controller ships a newer one,
  without stopping the game.

The save data lives on a PersistentVolumeClaim that you create yourself. The
controller never creates or deletes it, so the world survives suspension and
reinstallation. The game settings are read from ConfigMaps and Secrets by the
controller and handed to the agent over RPC; they are not mounted into the Pod.

## Getting Started

### Prerequisites
- [Nix](https://nixos.org/) with flake support — provides Go, kubectl, and all
  the other development tools via `nix develop`
- docker version 17.03+ (to load and push images)
- Access to a Kubernetes cluster.
- A StorageClass that can bind a `ReadWriteOnce` volume. The agent Pod runs as
  a non-root user (uid 65532) and sets `fsGroup`, so the volume becomes
  writable for it.
- The agent Pod needs outbound access to `factorio.com` to download the game
  (about 61 MB per version).

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/koujouo:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/koujouo:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

The manager reads its configuration from `/etc/koujouo/manager.yaml`, which the
deployment mounts from a ConfigMap (see [Configuration](#configuration)). The
scaffolded Kustomize manifests in `config/` still start the manager with the
command-line flags it no longer takes; they need to mount the configuration file
and pass `--config` instead.

**Create a server**

A `FactorioServer` refers to three things that you create first:

| Object | Used for |
| --- | --- |
| PersistentVolumeClaim | the save data (`.spec.savePersistentVolumeClaimName`) |
| ConfigMap | `config.ini` and the server whitelist (`.spec.gameConfig`) |
| Secret | the Discord webhook URL, the S3 credentials and, for experimental or old builds, the Factorio download credentials |

The samples in `config/samples/` show all of them. Edit the placeholders in
`config/samples/secret.yaml` and, if your cluster has no default
StorageClass, set `storageClassName` in `config/samples/pvc.yaml`. Then apply
them:

```sh
kubectl apply -k config/samples/
```

The controller creates the StatefulSet, the agent downloads Factorio
`2.1.24`, creates a save and starts the server:

```console
$ kubectl get factorioserver
NAME                     STATUS    VERSION   PLAYERS   AGE
factorioserver-sample    Running   2.1.24    0         2m
```

The operator creates no Service. Players reach the game through a Service that
you manage yourself, selecting the Pod and forwarding UDP 34197.

**Change the game version**

Set `.spec.gameVersion`. The agent warns the players, waits for them to leave
(up to `.spec.leaveTimeoutSeconds`), saves, downloads the new release and
starts it. While it works, the status shows `Draining`, `Saving` and
`Downloading`.

**Suspend a server**

Set `.spec.suspend: true`. The agent stops the game gracefully, then the
controller deletes the StatefulSet. The PersistentVolumeClaim and the save
stay. Set it back to `false` to start the server again on the same save.

**Update the controller**

The agent binary ships in the controller image. When a newer controller image
is rolled out, the controller streams the new agent binary to the running agent,
which replaces itself in place. The game keeps running. For this to hold the
StatefulSet uses the `OnDelete` update strategy: its Pod is not replaced when
the image changes.

## Configuration

Both binaries take their configuration from a YAML file, named by the only
command-line flag they have:

| Binary | Flag | Default path | Where the file comes from |
| --- | --- | --- | --- |
| manager | `--config` | `/etc/koujouo/manager.yaml` | the ConfigMap the deployment mounts |
| agent | `--config` | `/etc/koujouo/agent.yaml` | baked into the image |

[`examples/manager.yaml`](examples/manager.yaml) and
[`examples/agent.yaml`](examples/agent.yaml) list every key with its default
value. A key the file leaves out keeps its default; a file that is missing or
does not parse stops the binary with an error. The keys are lowerCamelCase, and
`log.development` and `log.level` choose the encoding (console or JSON) and the
level of the logs. Neither binary reads its configuration from environment
variables.

The manager's `controllerImage` is the image the agent Pods run: the controller
deploys it for the StatefulSets it creates and streams `/kojouo/agent` from its
own filesystem to the agents of another build, so it normally holds the
manager's own image. With an empty `controllerImage` the controller cannot
create a StatefulSet.

`make run` runs the manager on the host with `examples/manager.yaml`.

## Known limitations

- If the game process exits on its own, for example after a crash, the status
  turns `Failed` and the agent does not restart it. Set `.spec.suspend: true`
  and back to `false`, or change `.spec.gameVersion`, to start it again.
- The agent keeps the game binary of every version it ran on the ephemeral
  volume (about 313 MB each). The default volume is 2Gi; raise
  `.spec.ephemeralVolume` after several version changes within the life of one
  Pod.
- The Discord notification for a player who joins or leaves relies on the
  `[JOIN]` / `[LEAVE]` lines of the Factorio log. Its format was not verified
  against a live multiplayer session; only the parsing is unit tested.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

Deleting a `FactorioServer` deletes its StatefulSet. The PersistentVolumeClaim
is yours and is not deleted.

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/koujouo:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/koujouo/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing
Enter the development shell first; it provides every tool:

```sh
nix develop
```

| Command | What it does |
| --- | --- |
| `make manifests generate` | Regenerates the CRDs, RBAC and DeepCopy code after editing `api/` |
| `make generate-capnp` | Regenerates `rpc/koujouo.capnp.go` after editing `rpc/koujouo.capnp` |
| `make lint-fix` | Runs golangci-lint and fixes what it can |
| `make test` | Unit tests and envtest tests |
| `make test-e2e` | End-to-end tests on a dedicated Kind cluster with the real Factorio binary |

The e2e tests post nothing to Discord unless `DISCORD_WEBHOOK_URL` is set.
Run them only against a throwaway Kind cluster: they create and delete the
cluster `koujouo-test-e2e`.

The Nix flake only sees git-tracked files. Run `git add` on your changes
before `nix build .#manager` or `make docker-build`. The Go binaries are
built with [go2nix](https://github.com/numtide/go2nix) without a lockfile:
the module hashes come from `go.sum`, so a change of `go.mod`/`go.sum`
needs no hash update. Its Nix plugin is loaded by the dev shell, so
`nix build .#manager` works there; outside of it pass `--option plugin-files`
as described in [AGENTS.md](AGENTS.md) (`make docker-build` does it for you).

The design notes live in [docs/design.md](docs/design.md).

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

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


# koujouo - AI Agent Guide

koujouo is a Kubernetes operator that hosts headless Factorio servers. It ships one CRD,
`FactorioServer` (group `koujouo.anqou.net`, version `v1`), and one container image that holds
both the controller manager and the agent that runs the game. This file holds the rules for
working on the code; the behaviour is described in [README.md](README.md) and the design
rationale in [docs/design.md](docs/design.md) (Japanese).

## Project Structure

**Layout (single API group `koujouo.anqou.net`):**
```
cmd/manager/main.go               Manager entry (registers the FactorioServer controller)
cmd/agent/main.go                 Agent entry: PID 1 of the agent Pod, serves the Cap'n Proto RPC
api/v1/factorioserver_types.go    FactorioServer CRD schema (+kubebuilder markers)
api/v1/zz_generated.deepcopy.go   Auto-generated (DO NOT EDIT)
internal/controller/              Reconciliation
  factorioserver_controller.go      StatefulSet, suspension, status
  agentclient.go                    AgentDialer: the TCP connection to an agent Pod
  agentconfig.go                    Reads the referenced ConfigMaps/Secrets; configure/status/stop
  agentupgrade.go                   Streams the agent binary to an outdated agent
  status.go                         Conditions; derives status.phase
  resources/statefulset.go          Builds the agent StatefulSet (pod template hash,
                                    liveness probe on the RPC port, ephemeral volume)
  resources/networkpolicy.go        Builds the agent NetworkPolicy (UDP 34197 from
                                    anywhere, TCP 9550 from the manager namespace only)
  suite_test.go                     envtest setup (fakeagent_test.go: the in-process fake agent)
internal/agent/                   The agent: game lifecycle, game download, graceful stop, RCON,
                                  Discord notifications, S3 backup, self-upgrade
internal/version/version.go       Build version shared by the manager and the agent
rpc/koujouo.capnp                 Controller <-> agent protocol (edit this)
rpc/koujouo.capnp.go              Auto-generated (DO NOT EDIT; from `make generate-capnp`)
rpc/generate.go                   `go:generate` directive of the schema
charts/koujouo/Chart.yaml          Helm chart metadata (version, appVersion, icon)
charts/koujouo/values.yaml         Helm values: image, configuration, security contexts, ServiceMonitor
charts/koujouo/templates/          Helm templates; the CRD and manager-role.yaml are generated
examples/manager.yaml             Manager configuration (every key with its default)
examples/agent.yaml               Agent configuration (every key with its default)
examples/*.yaml                   PVC, ConfigMap, Secret (placeholders) and a FactorioServer
test/e2e/                         E2E suite (build tag `e2e`); test/utils/ holds its helpers
docs/design.md                    Design notes (Japanese): libraries, CRD and capnp schemas, tests
docs/factorio-server-plan*.html   The plan docs/design.md was derived from
hack/boilerplate.go.txt           License header of the Go files
flake.nix                         Nix flake: dev shell, binaries and docker image
flake.lock                        Pinned Nix inputs (update with `nix flake update`, DO NOT hand-edit)
.devcontainer/                    Scaffolded dev container (kind, kubebuilder, kubectl; no Nix)
.golangci.yml                     golangci-lint config (all linters enabled)
Makefile                          Build, test and image commands
.github/workflows/ci.yml          CI: make all and check-uncomitted
.github/workflows/e2e.yml         CI: make test-e2e (Kind, Docker)
.github/workflows/release.yml     Release: multi-arch image to GHCR; on a `v*` tag the chart
                                  `.tgz` attached to the GitHub Release
PROJECT                           Kubebuilder metadata Auto-generated (DO NOT EDIT)
```

`internal/webhook/` and the webhook manifests do not exist until
`kubebuilder create webhook` scaffolds them; the scaffolded manifests belong in
`charts/koujouo/templates/` like the other generated manifests.

**Multi-group layout** (for projects with multiple API groups):
```
api/<group>/<version>/*_types.go       CRD schemas by group
internal/controller/<group>/*          Controllers by group
internal/webhook/<group>/<version>/*   Webhooks by group and version (if present)
```

Multi-group layout organizes APIs by group name (e.g., `batch`, `apps`). Check the `PROJECT` file for `multigroup: true`.

**To convert to multi-group layout:**
1. Run: `kubebuilder edit --multigroup=true`
2. Move APIs: `mkdir -p api/<group> && mv api/<version> api/<group>/`
3. Move controllers: `mkdir -p internal/controller/<group> && mv internal/controller/*.go internal/controller/<group>/`
4. Move webhooks (if present): `mkdir -p internal/webhook/<group> && mv internal/webhook/<version> internal/webhook/<group>/`
5. Update import paths in all files
6. Fix `path` in `PROJECT` file for each resource
7. Update test suite CRD paths (add one more `..` to relative paths)

## How It Works

The manager and the agent are two binaries of one image. They talk
[Cap'n Proto RPC](rpc/koujouo.capnp) over plain TCP (pod IP, port 9550). The
transport is **unencrypted**: the NetworkPolicy stays the outer boundary, and
on top of it every state-changing call (`configure`, `stop`, `upgrade`) carries
the RPC token of the server, which the agent verifies against the token file it
mounts. `status` takes no token, on purpose (see the token bullet below).

- **Manager** (`/manager`, the image entrypoint) reconciles each `FactorioServer` into a
  single-replica StatefulSet of agent Pods. It reads the ConfigMaps and Secrets the spec
  refers to and sends the full desired state on every `configure` call. It creates no
  Service and never creates or deletes the save PVC.
- **Agent** (`/kojouo/agent`, PID 1) downloads the Factorio build of `spec.gameVersion`
  onto a generic ephemeral volume (`/opt/koujouo`), runs the game as its child process and
  owns its lifecycle: drain over RCON, save on SIGINT, Discord notifications, hourly S3
  backups and the graceful stop on SIGTERM. The save data and the agent state live on the
  user's PVC (`/var/lib/koujouo`), so the world survives suspension and Pod replacement.

A running server is reconciled in this order: ensure the StatefulSet and the NetworkPolicy →
wait for a ready agent Pod → `status` → `upgrade` when the agent reports another build than
the manager → resolve the referenced ConfigMaps and Secrets and hash the configuration →
`configure` when the spec generation, the game version or the configuration hash changed →
patch the status (also with the hash just delivered) → requeue after 30s. `spec.suspend: true`
calls `stop`, deletes the StatefulSet and keeps the PVC. A game that exits on its own ends the
whole agent rather than only the game: the Pod restarts and the new agent resumes the game from
the cache it left on the save volume (see the next bullet and the known limitations in
README.md).

- **A game that exits on its own ends the agent, so the kubelet restarts the Pod**
  (`internal/agent/agent.go`, `cmd/agent/main.go`). `watchGame` reports the exit of a game the
  agent still runs on the channel `Agent.Fatal()` returns; `cmd/agent` waits on that channel
  beside the signals and the RPC server error, and a fatal error makes it close the listener,
  call `Shutdown` (a crashed game leaves nothing to stop) and return the error, so the process
  exits non-zero. The kubelet then restarts the container under its `restartPolicy: Always`,
  and the new agent resumes the game from `agent-state.json` on the save volume
  (`Resume`/`resumeFromState`) before the controller dials it again. A game that keeps
  crashing therefore shows up as `CrashLoopBackOff`. A **requested** stop stays out of that
  path: `watchGame` reports only a game the agent still owns and `stopLocked` clears the
  handle before the exit is seen, so SIGTERM, `stop` and a version change never look like a
  crash.
- **The agent Pod is protected by a NetworkPolicy** (`internal/controller/resources/networkpolicy.go`).
  Ingress admits UDP 34197 from any source, because the players connect from anywhere, and
  TCP 9550 — the token-authenticated but plain-text Cap'n Proto RPC — from the manager namespace
  only. Egress stays open: the agent downloads the game, uploads the saves and posts to
  Discord. The manager namespace comes from `POD_NAMESPACE` of the manager Deployment
  (downward API, `charts/koujouo/templates/deployment.yaml`); `cmd/manager/main.go` falls
  back to the namespace file of the mounted ServiceAccount. An empty `ManagerNamespace`
  (the manager runs outside a cluster) means **no NetworkPolicy is created** and the RPC
  stays reachable from the whole Pod network; the reconcile logs that once. The token still
  authenticates the clients there: the policy is the outer boundary, not the only one.
- **The controller reads the referenced Secrets and ConfigMaps through `APIReader`**, not
  through the cache: they are few and need no cluster-wide watch, and this keeps the
  Secrets out of the manager's memory cache.
- **A change of a referenced ConfigMap or Secret reaches the agent without a spec change**
  (`agentConfig.configHash` in `internal/controller/agentconfig.go`). Once the agent is
  reachable, every reconcile resolves the references and hashes the whole configuration: game
  version, the file names and contents, the Discord webhook URL, the leave timeout, the backup
  endpoint/bucket/keys and the download credentials. The hash goes into
  `status.observedConfigHash` only after a successful configure (never from the progress
  polls, never after a failure, so the next reconcile retries), and `needsConfigure` compares
  it as well, so editing `config.ini` or rotating a Secret reconfigures the agent on the next
  poll without touching the spec. Only the hash reaches the status, never a secret value. A
  reference that cannot be resolved yet (a ConfigMap, a Secret or a selected key that does
  not exist) is reported in the `ReferencedResourceMissing` condition and retried after
  `RetryInterval` (5s) with a clean return instead of a failed reconcile, whose exponential
  backoff would delay the retry by up to about 16 minutes.
- **Creating a `FactorioServer` grants reading the Secrets it names.** The spec selectors
  (`notification.discordWebhookURL`, `backup.credentials`, `downloadCredentials`) name any
  Secret of the namespace; the controller delivers the values to the agent, which writes
  them into `agent-state.json` on the save volume. Whoever may mount that volume, or reach
  the agent RPC, can read them. This is the intended model — the user of a server owns its
  credentials — so do not add a cross-namespace reference or a webhook that reads Secrets
  on behalf of a user without an authorization check.
- **The controller creates one RPC token Secret per server and mounts it into the agent Pod**
  (`ensureAgentToken` in `internal/controller/factorioserver_controller.go`,
  `resources.AgentTokenSecretName`). `<FactorioServer name>-agent-token` holds a 32-byte random
  hex token under the key `token`: the controller reads it through `APIReader`, creates it when
  it is missing with the `FactorioServer` as its owner and **never rotates** it, so only
  deleting the Secret produces a new token. The agent Pod mounts it read-only at
  `/var/run/koujouo` with mode 0440, which the `fsGroup` 65532 makes readable for the non-root
  agent, and the agent reads `<mount>/token` — the `tokenFile` of its configuration.
- **`configure`, `stop` and `upgrade` carry the token in their call parameters; the agent
  verifies each call on its own** (`Agent.requireToken`, `internal/agent/agent.go`). A
  connection-wide `authenticate` call or an `authenticated` flag of the agent would let a
  client that authenticated once open the way for every other client, because one agent
  serves several connections at a time. The agent reads the token file on every call, so a
  recreated Secret reaches it without a restart, and compares with
  `crypto/subtle.ConstantTimeCompare`. The token never appears in a log, an error or the
  status. **`status` takes no token**: it carries no secret (only the agent version, the game
  version and the player count), and the controller polls it before it knows whether the
  agent verifies a token at all, so an old agent must keep answering. An old agent ignores
  the new parameter fields and keeps working; a new agent refuses a call whose token is
  missing or wrong. A missing token file or an empty one is an error, never a way to run
  without authentication; only an agent started without `tokenFile` and `Token` (the unit
  tests, a development run) accepts every call.
- **The pod template hash decides when a Pod is replaced.** The StatefulSet keeps
  `updateStrategy: OnDelete`, so a template change alone reaches no running Pod. The
  controller therefore stamps the hash of the owned pod template fields (labels and spec,
  with the agent container image blanked out) into the annotation
  `koujouo.anqou.net/template-hash`, the StatefulSet controller copies it into each Pod, and
  a Pod of another hash is deleted so that the StatefulSet recreates it. **Every owned
  template field except the image** — the save PVC name, `spec.agent.resources`, the
  ephemeral volume, the `imagePullSecrets`, the security contexts, the probe, the token
  Secret volume and its mount — therefore restarts the game once. An **image-only** change
  is left out of the hash and reaches the agent only through the in-place upgrade path, so it
  never restarts the game.
- **The agent Pod carries no ServiceAccount token** (`automountServiceAccountToken: false`):
  it never talks to the API server.
- **The reconciler runs 8 reconciles at a time** (`defaultMaxConcurrentReconciles`): a
  configure of one server takes minutes, and a single worker would hold back every other
  server that long. Each status call is bounded by 30 seconds
  (`defaultAgentStatusTimeout`), so an agent that stops answering costs one poll, not the
  whole wait.
- **The agent bounds its own shutdown.** `Shutdown` raises the `terminating` flag before it
  waits for the lifecycle lock and waits with a deadline, so a configure that is still
  downloading cannot start the game again and cannot hold the shutdown past the grace
  period. A resume — which applies the cached configuration without a configure call from
  the controller — retries a transient failure 3 times with an exponential backoff.
- **A configuration that delivers no `config.ini` gets a default one.** The game always
  starts with `-c <saveDir>/config.ini`, so `spec.gameConfig` may be omitted entirely: the
  agent writes a minimal `config.ini` whose `write-data` points at the save directory,
  which keeps the autosaves on the save volume.
- **The agent serves a TCP liveness probe on the RPC port 9550**: it listens before the game
  starts and stays up during a drain, so a refused connection means a stuck agent. Three
  failures in a row make the kubelet restart the container.
- **The chart's defaults are installable**: `image.tag: latest` (the release pushes
  `master`, `latest` and the `v*` tags), `config.leaderElection: true` together with
  `deploymentStrategy: RollingUpdate`, so exactly one of the two manager Pods of a rollout
  reconciles. `imagePullSecrets` holds the Secrets of the manager Pod and
  `spec.agent.imagePullSecrets` the ones of the agent Pods: an image in a private registry
  needs **both**.
- **`leaveTimeoutSeconds: 0` waits for nobody** and sends SIGINT at once. Only a
  `FactorioServer` without the field gets the default of 60 seconds; the CRD default keeps
  the minimum at 0.
- **A dirty tree gets a timestamped `VERSION`.** The Makefile appends the build time when
  `git describe` reports `-dirty`, so two builds of the same commit with different
  uncommitted changes carry different versions; without it the manager would take the new
  agent for a build of its own version and skip the in-place upgrade.
- **The manager RBAC holds no `create`/`delete` on `FactorioServer`s** — the user owns the
  CR — but it holds `delete` on Pods, which recreates the agent Pod of a changed pod
  template, the whole NetworkPolicy rule set, and `create` on Secrets for the RPC token
  Secret of each server; `configmaps` stays at `get;list;watch`.


**Configuration.** Both binaries are Cobra commands whose only flag is `--config <path>`, the
YAML file they read with Viper (`cmd/manager/main.go`, `cmd/agent/main.go`). The keys are
lowerCamelCase, and `examples/manager.yaml` and `examples/agent.yaml` list all of them with
their defaults: a key the file leaves out keeps the default, while a file that is missing or
does not parse stops the process with exit 1. `log.development` picks the console encoding over
JSON and `log.level` the minimum level (debug, info, warn, error, ...). Neither binary reads
environment variables for its configuration.

- The manager reads `/etc/koujouo/manager.yaml`, which the deployment mounts from a ConfigMap.
  Its `controllerImage` names the image the agent StatefulSets run, so it usually holds the
  manager's own image; empty leaves the controller unable to create a StatefulSet.
- The agent reads `/etc/koujouo/agent.yaml`, which the Nix image holds (flake.nix writes it with
  `pkgs.writeText`). The agent Pod mounts no ConfigMap. `tokenFile` names the mounted RPC token
  file (default `/var/run/koujouo/token`), the one input the controller writes as a volume
  instead of sending it over the RPC. The agent reads it on every `configure`, `stop` and
  `upgrade` call; the file wins over `Options.Token`, which only the tests set.

## Nix Development Environment

All development tools come from the Nix flake dev shell. Do NOT install
them with `go install` or download them manually (the Go tools of go.mod are
the exception: they run as `go tool <name>`, never installed):

```bash
nix develop   # go, golangci-lint, gitleaks, kubebuilder, kind, kubectl, helm,
              # yq, setup-envtest, controller-gen, capnp
```

- `controller-gen` is provided by the nixpkgs `kubernetes-controller-tools` package
- `capnp`, the Cap'n Proto compiler, comes from the nixpkgs `capnproto` package. Its Go plugin
  `capnpc-go` is a tool of go.mod instead (the `tool` directive, at the version of
  `capnproto.org/go/capnp/v3` in the same file): the flake neither builds nor installs it, and
  `make generate-capnp` runs it as `go tool capnpc-go` from `rpc/generate.go`. Bump the version
  with `go get -tool capnproto.org/go/capnp/v3/capnpc-go@<version>`
- `nix fmt` formats Nix files with alejandra
- `nix build .#manager` builds the `manager` and the `agent` binaries (`cmd/manager`, `cmd/agent`)
  with [go2nix](https://github.com/numtide/go2nix); `nix build .#dockerImage` (the default package)
  builds the OCI image `ghcr.io/ushitora-anqou/koujouo:latest`
- The image is built with `dockerTools.buildImage` (static binaries, nonroot user `65532`, CA certs
  and tzdata); there is no Dockerfile. `/manager` is the entrypoint and `/kojouo/agent` the agent.
  The image also holds `/etc/koujouo/agent.yaml`, the configuration the agent reads (the manager
  takes its own from the ConfigMap the deployment mounts, so the image holds none).
  The Factorio binary the agent downloads is dynamically linked, so the image links the glibc
  loader the binary looks for: `/lib64/ld-linux-x86-64.so.2` on x86_64,
  `/lib/ld-linux-aarch64.so.1` on aarch64
- Both binaries get the build stamped into `internal/version.Agent` (`-X`): the short git revision,
  also over uncommitted changes. The Makefile bakes the same value into locally built binaries
  (`VERSION`); a dirty tree gets the build time appended (`<rev>-dirty-<unixtime>`), so two builds
  of the same commit with different changes differ. Without it the manager would take the new
  agent for a build of its own version and skip the in-place upgrade. Releases are built from a
  clean commit, so only development builds carry the suffix.

**The Go build is go2nix without a lockfile.** flake.nix defines `goEnv` with `go2nix.lib.mkGoEnv`
and builds `manager` with `goEnv.buildGoApplication`: there is no `vendorHash`, and the module
hashes are derived from `go.sum` while Nix evaluates. A change of `go.mod`/`go.sum` alone (a
Dependabot pull request) therefore builds as it is; do not add a `go2nix.toml` lockfile (it would
have to be regenerated on every bump and would fail the build until someone does).

**go2nix's builder needs its Nix plugin.** The evaluator must provide
`builtins.resolveGoPackages`, which `libgo2nix_plugin.so` registers:
- inside the dev shell nothing to do: the `shellHook` appends `plugin-files = <plugin>` to
  `NIX_CONFIG`, so `nix build .#manager`, `nix build .#dockerImage` and `make docker-build` just
  work there
- outside of it, load the plugin per command:
  `nix build --option plugin-files "$(nix build --no-link --print-out-paths .#go2nix-nix-plugin)/lib/nix/plugins/libgo2nix_plugin.so" .#manager`
  (`make docker-build` passes the option itself, so it works outside the dev shell too). Without
  the plugin evaluation stops at `error: attribute 'resolveGoPackages' missing`

**The flake source only contains git-tracked files.** Commit (or at least
`git add`) your changes before building the manager or the docker image,
otherwise your edits are silently excluded from the build.

## Critical Rules

### Never Edit These (Auto-Generated)
- `charts/koujouo/templates/koujouo.anqou.net_factorioservers.yaml` - from `make manifests`
- `charts/koujouo/templates/manager-role.yaml` - from `make manifests`
- the webhook manifests of the chart - from `make manifests` (once a webhook exists)
- `**/zz_generated.*.go` - from `make generate`
- `rpc/koujouo.capnp.go` - from `make generate-capnp`; edit `rpc/koujouo.capnp` instead
- `PROJECT` - from `kubebuilder [OPTIONS]`
- `flake.lock` - from `nix flake update`

### Never Remove Scaffold Markers
Do NOT delete `// +kubebuilder:scaffold:*` comments. CLI injects code at these markers.

### Keep Project Structure
Do not move files around. The CLI expects files in specific locations.

### Never Commit a Secret
`make test` begins with `make secrets`, which scans the whole git history with gitleaks and fails
on a finding. Keep the full history in any checkout (`fetch-depth: 0`, as `ci.yml` does). Track
placeholders only, as `examples/secret.yaml` does; put real credentials in what you apply, never
in a tracked file.

### Always Use CLI Commands
Always use `kubebuilder create api` and `kubebuilder create webhook` to scaffold APIs, controllers
and webhooks. Do NOT create those files manually. The agent and the RPC schema are hand-written;
they have no scaffolder.
Run `kubebuilder` from inside the dev shell (`nix develop`).

### Manager and Agent Invariants

The manager and the agent are upgraded separately: the agent of a running game keeps its old
binary until the new manager replaces it. Keep these couplings intact:

- **Image layout**: `/manager` (entrypoint) and `/kojouo/agent` are referenced in `flake.nix`,
  `charts/koujouo/templates/deployment.yaml` (the image), `internal/controller/resources/statefulset.go`
  (`AgentCommand`) and `internal/controller/agentupgrade.go` (`defaultAgentBinaryPath`). Rename them
  together.
- **Controller image**: the `controllerImage` key of the manager configuration (the ConfigMap
  mounted at `/etc/koujouo/manager.yaml`) names the image the agent StatefulSets run; it normally
  holds the manager's own image. Empty leaves the controller unable to create a StatefulSet
  (`errControllerImageUnset`). The chart writes that ConfigMap itself: it sets `controllerImage` to
  its `image` values, so the manager Deployment and the agents always run the same image. The
  manager container must stay `containers[0]` of the manager Deployment.
- **In-place agent upgrade**: the build stamps the same version into both binaries
  (`internal/version.Agent`). An agent that reports another version receives `/kojouo/agent` from
  the manager and `execve(2)s` it without stopping the game. Consequences:
  - The StatefulSet uses the `OnDelete` update strategy. Do not switch to `RollingUpdate`: every
    image rollout would restart the game.
  - A new manager first calls `status` and `upgrade` on an agent that still runs the previous
    build. Changes to `rpc/koujouo.capnp` must stay wire-compatible: add fields, enumerants and
    methods with numbers higher than every existing one, and never change a number, a type or a
    default value (Cap'n Proto, "Evolving Your Protocol").
  - Because the strategy is `OnDelete`, a template change alone reaches no running Pod. The
    controller compares the `koujouo.anqou.net/template-hash` annotation of the Pod with the
    hash of the desired template (`resources/podTemplateHash`) and deletes a Pod of another
    hash, so the StatefulSet recreates it. The hash covers the owned pod template fields with
    the container image blanked out: an image change must not recreate the Pod, and it does
    not (see "How It Works"). A field the controller owns that is left out of the hash is
    never repaired on a running Pod.
- **Restricted Pod Security**: the agent Pod satisfies the `restricted` profile, which the e2e
  namespaces enforce: non-root (the image runs as uid `65532`, `fsGroup` 65532), read-only root
  filesystem, no privilege escalation, all capabilities dropped, `RuntimeDefault` seccomp. Writes
  go to the two volumes only. The Pod also carries `automountServiceAccountToken: false`: the
  agent never talks to the API server, so it gets no token.
- **Stop budgets**: `terminationGracePeriodSeconds` is 900 (`resources/statefulset.go`). A drain
  waits up to `leaveTimeoutSeconds` (at most 300) and the save up to 5 more minutes before the
  kubelet may kill the Pod; `cmd/agent/main.go` bounds the agent shutdown with a 9-minute
  deadline. Re-check the others when you change one of them. `leaveTimeoutSeconds: 0` waits for
  nobody: the drain sends SIGINT at once, and only a `FactorioServer` without the field is
  defaulted.
- **The agent bounds its own shutdown and resume**: `Shutdown` raises the `terminating` flag
  before it waits for the lifecycle lock, and that wait carries a deadline, so a configure that
  is still downloading never starts the game again and never holds the shutdown past the grace
  period. A resume (the cached configuration applied without a `configure` call) retries a
  transient failure 3 times with an exponential backoff, because the controller never repeats
  the call a resume replaces.
- **Configuration over RPC only**: the controller reads the referenced ConfigMaps and Secrets and
  passes their values in the `Config` struct; the agent Pod mounts neither. The controller reads
  them through `APIReader` (the API server directly, no cache): they are few, need no
  cluster-wide watch, and the manager then caches no Secret. The agent caches the
  last configuration in `agent-state.json` (mode 0600) on the save volume to resume after a Pod
  restart. Never mount these objects into the Pod, and never print that file into logs, docs or
  test output: it holds the delivered secrets and the RCON password in clear text. The RPC token
  Secret is the exception: the controller creates it itself, it carries no user credential, and
  it reaches the agent as the `tokenFile` mount.
- **Reconciler concurrency**: `defaultMaxConcurrentReconciles` is 8. A configure call takes
  minutes (drain plus download) and the reconcile waits for it, so a single worker would hold
  back every other `FactorioServer` that long. A status call is bounded by 30 seconds
  (`defaultAgentStatusTimeout`), so an agent that keeps the connection open without answering
  costs one poll instead of the whole wait.

### E2E Tests Require an Isolated Kind Cluster
`make test-e2e` works on the dedicated [Kind](https://kind.sigs.k8s.io/) cluster
`koujouo-test-e2e` (`KIND_CLUSTER`): it creates the cluster when missing and deletes it after a
successful run — also a cluster of that name that already existed. A failed run leaves the
cluster behind for debugging; `make cleanup-test-e2e` removes it. Never point it at your “real”
dev/prod cluster. Kind is available in the dev shell; see "Testing & Development" for what the
suite needs.

## After Making Changes

**Before committing** (the same two commands `ci.yml` runs):
```
make all               # fmt generate manifests lint-config lint test
make check-uncomitted  # Fails unless the regeneration leaves nothing to commit
```

`make all` does not run the e2e suite: it regenerates the code and the chart, lints and runs the
unit and envtest tests, and, inside `test`, the gitleaks scan of the git history. Run the e2e
suite with `make test-e2e` locally or let the E2E workflow (`.github/workflows/e2e.yml`) run it —
it needs Docker, a Kind cluster and up to 60 minutes. `check-uncomitted` fails on any uncommitted
change by design, so commit (or at least `git add`) your work first and run it to learn whether
the generated files are up to date.

`make release` builds the two release artifacts from a local tree and is not part of either
command: `make release-image` (the image of this tree, pushed to `IMG`) and `make release-chart`
(`helm lint`, then `helm package` into `dist/`). See "Release Targets" under "Distribution
Options".

**After editing `*_types.go` or markers:**
```
make manifests  # Regenerate the CRD and the manager ClusterRole of the chart from markers
make generate   # Regenerate DeepCopy methods
```

**After editing `rpc/koujouo.capnp`:**
```
make generate-capnp   # Regenerate rpc/koujouo.capnp.go (`make generate` runs it too)
```
Commit the generated file with the schema, and update both sides of the protocol — the manager
(`internal/controller`) and the agent (`internal/agent`) — in the same change. Mind the
wire-compatibility rule under "Manager and Agent Invariants".

**After editing `*.go` files:**
```
make lint-fix   # Auto-fix code style
make test       # Run the secret scan and the tests
```

**After editing `go.mod`/`go.sum`:**
Nothing to update in flake.nix: go2nix derives the module hashes from `go.sum`
(see "Nix Development Environment"). Run `go mod tidy` and build to confirm.

**After editing `flake.nix`:**
```
nix fmt .       # Format the flake
nix build .#dockerImage   # Verify it still builds (load the go2nix plugin as described above)
```

## CI

GitHub Actions workflows install Nix via `cachix/install-nix-action`
and work inside the dev shell using
[`nicknovitski/nix-develop`](https://github.com/nicknovitski/nix-develop), which
exports the dev shell environment to subsequent steps. All actions are pinned by
commit SHA. Dependabot (`.github/dependabot.yml`) updates Go modules and the
pinned actions weekly.

The action pins upstream Nix 2.34.9 with `install_url` (the installer of that
release) and `install_sha256` (the published hash of the installer), because the
go2nix Nix plugin is built against nixpkgs' Nix 2.34.9 and needs that exact
ABI. Keep the pinned version in step with `nixVersions.nix_2_34` of the nixpkgs
input in `flake.lock`.

There are two CI workflows. `ci.yml` runs the gate a commit has to pass, `make all` plus
`make check-uncomitted`; `e2e.yml` runs the Kind end-to-end suite, `make test-e2e`, on its own,
because the suite needs Docker and up to 60 minutes.

`ci.yml` runs on every push to `master` and on every pull request: one job on
`ubuntu-latest` that does `go mod tidy`, then `make all`, then `make check-uncomitted` —
the same two commands a commit has to pass locally. The push trigger names `master` only:
a branch of this repository with an open pull request would otherwise run the job twice,
once for the push and once for the pull request. The checkout fetches the full history
(`fetch-depth: 0`): `make test` scans it for secrets with gitleaks, and a shallow clone would
hide every commit but the latest. The job has `timeout-minutes: 30`: `make all` lints and runs
the unit and envtest suites, which take minutes.

- `make all` is the pre-commit gate:
  `fmt generate manifests lint-config lint test`. It regenerates the DeepCopy code, the
  capnp code and the chart, verifies the golangci-lint configuration, runs `golangci-lint` and
  `helm lint`, then the unit and envtest suite (`test`, which scans the git history for secrets
  first). It has no `build` step on purpose: `test` compiles the manager and the agent. It has no
  e2e step either: the Kind end-to-end suite (`test-e2e`) needs Docker, builds the image with Nix
  and takes up to 60 minutes, so `e2e.yml` runs it instead.
- `make check-uncomitted` (one "m", as the target is spelled) runs `fmt generate manifests` and
  then fails when `git status --porcelain` is not empty: a generated file whose change was not
  committed, an untracked file, and an unrelated uncommitted change all fail it. In CI
  `go mod tidy` runs before `make all`, so a `go.mod`/`go.sum` that tidy would rewrite fails here
  too.
- `e2e.yml` runs `go mod tidy`, then `make test-e2e`, on every pull request, on every push to
  `master`, nightly at 18:00 UTC (03:00 JST) and on a manual dispatch. It keeps the log of the
  suite as an artifact when it fails (`tee e2e.log`, `actions/upload-artifact`), dumps the state of
  the cluster (`kubectl get all -A`, the events, the manager log, the Pods), and always deletes the
  Kind cluster `koujouo-test-e2e`: a runner is thrown away, so a failed run leaves no cluster to
  debug. The job has `timeout-minutes: 90`: `E2E_TIMEOUT` is 60m, and the image builds and the
  cluster need the rest. `make test-e2e` receives `DISCORD_WEBHOOK_URL` from a repository secret of
  the same name when one is configured; without it the suite runs without Discord notifications.
- `release.yml` is separate: it builds `nix build .#dockerImage` and publishes it to GHCR (see the
  next paragraph). The image jobs do not go through the Makefile, so a change of a `make` target
  cannot break them; only its `chart` job runs `make release-chart`.

CI therefore also compares the regenerated files with the committed ones: run the `make` targets
under "After Making Changes" yourself and commit their output.

`release.yml` publishes the image on a push to `master` and on a `v*` tag: a `master` push tags
`master` and `latest`, a `v*` tag tags `vX.Y.Z` only, so an old tag cannot move `latest`. It runs
four jobs. `meta` takes the repository name from the flake (`dockerImage.imageName`) and lets
`docker/metadata-action` compute the final tags (including `version`, the tag as it is written).
`build` runs once per platform — `linux/amd64` on `ubuntu-latest`, `linux/arm64` on
`ubuntu-26.04-arm` — and `nix build .#dockerImage` resolves to the runner's own system there, so
each architecture is built natively (no cross-compilation or emulation); each run pushes
`<version>-linux-<arch>` and uploads its digest. `merge` assembles the per-arch digests into the
final multi-arch tags with `docker buildx imagetools create`, which needs buildx pinned to v0.31.0
(`imagetools create` is broken on GHCR since v0.31.1, docker/buildx#3708). The image jobs run
`nix build` rather than a `make` target; `meta` and `build` enter the dev shell because evaluating
the flake needs go2nix's Nix plugin, which the `shellHook` exports through `NIX_CONFIG`. `merge`
only assembles the digests with Docker and needs neither.

On a `v*` tag (only: a `master` push releases no chart) a fourth job, `chart`, adds the Helm chart
to the release: it strips the `v` of `meta.outputs.version` (`v0.1.0` → `0.1.0`), runs `helm lint`
and `make release-chart RELEASE_VERSION=<version>`, and attaches `dist/koujouo-<version>.tgz` to
the GitHub Release of the tag with `gh release create` (`gh release upload --clobber` when the
release exists). The chart version and its `appVersion` are both the tag without the `v`, while
the image keeps the `v` (the tag metadata-action reports). The job carries
`permissions: contents: write` (the top-level `packages: write` stays as it is) and no step
continues on failure: a release missing its chart fails the run. The image is pushed by
`build`/`merge`, which `chart` does not wait for (`needs: [meta]`), so a failed run can leave the
image published without a GitHub Release; the chart publishes to no other place (no GHCR chart
repository), so the archive has a single source.

## CLI Commands Cheat Sheet

### Create API (your own types)
```bash
kubebuilder create api --group <group> --version <version> --kind <Kind>
```

### Deploy Image Plugin (scaffold to deploy/manage ANY container image)

Generate a controller that deploys and manages a container image (nginx, redis, memcached, your app, etc.):

```bash
# Example: deploying memcached
kubebuilder create api --group example.com --version v1alpha1 --kind Memcached \
  --image=memcached:alpine \
  --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Scaffolds good-practice code: reconciliation logic, status conditions, finalizers, RBAC. Use as a reference implementation.


### Create Webhooks
```bash
# Validation + defaulting
kubebuilder create webhook --group <group> --version <version> --kind <Kind> \
  --defaulting --programmatic-validation

# Conversion webhook (for multi-version APIs)
kubebuilder create webhook --group <group> --version v1 --kind <Kind> \
  --conversion --spoke v2
```

### Controller for Core Kubernetes Types
```bash
# Watch Pods
kubebuilder create api --group core --version v1 --kind Pod \
  --controller=true --resource=false

# Watch Deployments
kubebuilder create api --group apps --version v1 --kind Deployment \
  --controller=true --resource=false
```

### Controller for External Types (e.g., from other operators)

Watch resources from external APIs (cert-manager, Argo CD, Istio, etc.):

```bash
# Example: watching cert-manager Certificate resources
kubebuilder create api \
  --group cert-manager --version v1 --kind Certificate \
  --controller=true --resource=false \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

**Note:** Use `--external-api-module=<module>@<version>` only if you need a specific version. Otherwise, omit `@<version>` to use what's in go.mod.

### Webhook for External Types

```bash
# Example: validating external resources
kubebuilder create webhook \
  --group cert-manager --version v1 --kind Issuer \
  --defaulting \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

## Testing & Development

```bash
nix develop       # Enter the dev shell first
make secrets      # Scan the git history for committed secrets with gitleaks
make test         # Secret scan + agent unit tests + controller envtest suite (skips test/e2e)
make test-e2e     # End-to-end on a dedicated Kind cluster, with the real Factorio binary
make run          # Run the manager on the host (uses current kubeconfig context)
make release      # Release artifacts: the image pushed to IMG and the chart .tgz in dist/
make release-image  # Just the image: docker-build docker-push IMG=<registry>/koujouo:<tag>
make release-chart  # Just the chart: helm lint, then helm package into dist/
```

Tests come in three layers:

- **Agent unit tests** (`internal/agent/*_test.go`): plain `go test`, no cluster. Everything
  external goes through `agent.Options` (`HTTPClient`, `ExecBinary`, `DialRcon` and the
  intervals), so the tests use `httptest` servers, fake RCON connections and fake game
  processes.
- **Controller tests** (`internal/controller/`): Ginkgo + envtest (real K8s API + etcd, no
  agent Pod). The reconciler's `Dialer` is replaced by an in-process fake agent
  (`fakeagent_test.go`) that speaks real Cap'n Proto RPC over `net.Pipe` or hands out its
  capability directly; the agent binary of the upgrade tests comes from a temporary file
  (`AgentBinaryPath`). Check `suite_test.go` for the setup.
- **E2E** (`test/e2e/`): Kind, the Nix-built image and the real game; see below.

`make test` begins with `make secrets`, which scans the whole git history with gitleaks; it
therefore needs a full checkout (CI passes `fetch-depth: 0` to `actions/checkout`). It resolves
envtest binaries via `setup-envtest` (downloaded on demand);
the `ENVTEST_K8S_VERSION` is derived from `k8s.io/api` in go.mod. `make test`, `make build`,
`make run` and `make test-e2e` regenerate the manifests and the code first
(`manifests generate fmt`), so they can leave modified generated files behind.

`make run` runs the manager with `examples/manager.yaml`, so it cannot build a StatefulSet while
`controllerImage` there is empty: set it to the published agent image first. The manager upgrades
agents like a deployed manager does, by streaming `/kojouo/agent` from its own filesystem — a
file that only exists inside the image.

The configuration loaders of both binaries are covered by plain unit tests
(`cmd/manager/config_test.go`, `cmd/agent/config_test.go`): they check the defaults, a full file
and the errors of a missing or malformed one.

Beyond the loaders, the following behaviour has a test of its own; keep them in step with the
code:

| Area | Test |
| --- | --- |
| `shutdown` absorbs the closed listener and joins the serve error; `waitForSignal` returns a server error, the error of a game that exited on its own and no error on a signal | `cmd/agent/main_test.go` |
| `Suspended` is cleared when a resumed server reports again (`derivePhase` and the `sync*Status` helpers) | `internal/controller/status_internal_test.go` |
| `callAgentStop` runs under `context.WithoutCancel`: the reconcile deadline does not end it | `internal/controller/agentstop_internal_test.go` |
| The NetworkPolicy ports, its manager-namespace source, its survival of a suspension and its absence without `ManagerNamespace` | `factorioserver_controller_test.go`, "When reconciling the agent NetworkPolicy" |
| A changed pod template (save PVC, `spec.agent.resources`) recreates the Pod, an image-only change does not, and the annotations the controller does not own survive | `factorioserver_controller_test.go`, "When the agent Pod runs an outdated pod template" |
| The RPC token Secret of a server: its creation with an owner reference, the unchanged token on the next reconcile, a new token only after a deletion, and the volume and read-only mount of the pod template (also repaired on an existing StatefulSet) | `factorioserver_controller_test.go`, "When reconciling the agent token Secret" and "When the agent is reachable" |
| The RPC token in the calls: configure, stop and upgrade carry the token of the Secret, while `status` answers without one; a wrong token is refused by configure, stop and upgrade, and the agent re-reads the token file on every call | `factorioserver_controller_test.go` ("When the agent is reachable", "When suspending the server", "When the agent runs another version"), `internal/agent/agent_test.go` (`TestAgentRejectsAWrongToken`, `TestAgentReadsTheTokenFile`) |
| Suspension: the stop call, the StatefulSet and PVC end state, the unreachable agent (the StatefulSet stays), the unready Pod (no stop) and the stop that outlives the reconcile deadline | `factorioserver_controller_test.go`, "When suspending the server" |
| A restarted Pod: the server turns `Pending` while the crashed agent is gone and continues with the agent that resumed the cached configuration, which needs no `configure` | `factorioserver_controller_test.go`, "When the agent crashed and the Pod restarted" |
| A referenced ConfigMap or Secret that changes reconfigures the agent on the next reconcile (and only then), and a reference that cannot be resolved yet is retried after `RetryInterval` instead of failing the reconcile | `factorioserver_controller_test.go`, "When a referenced ConfigMap or Secret changes" and "When a spec reference cannot be resolved" |
| Long game output lines, a webhook that does not answer, an overlong stream that must still be drained | `internal/agent/logs_test.go` |
| The default `config.ini` of a configuration without `spec.gameConfig`, and the delivered one winning | `internal/agent/config_test.go` |
| `Shutdown` returning at its deadline while a configure holds the lock, the resume retry of a transient failure, a game that exits on its own reaching `Fatal()` and a graceful stop staying out of it | `internal/agent/agent_test.go` |
| A failing S3 endpoint and an empty saves directory: the backup is logged and skipped, the loop still ends with its context | `internal/agent/backup_test.go` |

**E2E suite** (`test/e2e/`, build tag `e2e`)

The files carry `//go:build e2e`: `make test` skips the package and `make lint` does not see
it. `BeforeSuite` builds two images, loads both into Kind and installs the chart into
`koujouo-system` with `helm upgrade --install koujouo charts/koujouo -n koujouo-system --set
image.repository=... --set image.tag=...`; the suite creates and labels that namespace itself,
since the chart makes none. The first image is the one of the tree
(`make docker-build IMG=example.com/koujouo:v0.0.1`); the second is the same source with the
version `e2e-old` stamped into both binaries (`make docker-build-e2e-old
IMG=example.com/koujouo:e2e-old`, `dockerImageE2EOld` of flake.nix), and the release *starts*
with it. Two containers then run against it: the scaffolded `Manager` metrics checks and
`FactorioServer` (`Ordered`), which walks one server through download and host → inspect the
save volume → upgrade the agent in place → change the game version → suspend → resume from
the same save. The scenarios assert on the `FactorioServer` status and on the agent log, which
`utils.FollowPodLogs` keeps in memory after the Pod is deleted.

- The upgrade scenario is what covers the in-place agent upgrade of "Manager and Agent
  Invariants": it `helm upgrade`s the release to the image of this build, which rolls the
  manager Deployment but leaves the agent Pod of the `OnDelete` StatefulSet, and then asserts
  that the agent logs `Execing the new agent binary` and `Resumed the game after an agent
  upgrade`, that the Pod UID and the container restart count did not change, that `Started
  Factorio` appears once, and that the StatefulSet pod template names the new image.
- Needs Docker, outbound access to `factorio.com` (the agent downloads Factorio 2.1.24 and
  2.1.25) and up to `E2E_TIMEOUT` (60 minutes by default); cert-manager is not installed.
- The image comes from the Nix flake: stage or commit your changes first, otherwise the suite
  tests the last committed build against CRDs generated from the working tree.
- `AfterSuite` uninstalls the release with `helm uninstall`, then deletes the CRD — it carries
  `helm.sh/resource-policy: keep`, so the uninstall keeps it — and the namespace last.
  `BeforeSuite` deletes a leftover CRD as well: one a failed run left behind makes the install
  fail on the ownership. The namespaces and the metrics ClusterRoleBinding are created
  idempotently, so a suite re-run on the cluster of a failed run works.
- `DISCORD_WEBHOOK_URL`, when set, is handed to the FactorioServer as a Secret; without it the
  suite runs without Discord notifications. Never put a webhook URL into the suite.
- A failed scenario dumps the objects, the events, the manager log and the agent log to the
  Ginkgo output. The volume inspector (`utils.RunVolumeInspector`) lists file names and the
  save file's fingerprint only, because `agent-state.json` holds secrets.

## Deployment Workflow

```bash
# 0. Enter the dev shell and make sure your changes are git-tracked
nix develop
git add -A  # the flake only sees git-tracked files when building the image

# 1. Regenerate manifests
make manifests generate

# 2. Build & push the image, then install the chart with it
export IMG=<registry>/koujouo:tag
make docker-build docker-push IMG=$IMG  # Image built by Nix, loaded into the local runtime, re-tagged to ${IMG}
helm upgrade --install koujouo charts/koujouo -n koujouo-system --create-namespace \
  --set image.repository=<registry>/koujouo --set image.tag=tag

# 3. Create a server: the PVC, the ConfigMap and the Secret it refers to, then the FactorioServer
kubectl apply -n <namespace> -f examples/pvc.yaml
kubectl apply -n <namespace> -f examples/configmap.yaml -f examples/secret.yaml
kubectl apply -n <namespace> -f examples/factorioserver.yaml

# 4. Debug
kubectl logs -n koujouo-system deployment/koujouo-controller-manager -c manager -f
kubectl logs -n <namespace> <factorioserver-name>-0 -c agent -f   # agent log with the game output
kubectl get factorioserver                                        # short name: fsrv
```

The chart in `charts/koujouo/` is the only way to deploy the operator; there is no Kustomize
tree any more. `make manifests` writes the CRD and the manager ClusterRole straight into
`charts/koujouo/templates/`, so a change of the markers shows up as a chart change. The chart
creates no Namespace: it installs into the namespace of the release (`koujouo-system` above,
which the e2e creates and labels with the `restricted` Pod Security Standards).

A `FactorioServer` refers to three objects that you create yourself (see `examples/`):
the save PVC (`.spec.savePersistentVolumeClaimName`), a ConfigMap with `config.ini` and the server
whitelist (`.spec.gameConfig`), and a Secret with the Discord webhook URL, the S3 credentials and,
for experimental or old builds, the Factorio download credentials. `examples/secret.yaml`
holds placeholders: substitute real values in what you apply, never in the tracked file.

### API Design

**Key markers for** `api/<version>/*_types.go`:

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"

// On fields:
// +kubebuilder:validation:Required
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:MaxLength=100
// +kubebuilder:validation:Pattern="^[a-z]+$"
// +kubebuilder:default="value"
```

- **Use** `metav1.Condition` for status (not custom string fields)
- **Use predefined types**: `metav1.Time` instead of `string` for dates
- **Follow K8s API conventions**: Standard field names (`spec`, `status`, `metadata`)

**koujouo conventions** (rationale in `docs/design.md` §3.1):
- Durations are `<name>Seconds` fields (`int32`), not `metav1.Duration` (`leaveTimeoutSeconds`)
- ConfigMaps and Secrets are referenced with `ConfigMapKeySelector` / `SecretKeySelector`; the
  controller reads the values and sends them over RPC — the agent Pod mounts none of them
- Abbreviations are spelled out (`savePersistentVolumeClaimName`); `URL` and `ID` stay uppercase
  per Kubernetes API conventions, and the `URL` fields carry `//nolint:tagliatelle` for that reason
- Conditions (`Ready`, `Progressing`, `Suspended`) are the source of truth. `status.phase` is
  derived from them in `internal/controller/status.go`: change the status through the
  `sync*Status` helpers there, never by setting the phase directly

### Controller Design

**RBAC markers in** `internal/controller/*_controller.go`:

```go
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
```

**Implementation rules:**
- **Idempotent reconciliation**: Safe to run multiple times
- **Re-fetch before updates**: `r.Get(ctx, req.NamespacedName, obj)` before `r.Update` to avoid conflicts
- **Structured logging**: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- **Owner references**: Enable automatic garbage collection (`SetControllerReference`)
- **Watch secondary resources**: Use `.Owns()` or `.Watches()`, not just `RequeueAfter`
- **Finalizers**: Clean up external resources (buckets, VMs, DNS entries)
- **Linting**: `.golangci.yml` enables every golangci-lint linter except `depguard`, `exhaustruct` (replaced by `exhaustruct_v5`), `gomodguard` (replaced by `gomodguard_v2`) and `wsl`; `exhaustruct_v5` ignores `k8s.io/*` and controller-runtime types
- **Linter exceptions**: Idiomatic scheme/test globals and other exceptions that cannot be removed carry `//nolint:<linter> // reason` directives — keep that style when adding new exceptions

**koujouo specifics:**
- `FactorioServerReconciler` takes its dependencies as fields (`Dialer`, `AgentBinaryPath`,
  `Recorder`, the intervals); zero values fall back to the production defaults and the tests
  substitute them. Keep new dependencies injectable the same way
- Only `cmd/agent`, `internal/agent` and `internal/controller` import the generated `rpc`
  package; keep the dependency on the protocol there
- `configure` and `stop` return only when the agent is done, and a drain waits up to
  `leaveTimeoutSeconds`. Both calls and the status patches that report their progress therefore
  run under `context.WithoutCancel`; only a controller shutdown interrupts them
- `syncStatefulSet` copies only the fields the controller owns into an existing StatefulSet. A
  new owned field needs handling there and in `containerDrifted` / `syncContainer`, or its drift
  is never repaired. A new owned **pod template** field also enters `podTemplateHash`
  (`resources/statefulset.go`), or a running Pod never picks it up
- The manager ClusterRole (`charts/koujouo/templates/manager-role.yaml`, generated from the
  markers) grants `get;list;watch;update;patch` on `factorioservers` but **no `create` or
  `delete`**: the `FactorioServer`s belong to the user. It grants `delete;get;list;watch` on
  `pods`, which recreates the agent Pod of a changed pod template, `get;list;watch` on
  `configmaps;secrets` and the whole NetworkPolicy rule set
- Events go through `k8s.io/client-go/tools/events` (`Recorder.Eventf`)

### Logging

**Follow Kubernetes logging message style guidelines:**

- Start from a capital letter
- Do not end the message with a period
- Active voice: subject present (`"Deployment could not create Pod"`) or omitted (`"Could not create Pod"`)
- Past tense: `"Could not delete Pod"` not `"Cannot delete Pod"`
- Specify object type: `"Deleted Pod"` not `"Deleted"`
- Balanced key-value pairs

```go
log.Info("Starting reconciliation")
log.Info("Created Deployment", "name", deploy.Name)
log.Error(err, "Failed to create Pod", "name", name)
```

**Reference:** https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines

### Webhooks
The project has no webhook today; scaffold one with the commands under "Create Webhooks" when it
needs one.
- **Create all types together**: `--defaulting --programmatic-validation --conversion`
- **When`--force`is used**: Backup custom logic first, then restore after scaffolding
- **For multi-version APIs**: Use hub-and-spoke pattern (`--conversion --spoke v2`)
  - Hub version: Usually oldest stable version (v1)
  - Spoke versions: Newer versions that convert to/from hub (v2, v3)
  - Example: `--group crew --version v1 --kind Captain --conversion --spoke v2` (v1 is hub, v2 is spoke)

### Learning from Examples

The **deploy-image plugin** scaffolds a complete controller following good practices. Use it as a reference implementation:

```bash
kubebuilder create api --group example --version v1alpha1 --kind MyApp \
  --image=<your-image> --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Generated code includes: status conditions (`metav1.Condition`), finalizers, owner references, events, idempotent reconciliation.

## Distribution Options

### Option 1: Helm Chart

```bash
# Install the chart shipped in this repository
helm upgrade --install koujouo charts/koujouo -n koujouo-system --create-namespace \
  --set image.repository=<registry>/koujouo --set image.tag=<tag>
```

**Key points:**
- `charts/koujouo/` holds the CRD, the RBAC, the configuration ConfigMap and the manager Deployment
- `icon` of `Chart.yaml` points at the raw URL of `charts/koujouo/icon.svg` on `master`: the icon is
  an asset of this repository and is not hosted anywhere else
- The CRD carries `helm.sh/resource-policy: keep`, so `helm uninstall` keeps it and the
  `FactorioServer`s it holds; delete the CRD yourself once none is left
- `helm package charts/koujouo` produces an archive for users without a checkout; `make
  release-chart` does the same with the version from `VERSION`, and a `v*` tag attaches it to the
  GitHub Release (see "Release Targets")
- `make lint` runs `helm lint charts/koujouo`; `make manifests` refreshes the generated parts of
  the chart

### Publish Container Image

```bash
export IMG=<registry>/<project>:<version>
make docker-build docker-push IMG=$IMG
```

`make docker-build` runs `nix build .#dockerImage`, loads the archive into
the local container runtime, and re-tags it to `${IMG}`. Remember that the
flake only builds from git-tracked files.

### Release Targets

| Target | What it does |
| --- | --- |
| `make release` | `release-image` and `release-chart`: the image pushed to `IMG` and the chart packaged |
| `make release-image` | `docker-build docker-push`: builds the image of this tree and pushes it to `IMG` |
| `make release-chart` | `helm lint`, then `helm package` into `dist/` as version `$(RELEASE_VERSION)` |

`RELEASE_VERSION` defaults to `VERSION` without its leading `v`
(`$(patsubst v%,%,$(VERSION))`), and both the chart version and its
`appVersion` are set from it (`helm package --version --app-version`, which
leaves `Chart.yaml` alone). `helm package` wants a semantic version, so pass one
explicitly when the default is a git revision: `make release-chart
RELEASE_VERSION=0.1.0`. `DIST_DIR` (default `dist`) moves the output; git
ignores it and the `*.tgz` archives.

A `v*` tag release is `release.yml`, not these targets: it publishes the image
to GHCR and attaches `dist/koujouo-<version>.tgz` to the GitHub Release, which
needs `gh` and a token, so the targets above stop at the artifacts.

## References

### Project Documents
- **README.md**: behaviour of the operator, getting started, known limitations
- **docs/design.md**: library choices, Nix/build integration, CRD and capnp schemas, test plan, open concerns (Japanese)

### Essential Reading
- **Kubebuilder Book**: https://book.kubebuilder.io (comprehensive guide)
- **controller-runtime FAQ**: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md (common patterns and questions)
- **Good Practices**: https://book.kubebuilder.io/reference/good-practices.html (why reconciliation is idempotent, status conditions, etc.)
- **Logging Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines (message style, verbosity levels)
- **Nix Flakes**: https://nix.dev/concepts/flakes

### API Design & Implementation
- **API Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
- **Operator Pattern**: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
- **Markers Reference**: https://book.kubebuilder.io/reference/markers.html

### Tools & Libraries
- **controller-runtime**: https://github.com/kubernetes-sigs/controller-runtime
- **controller-tools**: https://github.com/kubernetes-sigs/controller-tools
- **Kubebuilder Repo**: https://github.com/kubernetes-sigs/kubebuilder
- **Cap'n Proto schema language**: https://capnproto.org/language.html ("Evolving Your Protocol" lists the compatible schema changes)
- **Cap'n Proto RPC protocol**: https://capnproto.org/rpc.html
- **go-capnp** (Go code generation, runtime and RPC): https://github.com/capnproto/go-capnp

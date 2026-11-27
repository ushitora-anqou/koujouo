{
  description = "koujouo - A Kubernetes operator for Factorio servers";

  inputs = {
    nixpkgs.url = "https://channels.nixos.org/nixpkgs-unstable/nixexprs.tar.zst";
    # go2nix builds the Go binaries one package per derivation. The build has
    # no lockfile: the module hashes come from go.sum, so a change of go.mod or
    # go.sum alone (a Dependabot update) builds without a hash to update. Its
    # default builder needs the go2nix Nix plugin loaded into the evaluator;
    # see `go2nix-nix-plugin` and the devShell below.
    go2nix = {
      url = "github:numtide/go2nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = inputs @ {
    nixpkgs,
    go2nix,
    ...
  }: let
    forAllSystems = f:
      builtins.mapAttrs (system: pkgs: f system pkgs) nixpkgs.legacyPackages;
  in {
    formatter = builtins.mapAttrs (system: pkgs: pkgs.alejandra) nixpkgs.legacyPackages;

    packages = forAllSystems (
      system: pkgs: let
        goEnv = go2nix.lib.mkGoEnv {
          inherit (pkgs) go callPackage;
          go2nix = go2nix.packages.${system}.go2nix;
          # The scope compiles the Go standard library with this environment,
          # so it has to be set here as well as on `manager` below: a stdlib
          # that ships runtime/cgo makes the linker pick the external linker,
          # which turns the binaries dynamically linked against the Nix glibc.
          # `0` keeps them static, as the previous buildGoModule build had
          # them (the agent binary is streamed to agents of other images).
          goEnv = {CGO_ENABLED = "0";};
        };

        # The Nix plugin that provides `builtins.resolveGoPackages`: the
        # lockfile-free build resolves the module hashes from go.sum with it
        # while Nix evaluates. The evaluator has to load it, either with
        # `--option plugin-files <path>` or, inside the dev shell, through
        # NIX_CONFIG (see devShells below).
        nixPlugin = go2nix.packages.${system}.go2nix-nix-plugin;

        # The interpreter path of the official Factorio binary the agent
        # downloads: dynamically linked, with the standard loader path of its
        # architecture and no run path.
        glibcLoader =
          if pkgs.stdenv.hostPlatform.isAarch64
          then "/lib/ld-linux-aarch64.so.1"
          else "/lib64/ld-linux-x86-64.so.2";
      in rec {
        # The version baked into the manager and the agent binaries: the
        # short git revision of the source tree, of the last commit also
        # when the tree has uncommitted changes. The controller compares
        # it with the version the agents report and pushes the agent
        # binary of its image to the agents of another build. The
        # Makefile bakes the same value into locally built binaries.
        buildVersion = let
          source = inputs.self;
        in
          if source ? shortRev
          then source.shortRev
          else if source ? dirtyShortRev
          then source.dirtyShortRev
          else "dev";

        # The controller manager and agent binaries.
        # Note: the flake source only contains git-tracked files, so commit (or
        # at least `git add`) your changes before building the image.
        manager = goEnv.buildGoApplication {
          pname = "koujouo";
          version = "0.0.1";
          src = inputs.self;
          subPackages = ["cmd/manager" "cmd/agent"];
          # Static binaries, as the previous distroless image had.
          CGO_ENABLED = 0;
          # The controller tests need envtest (a real API server and etcd),
          # which the Nix sandbox cannot run. `make test` runs them instead.
          doCheck = false;
          ldflags = [
            "-s"
            "-w"
            "-X github.com/ushitora-anqou/koujouo/internal/version.Agent=${buildVersion}"
          ];
        };

        # The go2nix Nix plugin, so that the Makefile and the CI workflows can
        # load it with `--option plugin-files` outside the dev shell.
        go2nix-nix-plugin = nixPlugin;

        # Configuration of the agent, which the agent reads from its default
        # path in the image. The manager takes its configuration from a
        # ConfigMap the deployment mounts at /etc/koujouo/manager.yaml
        # instead, so the image holds no manager configuration.
        agentConfig = pkgs.writeText "agent.yaml" ''
          listenAddress: ":9550"
          saveDir: /var/lib/koujouo
          cacheDir: /opt/koujouo
          log:
            development: false
            level: info
        '';

        # OCI image of the controller manager and agent, equivalent to the
        # previous distroless-based Dockerfile (static binaries, nonroot user).
        # The StatefulSet runs /kojouo/agent as PID 1; the manager Deployment
        # uses the image entrypoint /manager.
        dockerImage = pkgs.dockerTools.buildImage {
          name = "ghcr.io/ushitora-anqou/koujouo";
          tag = "latest";
          copyToRoot = [
            (pkgs.runCommand "koujouo-root" {}
              ''
                mkdir -p $out/kojouo
                ln -s ${manager}/bin/manager $out/manager
                ln -s ${manager}/bin/agent $out/kojouo/agent
                # The agent reads its configuration from the default path of
                # cmd/agent/main.go (--config points elsewhere).
                mkdir -p $out/etc/koujouo
                cp ${agentConfig} $out/etc/koujouo/agent.yaml
                # The official Factorio binary the agent downloads is
                # dynamically linked, with the standard interpreter path of
                # its architecture and no run path. The link to the Nix
                # glibc loader provides it; the loader then finds the other
                # glibc libraries in its own directory, which the image
                # holds under /nix/store.
                mkdir -p "$out$(dirname "${glibcLoader}")"
                ln -s ${pkgs.glibc}${glibcLoader} "$out${glibcLoader}"
              '')
            pkgs.cacert
            pkgs.tzdata
          ];
          config = {
            Entrypoint = ["/manager"];
            User = "65532:65532";
            WorkingDir = "/";
            Env = [
              "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
              "TZDIR=/share/zoneinfo"
            ];
          };
        };

        default = dockerImage;
      }
    );

    devShells = forAllSystems (
      system: pkgs: {
        default = pkgs.mkShell {
          # The capnp Go code generator plugin (capnpc-go) is not listed here:
          # it is the tool of go.mod and runs as `go tool capnpc-go` from the
          # `go:generate` directive of rpc/generate.go.
          packages = with pkgs;
            [
              go
              golangci-lint
              kubebuilder
              kind
              kubectl
              kubernetes-helm
              yq-go
              setup-envtest
              kubernetes-controller-tools
              capnproto
            ]
            ++ [inputs.self.packages.${system}.go2nix-nix-plugin];
          shellHook = ''
            # A lockfile-free go2nix build resolves the module hashes from
            # go.sum with `builtins.resolveGoPackages`, which the Nix plugin of
            # go2nix-nix-plugin provides. The evaluator loads it from the
            # `plugin-files` setting: append that line to NIX_CONFIG, keeping
            # whatever the caller already had there, so that `nix build
            # .#manager` works in this shell. Outside of it, pass `--option
            # plugin-files <path>`.
            export NIX_CONFIG="''${NIX_CONFIG:+$NIX_CONFIG$'\n'}plugin-files = ${inputs.self.packages.${system}.go2nix-nix-plugin}/lib/nix/plugins/libgo2nix_plugin.so"
          '';
        };
      }
    );
  };
}

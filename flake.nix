{
  description = "koujouo - A Kubernetes operator for Factorio servers";

  inputs = {
    nixpkgs.url = "https://channels.nixos.org/nixpkgs-unstable/nixexprs.tar.zst";
  };

  outputs = inputs: let
    forAllSystems = f:
      builtins.mapAttrs (system: pkgs: f system pkgs) inputs.nixpkgs.legacyPackages;
  in {
    formatter = builtins.mapAttrs (system: pkgs: pkgs.alejandra) inputs.nixpkgs.legacyPackages;

    packages = forAllSystems (
      system: pkgs: rec {
        # The capnp Go code generator plugin (not packaged in nixpkgs).
        # The `capnp` compiler itself comes from the nixpkgs `capnproto`
        # package and invokes this plugin via -ogo.
        capnpc-go = pkgs.buildGoModule {
          pname = "capnpc-go";
          version = "3.1.0-alpha.2";
          src = pkgs.fetchFromGitHub {
            owner = "capnproto";
            repo = "go-capnp";
            rev = "v3.1.0-alpha.2";
            hash = "sha256-BIrqiaF9rXcn6S+p/+Y6iq9g66oqH9F8qPTBOpC+Gc4=";
          };
          subPackages = ["capnpc-go"];
          vendorHash = "sha256-yk2+rFNRrMH2iWnuatPyvPLvo9p1bDAihmda6i2zNV4=";
          # Upstream tests shell out to `go build` inside a vendored tree,
          # which fails under buildGoModule's vendor layout.
          doCheck = false;
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
          ];
        };

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
        manager = pkgs.buildGoModule {
          pname = "koujouo";
          version = "0.0.1";
          src = inputs.self;
          vendorHash = "sha256-AJ0AJDtLnsyy8tFZysxL48F2m2PZJ7E7axOlYV5DRnA=";
          subPackages = ["cmd/manager" "cmd/agent"];
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
            "-X koujouo.anqou.net/project/internal/version.Agent=${buildVersion}"
          ];
        };

        # OCI image of the controller manager and agent, equivalent to the
        # previous distroless-based Dockerfile (static binaries, nonroot user).
        # The StatefulSet runs /kojouo/agent as PID 1; the manager Deployment
        # uses the image entrypoint /manager.
        dockerImage = pkgs.dockerTools.buildImage {
          name = "controller";
          tag = "latest";
          copyToRoot = [
            (pkgs.runCommand "koujouo-root" {}
              ''
                mkdir -p $out/kojouo
                ln -s ${manager}/bin/manager $out/manager
                ln -s ${manager}/bin/agent $out/kojouo/agent
                # The official Factorio binary the agent downloads is
                # dynamically linked, with the standard interpreter path
                # /lib64/ld-linux-x86-64.so.2 and no run path. The link to
                # the Nix glibc loader provides it; the loader then finds
                # the other glibc libraries in its own directory, which the
                # image holds under /nix/store.
                mkdir -p $out/lib64
                ln -s ${pkgs.glibc}/lib/ld-linux-x86-64.so.2 $out/lib64/ld-linux-x86-64.so.2
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
          packages =
            (with pkgs; [
              go
              golangci-lint
              kubebuilder
              kind
              kubectl
              kustomize
              setup-envtest
              kubernetes-controller-tools
              capnproto
            ])
            ++ [inputs.self.packages.${system}.capnpc-go];
        };
      }
    );
  };
}

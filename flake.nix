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
        # The controller manager binary.
        # Note: the flake source only contains git-tracked files, so commit (or
        # at least `git add`) your changes before building the image.
        manager = pkgs.buildGoModule {
          pname = "koujouo-manager";
          version = "0.0.1";
          src = inputs.self;
          vendorHash = "sha256-DNPKehuZlrPqaeQYeFy+b+YZblcztV5K9JJPpXWzIr4=";
          subPackages = ["cmd"];
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
          ];
          postInstall = ''
            mv $out/bin/cmd $out/bin/manager
          '';
        };

        # OCI image of the controller manager, equivalent to the previous
        # distroless-based Dockerfile (static binary, nonroot user).
        dockerImage = pkgs.dockerTools.buildImage {
          name = "controller";
          tag = "latest";
          copyToRoot = [
            (pkgs.runCommand "manager-root" {}
              ''
                mkdir -p $out
                ln -s ${manager}/bin/manager $out/manager
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
          packages = with pkgs; [
            go
            golangci-lint
            kubebuilder
            kind
            kubectl
            kustomize
            setup-envtest
            kubernetes-controller-tools
          ];
        };
      }
    );
  };
}

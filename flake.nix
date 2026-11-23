{
  description = "koujouo - A Kubernetes operator for Factorio servers";

  inputs = {
    nixpkgs.url = "https://channels.nixos.org/nixpkgs-unstable/nixexprs.tar.zst";
  };

  outputs = inputs: {
    formatter = builtins.mapAttrs (system: pkgs: pkgs.alejandra) inputs.nixpkgs.legacyPackages;

    devShells = builtins.mapAttrs (system: pkgs: {
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
    }) inputs.nixpkgs.legacyPackages;
  };
}

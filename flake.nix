{
  description = "A very basic flake";

  inputs = {
    nixpkgs.url = "https://channels.nixos.org/nixpkgs-unstable/nixexprs.tar.zst";
  };

  outputs = inputs: {
    formatter = builtins.mapAttrs (system: pkgs: pkgs.alejandra) inputs.nixpkgs.legacyPackages;

    devShells =
      builtins.mapAttrs (system: pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            golangci-lint
            kubebuilder
          ];
        };
      })
      inputs.nixpkgs.legacyPackages;

    #packages = builtins.mapAttrs (system: pkgs: {
    #  hello = pkgs.hello;

    #  default = inputs.self.packages.${system}.hello;
    #}) inputs.nixpkgs.legacyPackages;
  };
}

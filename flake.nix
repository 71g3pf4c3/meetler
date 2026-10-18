{
  description = "meetler — self-hosted WebRTC conferencing (LiveKit SFU) with anonymous join";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      # web — статика клиента как derivation (просто копия web/ в store)
      mkWeb = pkgs: pkgs.runCommand "meetler-web" { } ''
        mkdir -p $out
        cp -r ${./web}/. $out/
      '';
    in
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        packages = {
          token-svc = pkgs.callPackage ./nix/token-svc.nix { };
          tgbridge = pkgs.callPackage ./nix/tgbridge.nix { };
          web = mkWeb pkgs;
          default = pkgs.callPackage ./nix/token-svc.nix { };
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [ go gopls livekit livekit-cli caddy nginx ];
        };

        formatter = pkgs.nixpkgs-fmt;
      })
    // {
      # NixOS-модуль — не зависит от system
      nixosModules.default = import ./nix/module.nix {
        inherit self;
        mkWeb = mkWeb;
      };
      nixosModules.meetler = self.nixosModules.default;
    };
}

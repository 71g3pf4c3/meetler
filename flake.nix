{
  description = "meetler — self-hosted WebRTC conferencing (LiveKit SFU) with anonymous join";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      # целевые системы: серверный проект, x86_64-linux достаточно
      systems = [ "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);

      # web — статика клиента как derivation (просто копия web/ в store)
      mkWeb = pkgs: pkgs.runCommand "meetler-web" { } ''
        mkdir -p $out
        cp -r ${./web}/. $out/
      '';
    in
    {
      packages = forAllSystems (system:
        let pkgs = import nixpkgs { inherit system; }; in
        {
          api = pkgs.callPackage ./nix/api.nix { };
          tgbridge = pkgs.callPackage ./nix/tgbridge.nix { };
          web = mkWeb pkgs;
          default = pkgs.callPackage ./nix/api.nix { };
        });

      devShells = forAllSystems (system:
        let pkgs = import nixpkgs { inherit system; }; in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [ go gopls livekit livekit-cli nginx ];
          };
        });

      formatter = forAllSystems (system: (import nixpkgs { inherit system; }).nixpkgs-fmt);

      # NixOS-модуль — не зависит от system
      nixosModules.default = import ./nix/module.nix {
        inherit self;
      };
      nixosModules.meetler = self.nixosModules.default;
    };
}

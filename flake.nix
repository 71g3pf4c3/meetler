{
  description = "meetler — self-hosted WebRTC conferencing (LiveKit SFU) with anonymous join";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      # целевые системы: серверный проект, x86_64-linux достаточно
      systems = [ "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);

      # web — статика клиента как derivation. app.js/critters.js версионируются
      # хэшем содержимого (?v=…): index.html (no-cache) ссылается на новый URL
      # при любом изменении кода — браузеры держат жёсткий immutable-кэш,
      # и при этом всегда получают свежую версию после deploy.
      mkWeb = pkgs: pkgs.runCommand "meetler-web" { } ''
        mkdir -p $out
        cp -r ${./web}/. $out/
        ph=$(sha256sum $out/poker.js | cut -c1-12)
        sh=$(sha256sum $out/audio.js | cut -c1-12)
        substituteInPlace $out/app.js \
          --replace-fail "from '/poker.js'" "from '/poker.js?v=$ph'" \
          --replace-fail "from '/audio.js'" "from '/audio.js?v=$sh'"
        ah=$(sha256sum $out/app.js | cut -c1-12)
        ch=$(sha256sum $out/critters.js | cut -c1-12)
        substituteInPlace $out/index.html \
          --replace-fail "href=\"/app.js\"" "href=\"/app.js?v=$ah\"" \
          --replace-fail "src=\"/app.js\"" "src=\"/app.js?v=$ah\"" \
          --replace-fail "src=\"/critters.js\"" "src=\"/critters.js?v=$ch\""
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

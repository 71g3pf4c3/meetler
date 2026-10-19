# server-sdk-go/v2 требует go >= 1.26.8; nixpkgs default go = 1.26.7 → берём go_1_27.
{ lib, buildGoModule, go_1_27 }:

(buildGoModule.override { go = go_1_27; }) {
  pname = "meetler-tgbridge";
  version = "0.1.0";

  src = lib.cleanSourceWith {
    src = ../tgbridge;
    filter = path: _type: let base = baseNameOf path; in
      base != "Dockerfile";
  };

  # пересчитай при изменении go.sum: nix build .#tgbridge выдаст ожидаемый хеш
  vendorHash = "sha256-xZZ3Wn1L4ge2FtzWvRbDwCJSgH44g+2yZDQdB+vMvb8=";

  ldflags = [ "-s" "-w" ];

  meta = with lib; {
    description = "Telegram → meetler (LiveKit) media bridge bot";
    license = licenses.asl20;
    mainProgram = "tgbridge";
  };
}

{ lib, buildGoModule }:

buildGoModule {
  pname = "meetler-token-svc";
  version = "0.1.0";

  src = lib.cleanSourceWith {
    src = ../token-svc;
    filter = path: _type: let base = baseNameOf path; in
      base != "Dockerfile";
  };

  # пересчитай при изменении go.sum: nix build .#token-svc выдаст ожидаемый хеш
  vendorHash = "sha256-6dEag5IvA2e/7aHktkEcSiJlZ+MaoCbtK1aasnXaSnQ=";

  ldflags = [ "-s" "-w" ];

  meta = with lib; {
    description = "Anonymous room-JWT issuer for meetler (LiveKit)";
    license = licenses.asl20;
    mainProgram = "token-svc";
  };
}

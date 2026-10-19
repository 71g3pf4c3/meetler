{ lib, buildGoLatestModule }:

buildGoLatestModule {
  pname = "meetler-token-svc";
  version = "0.1.0";

  src = ../token-svc;

  # Cache complete modules, so new package imports can reuse existing dependencies.
  proxyVendor = true;
  vendorHash = "sha256-0R8RHQqjsYHTt2gwBeytxZWMVYXcwtQl9pCP6WIdKtw=";

  meta = with lib; {
    description = "Anonymous room-JWT issuer for meetler (LiveKit)";
    license = licenses.asl20;
    mainProgram = "token-svc";
  };
}

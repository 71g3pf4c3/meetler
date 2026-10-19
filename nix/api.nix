{ lib, buildGoLatestModule }:

buildGoLatestModule {
  pname = "meetler-api";
  version = "0.1.0";

  src = ../api;

  # Cache complete modules, so new package imports can reuse existing dependencies.
  proxyVendor = true;
  vendorHash = "sha256-0R8RHQqjsYHTt2gwBeytxZWMVYXcwtQl9pCP6WIdKtw=";

  meta = with lib; {
    description = "meetler API for rooms, tokens and media";
    license = licenses.asl20;
    mainProgram = "api";
  };
}

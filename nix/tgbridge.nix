{ lib, buildGoLatestModule }:

buildGoLatestModule {
  pname = "meetler-tgbridge";
  version = "0.1.0";

  src = ../tgbridge;

  # Cache complete modules, so new package imports can reuse existing dependencies.
  proxyVendor = true;
  vendorHash = "sha256-NKzwYJ1cJbJ2MqGXwl4o0bJrI8CgrfkKyR7RNeOUw2Y=";

  meta = with lib; {
    description = "Telegram → meetler (LiveKit) media bridge bot";
    license = licenses.asl20;
    mainProgram = "tgbridge";
  };
}

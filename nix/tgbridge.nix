{ lib, buildGoLatestModule }:

buildGoLatestModule {
  pname = "meetler-tgbridge";
  version = "0.1.0";

  src = ../tgbridge;

  # Cache complete modules, so new package imports can reuse existing dependencies.
  proxyVendor = true;
  vendorHash = "sha256-uJ6O7wMIOEkBt6NNNDJPs+oybAw6gY7r8Vo8UHJBZZM=";

  meta = with lib; {
    description = "Telegram → meetler (LiveKit) media bridge bot";
    license = licenses.asl20;
    mainProgram = "tgbridge";
  };
}

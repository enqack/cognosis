# nix-darwin module: declares the same launchd agent
# contrib/com.enqack.cognosis.plist documents for manual installs
# (RunAtLoad, restart unless it exited cleanly), so importing this module
# replaces that copy-paste step rather than adding a second lifecycle.
{ self }:
{ lib, pkgs, config, ... }:
let
  cfg = config.services.cognosis;
  common = import ./options.nix {
    inherit lib;
    defaultPackage = self.packages.${pkgs.system}.default;
  };
in
{
  options.services.cognosis = common;

  config = lib.mkIf cfg.enable {
    launchd.user.agents.cognosis = {
      serviceConfig = {
        ProgramArguments = [ "${cfg.package}/bin/cognosis" "start" "--foreground" ];
        RunAtLoad = true;
        KeepAlive.SuccessfulExit = false;
        EnvironmentVariables = cfg.environment;
      };
    };
  };
}

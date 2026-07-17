# home-manager module for a user-level install (no system-config access
# needed): systemd --user on Linux, launchd on Darwin. Same option surface
# as nixos.nix/darwin.nix so all three speak one vocabulary.
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

  config = lib.mkIf cfg.enable (lib.mkMerge [
    (lib.mkIf pkgs.stdenv.isLinux {
      systemd.user.services.cognosis = {
        Unit.Description = "Cognosis memory daemon";
        Unit.After = [ "network.target" ];
        Service = {
          ExecStart = "${cfg.package}/bin/cognosis start --foreground";
          Restart = "on-failure";
          RestartSec = 5;
          Environment = lib.mapAttrsToList (k: v: "${k}=${v}") cfg.environment;
        };
        Install.WantedBy = [ "default.target" ];
      };
    })

    (lib.mkIf pkgs.stdenv.isDarwin {
      launchd.agents.cognosis = {
        enable = true;
        config = {
          ProgramArguments = [ "${cfg.package}/bin/cognosis" "start" "--foreground" ];
          RunAtLoad = true;
          KeepAlive.SuccessfulExit = false;
          EnvironmentVariables = cfg.environment;
        };
      };
    })
  ]);
}

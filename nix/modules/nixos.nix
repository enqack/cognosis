# NixOS module: declares the same unit contrib/cognosis.service documents for
# manual installs (`--foreground`, restart-on-failure), so importing this
# module replaces that copy-paste step rather than adding a second lifecycle.
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
  options.services.cognosis = common // {
    user = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        User to run the daemon as. Leave null for a DynamicUser (the
        default) -- set this only if the daemon needs to run as a
        specific existing user, e.g. to reach a Postgres unix socket
        restricted to that user.
      '';
    };

    group = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Group to run the daemon as. Ignored when `user` is null.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.cognosis = {
      description = "Cognosis memory daemon";
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];
      environment = cfg.environment;
      serviceConfig =
        {
          ExecStart = "${cfg.package}/bin/cognosis start --foreground";
          Restart = "on-failure";
          RestartSec = 5;
        }
        // lib.optionalAttrs (cfg.user != null) { User = cfg.user; }
        // lib.optionalAttrs (cfg.group != null) { Group = cfg.group; }
        // lib.optionalAttrs (cfg.user == null) {
          DynamicUser = true;
          StateDirectory = "cognosis";
        };
    };
  };
}

# home-manager module for a user-level install (no system-config access
# needed): systemd --user on Linux, launchd on Darwin. Same option surface
# as nixos.nix/darwin.nix so all three speak one vocabulary, plus
# `provisionPostgres`, which is home-manager-only: NixOS and nix-darwin
# users have `services.postgresql`, but home-manager has no way to get a
# cluster at all.
{ self }:
{ lib, pkgs, config, ... }:
let
  cfg = config.services.cognosis;
  pgCfg = cfg.provisionPostgres;
  common = import ./options.nix {
    inherit lib;
    defaultPackage = self.packages.${pkgs.system}.default;
  };

  # Vault history shells out to git, and launchd's default PATH lacks it
  # (systemd --user is friendlier, but only on NixOS-managed hosts). A
  # user-set environment.PATH / COGNOSIS_DSN replaces these defaults.
  env =
    { PATH = "${lib.makeBinPath [ pkgs.git ]}:/usr/bin:/bin"; }
    // lib.optionalAttrs pgCfg.enable {
      COGNOSIS_DSN = "postgres:///${pgCfg.database}?host=${pgCfg.dataDir}";
    }
    // cfg.environment;

  # initdb on first boot, then run postgres socket-only (no TCP listen).
  # Trust auth is confined to unix sockets inside the 0700 data dir, the
  # user-level analogue of contrib/flake.nix's peer-authenticated setup.
  pgStart = pkgs.writeShellScript "cognosis-pg-start" ''
    set -eu
    PGDATA=${pgCfg.dataDir}
    if [ ! -f "$PGDATA/PG_VERSION" ]; then
      mkdir -p "$PGDATA"
      chmod 700 "$PGDATA"
      ${pgCfg.package}/bin/initdb -D "$PGDATA" --auth=trust --encoding=UTF8
      ${pgCfg.package}/bin/pg_ctl -D "$PGDATA" -w \
        -o "-k $PGDATA -c listen_addresses=" start
      ${pgCfg.package}/bin/createdb -h "$PGDATA" ${pgCfg.database}
      ${pgCfg.package}/bin/pg_ctl -D "$PGDATA" -w stop
    fi
    exec ${pgCfg.package}/bin/postgres -D "$PGDATA" -k "$PGDATA" \
      -c listen_addresses=
  '';
  pgLog = "${dirOf pgCfg.dataDir}/pg.log";
in
{
  options.services.cognosis = common // {
    provisionPostgres = {
      enable = lib.mkEnableOption "a user-level Postgres+pgvector cluster for Cognosis" // {
        description = ''
          Provision a socket-only, trust-auth Postgres+pgvector cluster
          as a user service (initdb on first start, data under
          `dataDir`), and default `COGNOSIS_DSN` to it. Trust auth
          never leaves the 0700 data dir, since the server only listens
          on the unix socket inside it.
        '';
      };

      package = lib.mkOption {
        type = lib.types.package;
        default = pkgs.postgresql_16.withPackages (ps: [ ps.pgvector ]);
        defaultText = lib.literalExpression
          "pkgs.postgresql_16.withPackages (ps: [ ps.pgvector ])";
        description = "Postgres package to run; must bundle pgvector.";
      };

      dataDir = lib.mkOption {
        type = lib.types.str;
        default = "${config.xdg.stateHome}/cognosis/pg";
        defaultText = lib.literalExpression ''"''${config.xdg.stateHome}/cognosis/pg"'';
        description = "Cluster data directory; also hosts the unix socket.";
      };

      database = lib.mkOption {
        type = lib.types.str;
        default = "cognosis";
        description = "Database created on first start.";
      };
    };
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    (lib.mkIf pkgs.stdenv.isLinux {
      systemd.user.services = {
        cognosis = {
          Unit.Description = "Cognosis memory daemon";
          Unit.After = [ "network.target" ]
            ++ lib.optional pgCfg.enable "cognosis-postgres.service";
          Service =
            {
              ExecStart = "${cfg.package}/bin/cognosis start --foreground";
              Restart = "on-failure";
              RestartSec = 5;
              Environment = lib.mapAttrsToList (k: v: "${k}=${v}") env;
            }
            // lib.optionalAttrs (cfg.logFile != null) {
              StandardOutput = "append:${cfg.logFile}";
              StandardError = "append:${cfg.logFile}";
            };
          Install.WantedBy = [ "default.target" ];
        };
      } // lib.optionalAttrs pgCfg.enable {
        cognosis-postgres = {
          Unit.Description = "User-level Postgres for Cognosis";
          Service = {
            ExecStart = "${pgStart}";
            Restart = "always";
            RestartSec = 1;
          };
          Install.WantedBy = [ "default.target" ];
        };
      };
    })

    (lib.mkIf pkgs.stdenv.isDarwin {
      launchd.agents = {
        cognosis = {
          enable = true;
          config =
            {
              ProgramArguments = [ "${cfg.package}/bin/cognosis" "start" "--foreground" ];
              RunAtLoad = true;
              KeepAlive.SuccessfulExit = false;
              EnvironmentVariables = env;
            }
            // lib.optionalAttrs (cfg.logFile != null) {
              StandardOutPath = cfg.logFile;
              StandardErrorPath = cfg.logFile;
            };
        };
      } // lib.optionalAttrs pgCfg.enable {
        # launchd has no dependency ordering; the daemon exits nonzero
        # while Postgres is still coming up and its KeepAlive retries
        # until this converges.
        cognosis-postgres = {
          enable = true;
          config = {
            ProgramArguments = [ "${pgStart}" ];
            RunAtLoad = true;
            # KeepAlive = true, not SuccessfulExit = false: the latter
            # leaves the cluster down after any clean SIGTERM (agent
            # reload, logout). Stop deliberately with `launchctl
            # bootout`, not by killing postgres.
            KeepAlive = true;
            StandardOutPath = pgLog;
            StandardErrorPath = pgLog;
          };
        };
      };
    })
  ]);
}

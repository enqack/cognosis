# Shared `services.cognosis` option surface, imported by nixos.nix,
# darwin.nix, and home-manager.nix so all three declare the same vocabulary.
{ lib, defaultPackage }:
{
  enable = lib.mkEnableOption "the Cognosis memory daemon";

  package = lib.mkOption {
    type = lib.types.package;
    default = defaultPackage;
    description = ''
      The `cognosis` package to run. Defaults to this flake's own
      `packages.default` for the current system.
    '';
  };

  environment = lib.mkOption {
    type = lib.types.attrsOf lib.types.str;
    default = { };
    example = {
      COGNOSIS_DSN = "postgres://cognosis@localhost/cognosis";
      COGNOSIS_BIND_ADDRESS = "127.0.0.1:7433";
      COGNOSIS_EMBEDDING_URL = "http://localhost:11434";
    };
    description = ''
      Environment variables passed to the `cognosis` process, using the
      `COGNOSIS_*` names documented in the project's configuration
      reference (`dsn` -> `COGNOSIS_DSN`, `embedding.url` ->
      `COGNOSIS_EMBEDDING_URL`, etc.).

      This module does not provision or manage Postgres+pgvector or
      Ollama -- point `COGNOSIS_DSN` / `COGNOSIS_EMBEDDING_URL` at
      instances your own system configuration (or a remote deployment)
      already makes reachable. (Exception: the home-manager module's
      `provisionPostgres` can supply the Postgres side.)

      Each module puts `git` on the unit's `PATH` by default, because
      vault history shells out to it and launchd's default PATH lacks
      it. Setting `PATH` here replaces that default entirely.
    '';
  };

  logFile = lib.mkOption {
    type = lib.types.nullOr lib.types.str;
    default = null;
    example = "/Users/alice/.local/state/cognosis/daemon.log";
    description = ''
      File the unit appends the daemon's stdout/stderr to. The daemon
      logs to stdout only: under systemd the journal captures it, but
      launchd discards it, leaving the service silent -- set this on
      Darwin to get a daemon log at all. `null` keeps the platform
      default (journal on systemd, no capture on launchd).
    '';
  };
}

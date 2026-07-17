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
      Ollama — point `COGNOSIS_DSN` / `COGNOSIS_EMBEDDING_URL` at
      instances your own system configuration (or a remote deployment)
      already makes reachable.
    '';
  };
}

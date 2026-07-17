{
  description = ''
    Complete example: cognosis + Postgres/pgvector + Ollama on one NixOS
    host, wired together with peer-authenticated Postgres (no password) and
    a declaratively pre-pulled embedding model.

    This is a real, standalone flake (not a bare configuration.nix) so you
    can copy it as-is: `cognosis.url` below is the real remote reference —
    swap only the pieces you need for your own host (system, stateVersion,
    database/user names) and this evaluates as your system's flake input,
    or drop the `nixosConfigurations.example` module list into your own.
  '';

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    cognosis.url = "github:enqack/cognosis";
    cognosis.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = { self, nixpkgs, cognosis }: {
    nixosConfigurations.example = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        cognosis.nixosModules.default
        ({ pkgs, ... }: {
          # A plain, named system user — not `DynamicUser` — so the link
          # between this and the Postgres role below is explicit rather
          # than depending on systemd/nss-systemd name resolution.
          users.users.cognosis = {
            isSystemUser = true;
            group = "cognosis";
          };
          users.groups.cognosis = { };

          services.postgresql = {
            enable = true;
            package = pkgs.postgresql_16;
            extensions = ps: [ ps.pgvector ];
            # Peer-authenticated over the default unix socket: the "cognosis"
            # OS user above maps straight to the "cognosis" Postgres role,
            # no password to manage or leak into the Nix store.
            ensureDatabases = [ "cognosis" ];
            ensureUsers = [
              {
                name = "cognosis";
                # Lets the daemon's own migration run `create extension if
                # not exists vector` (see docs/configuration.md) without
                # needing Postgres superuser.
                ensureDBOwnership = true;
              }
            ];
          };

          services.ollama = {
            enable = true;
            # The pinned embedding model (docs/configuration.md) is pulled
            # declaratively on activation, rather than a manual `ollama pull`.
            loadModels = [ "nomic-embed-text:v1.5" ];
          };

          services.cognosis = {
            enable = true;
            user = "cognosis";
            group = "cognosis";
            environment = {
              COGNOSIS_DSN = "postgres:///cognosis?host=/run/postgresql";
              COGNOSIS_EMBEDDING_URL = "http://127.0.0.1:11434";
            };
          };

          # Set this to whatever release you actually installed with —
          # it's system-identity state, not something this example can pick
          # for you. See `man 5 configuration.nix` / the NixOS manual.
          system.stateVersion = "24.05";
        })
      ];
    };
  };

  # Deliberately not covered here:
  #   - TLS / non-loopback access — see docs/remote.md.
  #   - The MCP bearer token — auto-minted on first start, not declared.
  #   - nix-darwin / home-manager installs — those platforms don't get a
  #     services.postgresql/services.ollama from this module either; point
  #     services.cognosis.environment at an already-running instance
  #     instead, the same as Path B's manual install.
}

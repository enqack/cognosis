{
  description = ''
    Cognosis: dev shell (Go toolchain + Postgres/pgvector + Ollama), the cognosis
    package/app, and NixOS/nix-darwin/home-manager service modules — the modules
    run the daemon but never provision Postgres/pgvector or Ollama themselves
  '';

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    (flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        pg = pkgs.postgresql_16.withPackages (p: [ p.pgvector ]);

        # Unix-socket-only dev Postgres in an in-repo, gitignored data dir.
        # Port 5434 so it never collides with a silo-kb instance on 5433.
        # macOS caps sun_path at 104 bytes; fall back to a hashed $TMPDIR dir
        # when the repo path is too deep (persisted so the DSN stays stable).
        pgEnv = ''
          export COGNOSIS_PGDATA="$PWD/.pg-data"
          SOCKDIR="$COGNOSIS_PGDATA"
          if [ "''${#SOCKDIR}" -gt 90 ]; then
            if [ -f .pg-socket-path ]; then
              SOCKDIR="$(cat .pg-socket-path)"
            else
              SOCKDIR="''${TMPDIR:-/tmp}/cognosis-$(echo -n "$PWD" | shasum -a 256 | cut -c1-12)"
              echo "$SOCKDIR" > .pg-socket-path
            fi
            mkdir -p "$SOCKDIR"
          fi
          export COGNOSIS_SOCKDIR="$SOCKDIR"
          export COGNOSIS_DSN="postgres:///cognosis?host=$COGNOSIS_SOCKDIR&port=5434"
        '';

        pg-start = pkgs.writeShellScriptBin "pg-start" ''
          set -euo pipefail
          ${pgEnv}
          if [ ! -d "$COGNOSIS_PGDATA" ]; then
            ${pg}/bin/initdb -D "$COGNOSIS_PGDATA" --auth=trust >/dev/null
          fi
          if ! ${pg}/bin/pg_ctl -D "$COGNOSIS_PGDATA" status >/dev/null 2>&1; then
            ${pg}/bin/pg_ctl -D "$COGNOSIS_PGDATA" -l "$COGNOSIS_PGDATA/log" \
              -o "-k $COGNOSIS_SOCKDIR -p 5434 -c listen_addresses=" start
          fi
          if ! ${pg}/bin/psql -h "$COGNOSIS_SOCKDIR" -p 5434 -d cognosis -c "" >/dev/null 2>&1; then
            ${pg}/bin/createdb -h "$COGNOSIS_SOCKDIR" -p 5434 cognosis
          fi
          # Create pgvector in the public schema up front. The daemon's migration
          # also does this, but `go test ./...` runs packages in parallel against
          # one database with per-schema isolation, and CREATE EXTENSION is a
          # per-database singleton: concurrent creators race on
          # pg_extension_name_index, and whoever wins puts the vector type in a
          # private test schema the others can't see. Having it in public (on
          # every schema's search_path) before any test runs avoids both.
          ${pg}/bin/psql -h "$COGNOSIS_SOCKDIR" -p 5434 -d cognosis \
            -c 'create extension if not exists vector' >/dev/null
          echo "Postgres up: $COGNOSIS_DSN"
        '';

        pg-stop = pkgs.writeShellScriptBin "pg-stop" ''
          set -euo pipefail
          ${pgEnv}
          ${pg}/bin/pg_ctl -D "$COGNOSIS_PGDATA" stop || true
        '';

        # Kept in lockstep with go.mod's `go` line (the enforced minimum) and
        # magefile.go's ldflags() (version stamping). pkgs.go here may trail
        # go.mod's `toolchain` pin by a patch release — buildGoModule runs with
        # GOTOOLCHAIN=local, which only enforces the `go` line minimum, so that
        # lag doesn't break the build; it just means this binary misses
        # whatever stdlib fix the toolchain patch carries.
        version = pkgs.lib.removeSuffix "\n" (builtins.readFile ./VERSION);

        cognosis = pkgs.buildGoModule {
          pname = "cognosis";
          inherit version;
          # Excludes dotfiles/dirs (.git, .idea, the gitignored .pg-data, and
          # any stray unreadable local state like a root-owned .cache) rather
          # than allowlisting Go source dirs, so new source directories don't
          # need a flake.nix edit to be picked up.
          src = pkgs.lib.cleanSourceWith {
            src = ./.;
            filter = name: type:
              let base = baseNameOf name; in
              !(pkgs.lib.hasPrefix "." base) && base != "bin" && base != "dist";
          };
          vendorHash = "sha256-Y8IbLsajP9AruiTzD2DPYpgaBWSPY1Pj4SiKSToc1Zc=";
          subPackages = [ "cmd/cognosis" ];
          ldflags = [ "-X main.version=${version}" ];
          doCheck = false; # full suite needs Postgres/Ollama; run via `mage check`, not the Nix build
        };
      in
      {
        packages.default = cognosis;
        packages.cognosis = cognosis;

        apps.default = {
          type = "app";
          program = "${cognosis}/bin/cognosis";
        };

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gcc            # cc on PATH for `go test -race` / cgo
            pkgs.git
            pkgs.mage
            pkgs.golangci-lint  # `mage lint` static analysis
            pkgs.xz             # `mage release` streams .tar.xz through xz(1)
            pkgs.jq             # hooks/session-end-nudge.sh parses the SessionEnd payload
            pkgs.ollama
            pg
            pg-start
            pg-stop
          ];
          shellHook = ''
            ${pgEnv}
            echo "cognosis dev shell — pg-start / pg-stop; DSN in \$COGNOSIS_DSN"
          '';
        };
      }))
    // {
      # Service modules, not per-system: each resolves its own default
      # package via `self.packages.${pkgs.system}.default` at the point
      # the consumer's host config applies it.
      nixosModules.default = import ./nix/modules/nixos.nix { inherit self; };
      darwinModules.default = import ./nix/modules/darwin.nix { inherit self; };
      homeManagerModules.default = import ./nix/modules/home-manager.nix { inherit self; };
    };
}

{
  description = "Cognosis dev environment: Go toolchain + Postgres/pgvector + Ollama (installation only — does not manage the running daemon's lifecycle)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
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
          echo "Postgres up: $COGNOSIS_DSN"
        '';

        pg-stop = pkgs.writeShellScriptBin "pg-stop" ''
          set -euo pipefail
          ${pgEnv}
          ${pg}/bin/pg_ctl -D "$COGNOSIS_PGDATA" stop || true
        '';
      in
      {
        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gcc            # cc on PATH for `go test -race` / cgo
            pkgs.git
            pkgs.mage
            pkgs.golangci-lint  # `mage lint` static analysis
            pkgs.xz             # `mage release` streams .tar.xz through xz(1)
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
      });
}

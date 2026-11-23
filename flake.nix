{
  description = "DittoFS - Modular virtual filesystem with pluggable storage backends";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    # Original C-based pjdfstest (used by JuiceFS, FreeBSD, and others)
    # This is the authoritative POSIX compliance test suite
    pjdfstest-src = {
      url = "github:pjd/pjdfstest";
      flake = false;
    };
    # pynfs - the reference NFSv4.0/4.1 protocol conformance suite.
    # Speaks NFSv4 itself, so it needs no kernel mount and no privileges.
    pynfs-src = {
      url = "github:kofemann/pynfs";
      flake = false;
    };
    # Connectathon and NetApp's nfstest: what a kernel client sees through an
    # untuned mount. git.linux-nfs.org serves them over git:// only.
    cthon04-src = {
      url = "git+git://git.linux-nfs.org/projects/steved/cthon04.git";
      flake = false;
    };
    nfstest-src = {
      url = "git+git://git.linux-nfs.org/projects/mora/nfstest.git";
      flake = false;
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      pjdfstest-src,
      pynfs-src,
      cthon04-src,
      nfstest-src,
    }:
    let
      # Version configuration - update this for releases
      version = "0.36.0";
    in
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        # Git revision for build info (use "dirty" if uncommitted changes)
        gitRev = self.shortRev or self.dirtyShortRev or "unknown";

        # Helper scripts for DittoFS development (works in any shell)
        dfs-mount = pkgs.writeShellScriptBin "dfs-mount" ''
          mount_point="''${1:-/tmp/dittofs-test}"
          sudo mkdir -p "$mount_point" 2>/dev/null || true
          sudo ${pkgs.nfs-utils}/bin/mount.nfs -o nfsvers=3,tcp,port=12049,mountport=12049,nolock,actimeo=0 \
            localhost:/export "$mount_point"
          echo "Mounted at $mount_point"
        '';

        dfs-umount = pkgs.writeShellScriptBin "dfs-umount" ''
          mount_point="''${1:-/tmp/dittofs-test}"
          sudo ${pkgs.util-linux}/bin/umount "$mount_point"
          echo "Unmounted $mount_point"
        '';

        # Original C-based pjdfstest (used by JuiceFS, FreeBSD, and others)
        # This is the authoritative POSIX compliance test suite with 8,789 tests
        pjdfstest = pkgs.stdenv.mkDerivation {
          pname = "pjdfstest";
          version = "2025-01-07";
          src = pjdfstest-src;

          nativeBuildInputs = with pkgs; [
            autoconf
            automake
            pkg-config
          ];

          buildInputs = with pkgs; [
            acl
          ];

          # Build the pjdfstest binary
          buildPhase = ''
            autoreconf -ifs
            ./configure --prefix=$out
            make pjdfstest
          '';

          # Install binary and tests
          # Note: binary must be in share/pjdfstest/ because misc.sh walks up
          # from tests/ looking for it in parent directories
          installPhase = ''
            mkdir -p $out/bin $out/share/pjdfstest
            cp pjdfstest $out/bin/
            cp pjdfstest $out/share/pjdfstest/
            cp -r tests $out/share/pjdfstest/
          '';

          meta = with pkgs.lib; {
            description = "POSIX filesystem test suite (C version, used by JuiceFS/FreeBSD)";
            homepage = "https://github.com/pjd/pjdfstest";
            license = licenses.bsd2;
            platforms = platforms.linux;
          };
        };

        # pynfs - the reference NFSv4 protocol conformance suite.
        #
        # Unlike pjdfstest, pynfs is its own NFSv4 client: it speaks the protocol
        # over TCP and needs neither a kernel mount nor root, so it runs on any
        # unix host that can reach the server.
        #
        # Python 3.12 is deliberate. pynfs unpacks XDR with the stdlib `xdrlib`
        # module, which was removed in 3.13; the replacement `xdrlib3` is not in
        # nixpkgs. Moving to python3 (currently 3.13) silently breaks every test
        # at import time, so pin the interpreter rather than the alias.
        pynfs =
          let
            python = pkgs.python312;
          in
          # stdenvNoCC: pynfs is pure Python, and a plain mkDerivation would pull
          # the whole C toolchain (on darwin, clang plus the Apple SDK — the best
          # part of a gigabyte) to run a script that never compiles anything.
          pkgs.stdenvNoCC.mkDerivation {
            pname = "pynfs";
            version = "2026-03-27";
            src = pynfs-src;

            nativeBuildInputs = [
              python
              python.pkgs.ply
              python.pkgs.setuptools
              pkgs.makeWrapper
            ];

            # Generates xdrdef/*_const.py, *_type.py and *_pack.py in place from
            # the .x files. The top-level setup.py shells out per subdirectory
            # with os.system and discards the status, so verify the outputs.
            buildPhase = ''
              runHook preBuild
              ${python}/bin/python3 setup.py build
              for f in nfs4.1/xdrdef/nfs4_const.py \
                       nfs4.1/xdrdef/nfs4_pack.py \
                       nfs4.1/xdrdef/nfs3_const.py; do
                if [ ! -f "$f" ]; then
                  echo "pynfs: XDR generation did not produce $f" >&2
                  exit 1
                fi
              done
              runHook postBuild
            '';

            # -a keeps the symlinks: nfs4.0/xdrdef and nfs4.0/lib/testmod.py both
            # point into the nfs4.1 tree, and dereferencing them desynchronises
            # the two suites.
            installPhase = ''
              runHook preInstall
              mkdir -p $out/share/pynfs $out/bin
              cp -a . $out/share/pynfs/

              for v in 4.0 4.1; do
                makeWrapper ${python}/bin/python3 "$out/bin/pynfs-$v" \
                  --add-flags "$out/share/pynfs/nfs$v/testserver.py" \
                  --run "cd $out/share/pynfs/nfs$v"
              done
              runHook postInstall
            '';

            # nfs4.1/testserver.py imports use_local, which builds sys.path from
            # the current directory, so the wrappers must cd into their own tree.
            doCheck = false;

            meta = with pkgs.lib; {
              description = "NFSv4.0/4.1 protocol conformance test suite";
              homepage = "https://linux-nfs.org/wiki/index.php/Pynfs";
              license = licenses.gpl2Only;
              platforms = platforms.unix;
              mainProgram = "pynfs-4.1";
            };
          };

        # Connectathon's basic, general, special and lock tests, built in their
        # tree, which they run from; general's compile on the mount at run time.
        # bin holds upstream's entry points.
        cthon04 = pkgs.stdenv.mkDerivation {
          pname = "cthon04";
          version = "2026-06-01";
          src = cthon04-src;

          nativeBuildInputs = [ pkgs.pkg-config ];
          buildInputs = [ pkgs.libtirpc ];

          # The Makefile runs tests.init.sh to write tests.init, which it includes,
          # and whose PATH would hide the build's own tools.
          postPatch = "patchShebangs tests.init.sh";
          buildPhase = ''make PATH="$PATH"'';

          installPhase = ''
            mkdir -p $out/share $out/bin
            cp -a . $out/share/cthon04
            ln -s $out/share/cthon04/runtests $out/share/cthon04/server $out/bin/
          '';

          meta = with pkgs.lib; {
            description = "Connectathon NFS test suite";
            homepage = "https://git.linux-nfs.org/?p=steved/cthon04.git";
            license = licenses.bsd2;
            platforms = platforms.linux;
          };
        };

        # NetApp's nfstest modules, one wrapper each in bin, as nfstest_posix.
        #
        # Python 3.12 is deliberate, as for pynfs. nfstest's rexec runs each
        # statement it is sent with a bare exec() inside a function, and from 3.13
        # on a name one statement binds is gone by the next, so the lock and
        # delegation modules abort at their setup with KeyError: 'fcntl'. The
        # install check runs that setup, so moving to python3 fails the build.
        nfstest =
          let
            python = pkgs.python312;
          in
          pkgs.stdenvNoCC.mkDerivation {
            pname = "nfstest";
            version = "2024-01-17";
            src = nfstest-src;

            nativeBuildInputs = [ pkgs.makeWrapper ];
            dontBuild = true;

            installPhase = ''
              mkdir -p $out/share $out/bin
              cp -a . $out/share/nfstest
              for t in $out/share/nfstest/test/nfstest_*; do
                makeWrapper ${python}/bin/python3 "$out/bin/$(basename "$t")" \
                  --add-flags "$t" --prefix PYTHONPATH : $out/share/nfstest
              done
            '';

            doInstallCheck = true;
            installCheckPhase = ''
              PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=$out/share/nfstest ${python}/bin/python3 -P -c \
                'from nfstest.rexec import Rexec; Rexec().rimport("fcntl", ["fcntl", "F_SETLK"])'
            '';

            meta = with pkgs.lib; {
              description = "NFS test modules";
              homepage = "https://git.linux-nfs.org/?p=mora/nfstest.git";
              license = licenses.gpl2Only;
              platforms = platforms.linux;
            };
          };

        # Common build inputs for both development and CI
        commonBuildInputs = with pkgs; [
          # Shell
          zsh

          # Go development (matches go.mod)
          go_1_26
          gopls
          golangci-lint
          delve

          # Build tools
          gnumake
          git

          curl # Localstack's health check in test/harness/env.sh

          # NFSv4 conformance suite. Pure Python speaking NFSv4 over TCP, so it
          # needs no kernel mount and runs on every platform.
          pynfs
        ];

        # Platform-specific inputs
        linuxInputs =
          with pkgs;
          lib.optionals stdenv.isLinux [
            # Benchmark load generator for the dfsbench harness (bench/, cmd/bench).
            # Its libnbd dependency is Linux-only, and evaluating it on macOS
            # fails outright, taking the whole dev shell with it.
            fio
            # NFS testing tools (Linux only)
            nfs-utils
            # ACL support for POSIX compliance testing
            acl
            # Extended-attribute tools (setfattr/getfattr) for xattr e2e tests
            attr
            # Perl for pjdfstest (prove is included in base perl)
            perl
            # POSIX filesystem compliance testing (C version - authoritative)
            pjdfstest
            # Docker client for PostgreSQL testing (daemon must be running on host)
            docker-client
            # FoundationDB's client library and headers: builds with the
            # foundationdb tag compile the fdbkv backend, which links libfdb_c.
            # Any 7.3 client speaks to any 7.3 server.
            foundationdb
            # Helper scripts that need the Linux NFS client.
            dfs-mount
            dfs-umount
          ];

        darwinInputs =
          with pkgs;
          lib.optionals stdenv.isDarwin [
            # macOS uses built-in NFS client
            # For pjdfstest on macOS, use dtc: see test/posix/README.md
          ];

        # Go cache locations, shared by every shell.
        goEnv = ''
          # Ensure Go modules are cached in user's home directory
          export GOPATH="$HOME/go"
          export GOMODCACHE="$HOME/go/pkg/mod"
          export GOCACHE="$HOME/.cache/go-build"
        '';

        devShell = pkgs.mkShell {
          buildInputs = commonBuildInputs ++ linuxInputs ++ darwinInputs;

          shellHook = goEnv + ''
            echo "╔═══════════════════════════════════════════╗"
            echo "║     DittoFS Development Environment       ║"
            echo "╚═══════════════════════════════════════════╝"
            echo ""
            echo "Go version: $(go version | cut -d' ' -f3)"
            echo ""
            echo "Available commands:"
            echo "  go build ./cmd/dfs          Build DittoFS binary"
            echo "  go test ./...               Run all tests"
            echo "  go test -race ./...         Run tests with race detection"
            echo "  golangci-lint run           Run linters"
            echo ""
            if [[ "$(uname)" == "Darwin" ]]; then
              echo "NFS mount (macOS):"
              echo "  sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,resvport,nolock localhost:/export /tmp/dittofs-test"
              echo ""
              echo "POSIX compliance testing (pjdfstest; dt runs it in the dtc container):"
              echo "  test/harness/bin/dt services up localstack"
              echo "  test/harness/bin/dt run pjdfstest/badger-s3/3"
            else
              echo "NFS helper commands:"
              echo "  dfs-mount [path]            Mount NFS (default: /tmp/dittofs-test)"
              echo "  dfs-umount [path]           Unmount NFS"
              echo ""
              echo "POSIX compliance testing (pjdfstest, through the harness):"
              echo "  test/harness/bin/dt run pjdfstest"
              echo ""
              echo "E2E testing (dt runs it through passwordless sudo):"
              echo "  test/harness/bin/dt run e2e [TestName]"
              echo ""
            fi
            echo "PostgreSQL for the integration tests:"
            echo "  test/harness/bin/dt services up postgres"
            echo ""
            echo "NFSv4 protocol conformance (no mount, no root):"
            echo "  pynfs-4.0 / pynfs-4.1       pynfs test client"
            echo ""

            # Use zsh if available and not already in zsh
            if [ -n "$ZSH_VERSION" ]; then
              : # Already in zsh
            elif command -v zsh &> /dev/null; then
              exec zsh
            fi
          '';
        };
      in
      {
        devShells.default = devShell;

        # Same toolchain as the development shell, without the banner and
        # without exec-ing into zsh.
        devShells.ci = devShell.overrideAttrs { shellHook = goEnv; };

        # Go toolchain only, for jobs that compile the binaries and nothing
        # else. The full shell carries the conformance suites — `pynfs` in
        # commonBuildInputs, `pjdfstest` in linuxInputs — and both are built
        # from source rather than substituted, so entering it to run `go build`
        # compiles an NFS conformance suite first. This does not.
        devShells.build = pkgs.mkShell {
          buildInputs = [ pkgs.go_1_26 ];
          shellHook = goEnv;
        };

        # Packages for building DittoFS
        packages =
          let
            commonArgs = {
              inherit version;
              src = ./.;

              # Auto-updated by .github/workflows/nix-update-hash.yml on go.mod/go.sum changes.
              # Manual: go run scripts/update-nix-hash.go
              vendorHash = "sha256-oAVcHUd/aVgZOo6PjwtyZrRh5XSsESWvXQfDZTinxWQ=";

              ldflags = [
                "-s"
                "-w"
                "-X main.version=${version}"
                "-X main.commit=${gitRev}"
              ];

              meta = with pkgs.lib; {
                description = "Modular virtual filesystem with pluggable storage backends";
                homepage = "https://github.com/cubbit/dittofs";
                license = licenses.mit;
                platforms = platforms.unix;
              };
            };
          in
          # Track the toolchain the module requires: nixpkgs' default `go` lags
          # behind the go.mod directive, and buildGoModule runs with
          # GOTOOLCHAIN=local, so a lagging toolchain fails the fetch phase
          # outright instead of downloading the newer one.
          let
            buildGo = pkgs.buildGoModule.override { go = pkgs.go_1_26; };
          in
          {
            # Build both dfs and dfsctl
            default = buildGo (
              commonArgs
              // {
                pname = "dittofs";
                subPackages = [
                  "cmd/dfs"
                  "cmd/dfsctl"
                ];
                meta = commonArgs.meta // {
                  mainProgram = "dfs";
                };
              }
            );

            # Build only the server daemon
            dfs = buildGo (
              commonArgs
              // {
                pname = "dfs";
                subPackages = [ "cmd/dfs" ];
              }
            );

            # Build only the client CLI
            dfsctl = buildGo (
              commonArgs
              // {
                pname = "dfsctl";
                subPackages = [ "cmd/dfsctl" ];
                meta = commonArgs.meta // {
                  description = "Command-line client for managing DittoFS";
                };
              }
            );

            # NFSv4 protocol conformance suite. Not Linux-gated: pynfs is a
            # pure-Python NFSv4 client, so it also runs against a server on macOS.
            inherit pynfs;
          }
          // pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
            inherit pjdfstest cthon04 nfstest;
          };

        # `nix run .#dfs` / `nix run github:cubbit/dittofs#dfsctl`, so the
        # binaries are reachable without cloning and building first.
        apps =
          let
            # mkApp alone leaves out meta, which `nix flake check` warns about.
            app =
              drv: name: description:
              flake-utils.lib.mkApp { inherit drv name; } // { meta.description = description; };
          in
          rec {
            default = dfs;
            dfs = app self.packages.${system}.dfs "dfs" "DittoFS server daemon";
            dfsctl = app self.packages.${system}.dfsctl "dfsctl" "DittoFS REST client";

            # pynfs ships one entrypoint per NFSv4 minor version; `nix run .#pynfs`
            # resolves to the 4.1 one through the package's mainProgram. The
            # attribute names use an underscore because nix splits an attribute
            # path on dots.
            pynfs-4_0 = app pynfs "pynfs-4.0" "pynfs NFSv4.0 conformance client";
            pynfs-4_1 = app pynfs "pynfs-4.1" "pynfs NFSv4.1 conformance client";
          };

        # Flake checks - run with `nix flake check`
        checks = {
          # Verify the default package builds and contains both binaries
          default-has-dfs = pkgs.runCommand "check-default-has-dfs" { } ''
            ${self.packages.${system}.default}/bin/dfs version > /dev/null 2>&1 || \
            ${self.packages.${system}.default}/bin/dfs --help > /dev/null 2>&1
            touch $out
          '';
          default-has-dfsctl = pkgs.runCommand "check-default-has-dfsctl" { } ''
            ${self.packages.${system}.default}/bin/dfsctl version > /dev/null 2>&1 || \
            ${self.packages.${system}.default}/bin/dfsctl --help > /dev/null 2>&1
            touch $out
          '';

          # Verify individual packages produce the correct binary
          dfs-binary = pkgs.runCommand "check-dfs-binary" { } ''
            ${self.packages.${system}.dfs}/bin/dfs version > /dev/null 2>&1 || \
            ${self.packages.${system}.dfs}/bin/dfs --help > /dev/null 2>&1
            touch $out
          '';
          dfsctl-binary = pkgs.runCommand "check-dfsctl-binary" { } ''
            ${self.packages.${system}.dfsctl}/bin/dfsctl version > /dev/null 2>&1 || \
            ${self.packages.${system}.dfsctl}/bin/dfsctl --help > /dev/null 2>&1
            touch $out
          '';
        };
      }
    );
}

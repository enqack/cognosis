//go:build mage

package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/magefile/mage/sh"
)

// ldflags injects the version (from VERSION, or the VERSION/GITHUB_REF_NAME env
// overrides) into main.version at link time.
func ldflags() string {
	return fmt.Sprintf("-X main.version=%s", getVersion())
}

// Build compiles the cognosis binary into ./bin, version-stamped.
func Build() error {
	return sh.RunV("go", "build", "-ldflags", ldflags(), "-o", "bin/cognosis", "./cmd/cognosis")
}

// Test runs the full suite with the race detector. This is the suite CI runs,
// so the zero-downtime-under-load test (a 5k-chunk migration under concurrent
// queries) is proven on every push.
func Test() error {
	return sh.RunV("go", "test", "-race", "./...")
}

// TestShort runs the suite with -short, skipping the tests that gate on it —
// today just the 5k-chunk load test in internal/migrate. The fast inner-loop
// suite; Test remains the one that proves the load claim.
func TestShort() error {
	return sh.RunV("go", "test", "-race", "-short", "./...")
}

// Lint runs gofmt (check mode) and golangci-lint over the correctness +
// security linter set configured in .golangci.yml (govet, errcheck,
// staticcheck, gosec, and more) — see that file for the authoritative list.
func Lint() error {
	out, err := sh.Output("gofmt", "-l", ".")
	if err != nil {
		return err
	}
	if out != "" {
		return fmt.Errorf("gofmt needed:\n%s", out)
	}
	return sh.RunV("golangci-lint", "run", "./...")
}

// Install installs the binary into GOBIN, version-stamped.
func Install() error {
	return sh.RunV("go", "install", "-ldflags", ldflags(), "./cmd/cognosis")
}

// Check runs the end-to-end feature checks (scripts/checks/*.sh via
// scripts/check-all.sh). Local/dev only — needs a reachable Postgres
// (COGNOSIS_DSN, e.g. pg-start) and a local Ollama with the embedding model
// pulled, so it is deliberately not part of CI.
func Check() error {
	return sh.RunV("bash", "scripts/check-all.sh")
}

// Release cross-compiles version-stamped binaries into dist/, archives each as
// a .tar.gz, and writes a SHA256SUMS manifest. The version comes from
// GITHUB_REF_NAME (the release tag) or the VERSION file. Targets are Unix-only:
// cognosis is a Unix daemon (setsid self-daemonization, unix-socket Postgres,
// systemd/launchd units), so Windows is not a supported platform.
func Release() error {
	v := getVersion()
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	fmt.Printf("releasing %s\n", v)

	platforms := []struct{ os, arch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
	}

	if err := os.RemoveAll("dist"); err != nil {
		return err
	}
	if err := os.MkdirAll("dist", 0o750); err != nil {
		return err
	}
	flags := fmt.Sprintf("-X main.version=%s", v)

	for _, p := range platforms {
		binPath := filepath.Join("dist", "cognosis")

		fmt.Printf("building %s/%s\n", p.os, p.arch)
		cmd := exec.Command("go", "build", "-ldflags", flags, "-o", binPath, "./cmd/cognosis")
		// CGO off: pure-Go cross-compilation with no target C toolchain needed
		// (release binaries don't use -race).
		cmd.Env = append(os.Environ(), "GOOS="+p.os, "GOARCH="+p.arch, "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s/%s: %w", p.os, p.arch, err)
		}

		stem := filepath.Join("dist", fmt.Sprintf("cognosis-%s-%s-%s", v, p.os, p.arch))
		if err := archiveTarGz(stem+".tar.gz", binPath, "cognosis"); err != nil {
			return fmt.Errorf("archive %s/%s (gz): %w", p.os, p.arch, err)
		}
		if err := archiveTarXz(stem+".tar.xz", binPath, "cognosis"); err != nil {
			return fmt.Errorf("archive %s/%s (xz): %w", p.os, p.arch, err)
		}
		if err := os.Remove(binPath); err != nil {
			return err
		}
	}

	if err := writeSHA256Sums("dist"); err != nil {
		return fmt.Errorf("checksums: %w", err)
	}
	fmt.Println("release artifacts written to dist/")
	return nil
}

// writeTar streams a single-file tar (binPath under nameInArc) into w.
func writeTar(w io.Writer, binPath, nameInArc string) error {
	src, err := os.Open(binPath)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	hdr := &tar.Header{Name: nameInArc, Mode: 0o755, Size: info.Size()}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.Copy(tw, src); err != nil {
		return err
	}
	return tw.Close()
}

// archiveTarGz writes a single-file .tar.gz (gzip via the Go stdlib).
func archiveTarGz(archivePath, binPath, nameInArc string) error {
	out, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	if err := writeTar(gz, binPath, nameInArc); err != nil {
		return err
	}
	return gz.Close()
}

// archiveTarXz writes a single-file .tar.xz by streaming the tar through the
// xz(1) binary — the Go stdlib has no xz writer, and xz is flake-provided.
func archiveTarXz(archivePath, binPath, nameInArc string) error {
	out, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer out.Close()

	cmd := exec.Command("xz", "-z", "-c", "-T0")
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	werr := writeTar(stdin, binPath, nameInArc)
	stdin.Close()
	cerr := cmd.Wait()
	if werr != nil {
		return werr
	}
	return cerr
}

// writeSHA256Sums generates a SHA256SUMS manifest for every file in dir.
func writeSHA256Sums(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	defer f.Close()

	for _, e := range entries {
		if e.IsDir() || e.Name() == "SHA256SUMS" {
			continue
		}
		fh, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		h := sha256.New()
		if _, err := io.Copy(h, fh); err != nil {
			fh.Close()
			return err
		}
		fh.Close()
		fmt.Fprintf(f, "%x  %s\n", h.Sum(nil), e.Name())
	}
	return nil
}

// getVersion resolves the build version: an explicit VERSION or GITHUB_REF_NAME
// env var wins (the release/CI path), else the VERSION file, else "dev".
func getVersion() string {
	if v := os.Getenv("VERSION"); v != "" {
		return v
	}
	if v := os.Getenv("GITHUB_REF_NAME"); v != "" {
		return v
	}
	data, err := os.ReadFile("VERSION")
	if err != nil {
		return "dev"
	}
	return strings.TrimSpace(string(data))
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const (
	module = "go.sandbox.ec/runspace"

	// minPackages guards against go list silently finding almost nothing.
	minPackages = 8
)

// goPackage is the part of go list -json that the import graph needs.
// encoding/json matches the tags to go list's capitalized keys regardless of
// case.
type goPackage struct {
	ImportPath string   `json:"importPath"`
	Imports    []string `json:"imports"`
	Dir        string   `json:"dir"`
	GoFiles    []string `json:"goFiles"`
}

// listPackages returns runspace's own packages, from go list.
func listPackages(t *testing.T) []goPackage {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "go", "list", "-json", module+"/...").Output()
	if err != nil {
		exitErr, isExit := errors.AsType[*exec.ExitError](err)
		if isExit {
			t.Fatalf("go list: %v\n%s", err, exitErr.Stderr)
		}

		t.Skipf("go list: %v", err)
	}

	var pkgs []goPackage

	decoder := json.NewDecoder(bytes.NewReader(out))
	for decoder.More() {
		var pkg goPackage

		err := decoder.Decode(&pkg)
		if err != nil {
			t.Fatal(err)
		}

		pkgs = append(pkgs, pkg)
	}

	return pkgs
}

// The import graph of production code: nothing imports os/exec, only
// internal/csconn imports the Dev Tunnels and gRPC modules, the front ends
// import none of internal/remote, internal/github, and internal/csconn, and
// internal/github and internal/transfer use only the standard library. It
// checks the direct imports of runspace's own packages.
func TestImportGraph(t *testing.T) {
	t.Parallel()

	pkgs := listPackages(t)

	for _, pkg := range pkgs {
		for _, imp := range pkg.Imports {
			checkImport(t, pkg.ImportPath, imp)
		}
	}

	checkNoProcessStarts(t, pkgs)

	if len(pkgs) < minPackages {
		t.Errorf("go list returned only %d packages", len(pkgs))
	}
}

// checkImport checks one direct import of one package.
func checkImport(t *testing.T, pkg, imp string) {
	t.Helper()

	csconn := module + "/internal/csconn"
	// The CLI is the root package.
	frontEnds := []string{module, module + "/internal/mcpserver"}
	bannedForFrontEnds := []string{module + "/internal/remote", module + "/internal/github", csconn}
	stdlibOnly := []string{module + "/internal/github", module + "/internal/transfer"}

	if imp == "os/exec" {
		t.Errorf("%s imports os/exec", pkg)
	}

	protocol := strings.HasPrefix(imp, "github.com/microsoft/dev-tunnels") ||
		strings.HasPrefix(imp, "google.golang.org/grpc")
	if protocol && !under(pkg, csconn) {
		t.Errorf("%s imports %s; only internal/csconn may", pkg, imp)
	}

	if slices.Contains(frontEnds, pkg) {
		for _, banned := range bannedForFrontEnds {
			if under(imp, banned) {
				t.Errorf("front end %s imports %s", pkg, imp)
			}
		}
	}

	if slices.Contains(stdlibOnly, pkg) && strings.Contains(strings.Split(imp, "/")[0], ".") {
		t.Errorf("%s imports %s; it may use only the standard library", pkg, imp)
	}
}

// checkNoProcessStarts checks that no package starts a host process by
// other means than os/exec, which checkImport bans.
func checkNoProcessStarts(t *testing.T, pkgs []goPackage) {
	t.Helper()

	calls := []string{"os.StartProcess", "syscall.Exec", "syscall.ForkExec", "syscall.StartProcess"}

	for _, pkg := range pkgs {
		dir, err := os.OpenRoot(pkg.Dir)
		if err != nil {
			t.Fatal(err)
		}

		for _, file := range pkg.GoFiles {
			source, err := dir.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}

			for _, call := range calls {
				if strings.Contains(string(source), call) {
					t.Errorf("%s/%s calls %s", pkg.ImportPath, file, call)
				}
			}
		}

		err = dir.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func under(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

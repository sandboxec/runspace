# Contributing to runspace

This guide covers how to build runspace, the rules its code and tests depend
on, and how to check a change before you open a pull request.

## Set up

You need:

- Go 1.26 or later, the minimum `go.mod` declares. CI uses the latest
  stable release.
- make, and bash for the scripts in `scripts/`.
- A C compiler, such as gcc or clang. `make check` runs the tests with the
  race detector, which needs cgo.
- [golangci-lint](https://golangci-lint.run) v2.15.0 on `PATH`, for
  `make check`.

Build the binary into `build/bin/runspace`:

```sh
make build
```

`make help` lists the targets.

## Keep the CLI and the MCP server the same

The CLI, in the root package, and the MCP server, in `internal/mcpserver`,
run the same seven commands, defined once in `internal/sandbox`. Each
command has one options struct and, if it takes any options, one
`Validate` method, and both front ends use them. The two front ends must
accept and reject the same input with the same message:

- Put a new check in the shared `Validate`. If a problem shows up through
  only one front end, fix it for both. A front end checks only its own
  transport, as the MCP server does when it rejects `cp -`.
- Add an option as a field of the options struct. Tag it with `json`, with
  `flag` (or `arg` for a positional), and with a `jsonschema` description
  that states any default. The MCP tool schema comes from the struct.
  Declare the flag in the root package with the default, and apply the
  default in `Validate` for an MCP call that leaves the field empty.

The front ends differ in only four ways:

- Stream and output-format flags (`-i`, `-t`, `--json`, and `-q`) exist only
  in the CLI.
- The `stdin` field exists only over MCP.
- `cp` accepts `-`, a tar stream on stdin or stdout, only in the CLI.
- Over MCP, `create` and `run` require `repo`. The CLI can take it from the
  git remote `origin` of the current directory.

These tests hold the front ends together. Add cases to them when you add an
option or a check:

| Test | What it checks |
|---|---|
| `TestParity` in the root package | Every flag and positional maps to an options field, and every field to a flag or positional. |
| `TestMCPParity` in the root package | Each tool's input schema has exactly the options fields plus the transport fields, with the same required fields. |
| `TestSameValidation` in `internal/mcpserver` | Each tool rejects the same invalid input as its CLI command, with the same message. |
| `TestRejectsInputsWithoutCLIEquivalent` in `internal/mcpserver` | The MCP server rejects inputs that only the CLI's transport can carry, such as `cp -`. |

## Keep the security boundaries

- **The token.** runspace reads its GitHub token only from `-T`/`--token` or
  `RUNSPACE_TOKEN`, never from `GITHUB_TOKEN` or `GH_TOKEN`. The API client sends it only
  to the API host, even across a redirect, and the Dev Tunnels client never
  sends it. Tests in `internal/github` check all three. Never print, log, or
  write a token, including in tests and error messages.
- **The environment.** Of the host's environment, a remote command gets
  only the `KEY=VALUE` entries that the caller passes. runspace doesn't copy
  its own environment into a remote command. The one exception is `exec -t`, which sends the local `TERM` as
  the terminal type.
- **Downloads.** Files that `cp` downloads come from the sandbox, so
  `internal/transfer` treats them as hostile. It stages them in a temporary
  directory next to the destination, writes only through an `os.Root`,
  accepts only regular files and directories, and leaves nothing at the
  destination until the transfer succeeds. Keep these properties when you
  change `cp`.
- **Imports.** `TestImportGraph` in the root package checks the direct
  imports of production code:
  - No package imports `os/exec`. One dependency can still start a
    process: lipgloss, which the logger uses, may run `tmux info` at
    startup when stdout is a terminal inside tmux, to detect its colors.
  - Only `internal/csconn` imports the Dev Tunnels and gRPC modules.
  - The front ends don't import `internal/remote`, `internal/github`, or
    `internal/csconn`.
  - `internal/github` and `internal/transfer` use only the standard library.

## Log through the logger

runspace logs with [charm.land/log/v2](https://github.com/charmbracelet/log).
Each package gets its `*log.Logger` from its caller, as `sandbox.Config`,
`remote.DialConfig`, and `mcpserver.Serve` do. Don't write messages to
stderr yourself, and don't prefix them with `runspace:`.

- Use `Info` for what a person running a command should see, such as
  progress; `Warn` when they must act; and `Debug` for detail that helps
  diagnose a failure.
- Return a runspace failure as an error. `main` logs it once, and the MCP
  server returns it to the client.
- Never call `Fatal`: it exits 1 and skips the exit codes of `exec` and
  `run`.
- Never log a token, an environment value, a command line, or file
  contents. An error can quote its input, so the MCP server logs that a
  call failed, not why; the client gets the error itself.
  `TestDebugLogHoldsNoSecrets` in the root package and
  `TestToolCallsAreLogged` in `internal/mcpserver` check this.

## Test your change

Add tests for the behavior you change. Unit tests never reach GitHub or the
network. `internal/sandbox/sandboxtest` provides in-memory fakes of the
Codespaces REST endpoints that runspace calls and of the SSH connection. The
tests in `internal/sandbox`, `internal/mcpserver`, and the root package
build on them. Because the fakes are in memory, tests of timeouts and idle
timers can run under `testing/synctest`.

Before you open a pull request, run:

```sh
make check
```

`make check` checks formatting, runs `go vet` and golangci-lint for Linux,
macOS, and Windows, runs the unit tests with the race detector, and checks
that `go.mod` is tidy. It needs no token. CI doesn't check formatting,
run golangci-lint, or check `go.mod`, and it runs `go vet` only for Linux,
so run `make check` yourself.

### Fix lint findings

`make lint` enables every golangci-lint linter (`--default=all`), and the
repository has no golangci-lint configuration. Don't add one. Fix each
finding in the code. If a finding can't be fixed, suppress it where it
occurs and give the reason:

```go
//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
```

`make lint` skips the live checks in `test/live`, which build only with the
`live` tag. `make vet` covers them.

### Run the live checks

The live checks in `test/live` run the runspace binary against real
codespaces.

> [!WARNING]
> The live checks create billable codespaces. When they finish, they delete
> every codespace in the repository they use, including codespaces that
> runspace didn't create. Point them at a throwaway repository with
> `RUNSPACE_LIVE_REPO`; the default repository is the maintainer's.

The live checks need:

- `RUNSPACE_TOKEN`, a token with the `codespace` scope.
- `RUNSPACE_LIVE_REPO`, your throwaway repository, as `OWNER/REPO`.
- `gh`, signed in to the same account as `RUNSPACE_TOKEN`. The checks use
  it to set up some cases and to delete codespaces when they finish.
- Docker, for the check that runs `runspace mcp` as another user. Docker
  Desktop shows shared files as owned by whoever reads them, so the check
  skips there.

To run the live checks, replace `OWNER/REPO` and run:

```sh
RUNSPACE_TOKEN="$(gh auth token)" RUNSPACE_LIVE_REPO=OWNER/REPO make live
```

Keep the computer awake until the run ends. If it sleeps, the checks that
are running fail with timeouts.

`make live` skips the waits for idle timeouts: it skips `TestIdleBehavior`,
and `TestMCPSession` stops before its idle wait. To run them too, as before
a release, set `LONG=1`, which adds 15 to 30 minutes. `TestIdleBehavior`
runs only with `LONG=1`. The checks that create their own sandbox run in
parallel, two at a time, which keeps a run below GitHub's limit on how many
codespaces an account can run at once.

To run only some checks, set `RUN` to a pattern of test names, for example
`RUN=TestExitCodes`. `make canary` runs the shorter set that the weekly
canary workflow uses to detect changes in the undocumented connection
protocol. Both targets stop before building anything if `RUNSPACE_TOKEN` is
unset.

## Change internal/csconn

`internal/csconn` is a pinned subset of the GitHub CLI's
`internal/codespaces` package at v2.100.0. It may differ from upstream only
by the edits logged in [UPSTREAM.md](../internal/csconn/UPSTREAM.md). When
you change it, add an entry to the edit log and refer to it in the file map.
Then compare the copy with the pinned release:

```sh
make upstream-diff
```

Every difference it prints must match a logged edit. To compare with
another release, for example before updating the copy, set `UPSTREAM_TAG`.

## Add a dependency

A release fails if a linked package has a forbidden, restricted, or unknown
license. After you add or update a dependency, run `make tidy`, then check
the licenses for every release target:

```sh
make licenses
```

## Write documentation

`README.md` is for people who use runspace. Keep its MCP instructions
usable with any MCP client: describe what every client needs, and when an
example uses one client, name it and say what to check in others. The MCP
tool descriptions are in `internal/mcpserver/descriptions.go`.

## Open a pull request

Open your pull request against `base`. CI runs `go vet`, the race tests, and
a snapshot build of every release archive on each pull request.

## Release a version

Maintainers release runspace:

1. Push a `v*` tag. The release workflow builds the archives for Linux,
   macOS, and Windows, on amd64 and arm64, into a draft GitHub release.
2. Check the draft, then publish it.

The binary takes its version from the tag, which Go stamps into the build
information. To build the same archives into `dist/` without publishing
anything, run `make snapshot`, which needs
[goreleaser](https://goreleaser.com).

<!-- # runspace -->

<p align="center">
  <img height="100" alt="runspace" src="https://i.postimg.cc/NMLmSw-5Y/runspace-1.png" />
</p>
<p align="center">
  <a href="https://github.com/sandboxec/runspace/actions/workflows/tests.yml"><img height="100" alt="runspace" src="https://github.com/sandboxec/runspace/actions/workflows/tests.yml/badge.svg" /></a>
  <a href="https://github.com/sandboxec/runspace/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/sandboxec/runspace?color=c8ff00" /></a>
  <a href="/LICENSE"><img alt="License" src="https://img.shields.io/badge/License-MIT-00add8" /></a>
  <!-- <img alt="Platform" src="https://img.shields.io/badge/platform-linux%20%2F%20macOS%20%2F%20windows-c8ff00" /> -->
<p align="center">
  Disposable cloud sandboxes for humans and AI agents, powered by GitHub Codespaces.
</p>

runspace runs commands in disposable GitHub codespaces, which it calls
*sandboxes*, so dependencies, build scripts, and tests execute on a
codespace's VM instead of on the *host*, the machine where you or your agent
run runspace. It is a single static binary with two front ends over the same
commands:

- a CLI for humans (and scripts)
- a MCP server for AI agents

An agent on the host uses it to create a sandbox, run every command there,
copy files in and out, and take its work back as a patch. runspace needs
only a GitHub token: there is no daemon, no local state, and no
repository configuration.

> [!CAUTION]
> runspace uses the undocumented codespace connection protocol,
> which is can change without notice.

## How it works

1. `create` asks the REST API for a codespace tagged `runspace`, waits until
   it is available, and records the checked-out commit as
   `refs/runspace/base` inside the sandbox.
2. Each command opens an SSH connection through the codespace's Dev Tunnel,
   with a key that exists only in memory, and runs in `/workspaces/REPO`.
3. `diff` returns everything that changed since the base commit, and
   `delete` removes the sandbox.

runspace acts only on codespaces whose display name is `runspace`, so it
never touches your other codespaces. Code running inside a sandbox is not
bound by that rule; see [Limitations](#limitations).

## Install

* from source with Go 1.26 or later:

  ```sh
  go install go.sandbox.ec/runspace@v0.1.0
  ```

* or download a prebuilt binary from the 
[releases page](https://github.com/sandboxec/runspace/releases).

## Authenticate

runspace reads its token from `-T`/`--token`, or else from `RUNSPACE_TOKEN`.
The token needs the `codespace` scope.

> [!IMPORTANT]
> Prefer the environment variable because it keeps the token out of your
> shell history and process list.
> 
> TODO(dwisiswant0): store the token in a keyring.

With the GitHub CLI:

```sh
gh auth refresh -h github.com -s codespace # if your token lacks the scope
export RUNSPACE_TOKEN="$(gh auth token)"
```

Verified: a `gh` OAuth token that includes the `codespace` and `repo` scopes.

> [!NOTE]
> A classic personal access token with only the `codespace` scope is expected
> to work but has not been tested. And GitHub reports errors with fine-grained
> tokens ([cli/cli#7819](https://github.com/cli/cli/issues/7819)).

## Quick start

In these examples, replace `OWNER/REPO` with your repository and `BRANCH`
with a branch in it. A sandbox checks out the branch from
GitHub, not your working tree, so push your changes first!

Run one command in a fresh sandbox, then delete it:

```sh
runspace run -R OWNER/REPO -b BRANCH make test
```

> [!NOTE]
> Inside a git repository whose `origin` is on github.com, you can leave
> the `-R`/`--repo` flag off.

Keep the changes a command makes:

```sh
name=$(runspace create -R OWNER/REPO -b BRANCH)
runspace exec "$name" ./scripts/format.sh
runspace diff -o format.patch "$name"
runspace delete "$name"
git apply --stat --summary format.patch
git apply format.patch
```

Work interactively:

```sh
name=$(runspace create -R OWNER/REPO -b BRANCH)
runspace exec -t "$name" bash
runspace delete "$name"
```

## Commands

| Command | What it does |
|---|---|
| `create [-R OWNER/REPO]` | Creates a sandbox and waits until it is ready. Prints its name, or JSON with `--json`. |
| `exec NAME CMD [ARG...]` | Runs a command. `-i` attaches stdin, `-t` allocates a terminal, `-e KEY=VALUE` sets a variable, and `-w DIR` sets the working directory. |
| `run [-R OWNER/REPO] CMD [ARG...]` | Creates a sandbox, runs one command, and deletes it. `--keep always`, or `--keep failure` after a non-zero exit, stops it instead. |
| `diff NAME` | Prints the patch since `create`: commits, uncommitted edits, and new files. `-o FILE` writes it atomically. `--timeout` bounds it. |
| `cp SRC DST` | Copies between the host and a sandbox; one side is `NAME:PATH`. `-L` follows symbolic links, and `--timeout` bounds the transfer. |
| `ls` | Lists runspace's sandboxes. `-q` prints names only, and `--json` prints JSON. |
| `delete NAME...` | Deletes sandboxes, or every runspace sandbox with `--all`. |
| `mcp` | Serves the commands as MCP tools over stdio. |

In `exec` and `run`, flags come before `NAME` or `CMD`; everything after
belongs to the remote command. Run `runspace COMMAND --help` for every flag.
Every command also takes `-T`/`--token`; see [Authenticate](#authenticate).

<!-- Without `-R`, `create` and `run` take `OWNER/REPO` from the git remote
`origin` of the current directory and log
`Using OWNER/REPO from git remote origin`. They ask for
`-R` when the current directory is not in a git repository, when `origin` is
not on github.com, or when the repository's files belong to another user.
runspace reads only the repository's own git config and applies no
`insteadOf` rewrites, so an `origin` that relies on one needs `-R`. -->

The defaults limit how long a sandbox runs and how long it is kept. You can
change each one per call:

| Setting | Default |
|---|---|
| `--timeout` (`exec`, `run`, `cp`, `diff`) | 2 hours; `0` disables it |
| `--idle` | 10 minutes |
| `--retention` | 24 hours after a sandbox stops |
| `-m`, `--machine` | `basicLinux32gb` (2 cores) |
| `--keep` (`run`) | `never` |

### Copy files

`DST` names the result, unlike Docker: `runspace cp src NAME:dst` writes
`src` as `dst`, never as `dst/src`, whether or not `dst` exists. Relative
sandbox paths start at `/workspaces/REPO`. `-` streams a tar archive:
`cp - NAME:DIR` extracts stdin into `DIR`, and `cp NAME:SRC -` writes `SRC`
to stdout.

Downloads come from code you don't trust, so they are strict:

- Only regular files and directories are accepted. Each file gets mode
  `0644`, or `0755` if any execute bit was set, before the umask applies.
- Nothing appears at `DST` until the whole copy succeeds.
- A download never merges into an existing directory.

> [!TIP]
> For larger copies, pass a longer `--timeout`, or `--timeout 0` for no limit.

### Exit codes

`exec` and `run` pass the remote command's exit code through, as Docker
does:

| Code | Meaning |
|---|---|
| The command's code | The command ran. This includes 126 (not executable), 127 (not found), and 128+n (killed by signal n). |
| 124 | `--timeout` expired. |
| 125 | runspace itself failed, including a usage error, or the `-w` directory does not exist. |
| 130 | Interrupted. |

A command that exits 125 itself looks like a runspace failure. Every other
runspace command exits 0 or 1.

### Logs

runspace logs its own messages to stderr: errors, warnings, and progress
lines. A command's output stays on stdout, and its own stderr on stderr.

| Setting | Effect |
|---|---|
| `RUNSPACE_LOG_LEVEL` | `debug`, `info` (the default), `warn`, `error`, or `fatal`. `warn` hides progress and other informational lines, and `fatal` hides everything, as runspace logs nothing at that level. |
| `-v`/`--verbose` | Logs at `debug`. `RUNSPACE_LOG_LEVEL` is then not read. |
| `RUNSPACE_LOG_OUTPUT` | A file to append the log to instead of stderr. Every value is a path. `runspace mcp` refuses a path to `/dev/stdout`, because its stdout carries the MCP protocol. |

The log has colors only on a terminal, and `NO_COLOR` turns them off. Debug
lines name sandboxes, connection steps, and exit codes. They never include a
token, an environment value, or a command line.

## Use runspace with an AI agent

`runspace mcp` exposes `create`, `exec`, `run`, `diff`, `cp`, `ls`, and
`delete` as MCP tools, with parameters named after the CLI's long flags.
`stdin` replaces `-i`, and `-t`, `--json`, `-q`, and `-` have no MCP form.
No tool takes a token: the server uses the one it started with.

The tools carry your full authority: an agent can read and write any file
its user can reach, and copy it into a sandbox. It
can create sandboxes from any repository the token's identity can read,
including any public repository, at that account's expense, and act on every
runspace sandbox of that identity. Over MCP, `create` and `run` ALWAYS take
an explicit `repo`. Anything a sandbox returns can carry instructions that
steer the agent.

runspace runs the agent's commands in sandboxes, as long as the harness
gives the agent only the tools it needs.

### Recommended setup

1. Create a local user, such as `agent`, that holds only the harness, task
   directories, and the token. A container with its own user works too.
2. Create a GitHub machine account, and a token for it with the `codespace`
   scope and a short expiry (a scope-limited token is not yet verified; see
   [Authenticate](#authenticate)). Export it as `RUNSPACE_TOKEN` for the
   agent user. Codespaces of a personal repository are paid for by the
   account that creates them, here the machine account.
3. Create a private sandbox repository, such as `me/proj-sandbox`, add the
   machine account as a collaborator, and disable Actions for it.
4. Add the sandbox repository as a remote of your project, and push each task
   branch to it.
5. In the machine account's Codespaces settings, turn off dotfiles, add no
   secrets for the sandbox repository, and leave it out of GPG verification.
6. In the machine account's billing settings, set a Codespaces budget.

### Configure the harness

Add `runspace mcp` to your MCP client as a stdio server, with
`RUNSPACE_TOKEN` in its environment. This example uses the common
`mcpServers` format; adapt it to your client's, and replace
`/path/to/runspace` with your runspace path:

```json
{
  "mcpServers": {
    "runspace": {
      "command": "/path/to/runspace",
      "args": ["mcp"],
      "env": {
        "RUNSPACE_TOKEN": "${RUNSPACE_TOKEN}"
      }
    }
  }
}
```

Clients that expand variables in their config, such as Claude Code, fill in
`${RUNSPACE_TOKEN}` from their own environment, so the token never lands in
the file; check how your client does it. A client may also pass the server
its whole environment, model key included. Nothing in it reaches a sandbox:
`exec` and `run` take only `KEY=VALUE` entries, so the agent can set only
values it already has.

Then run the agent as the agent user, with only the runspace tools and no
host execution or network tools. For example, with Claude Code and the
config saved as `runspace-mcp.json` in the task directory:

```sh
claude -p "TASK" --restricted --strict-mcp-config --mcp-config runspace-mcp.json \
  --permission-mode dontAsk --allowedTools "mcp__runspace__*" \
  --disallowedTools Bash WebFetch WebSearch
```

When the client asks for progress, as Claude Code does, every tool call
sends a progress notification each minute while it waits or runs, so the
harness doesn't time out a long command for being quiet.

The server logs to stderr. To keep its log in a file instead, set
`RUNSPACE_LOG_OUTPUT` in the server's `env`; see [Logs](#logs). With
`RUNSPACE_LOG_LEVEL=debug`, it logs a line for each tool call.

In the prompt, ask the agent to create a sandbox with the repository, branch,
and `sha`; to run every command there; and when done, to call `diff` with an
`output` path and then `delete`.

Afterwards:

- Run `runspace ls -q` and delete any leftovers.
- Review the patch from your own account before you apply it. Check
  `git apply --stat --summary` for symlinks (mode `120000`) and for changes
  to `.github/`, `.husky/`, or `.pre-commit-config.yaml`.

## Limitations

- **Undocumented protocol.** The connection depends on an internal REST
  query, the codespace agent's gRPC service, and Dev Tunnels. Any of them can
  change without notice.
- **No egress control.** A sandbox can reach the internet. Its own
  `GITHUB_TOKEN` can read and write its source repository.
- **Sandboxes share one trust domain.** Code in a sandbox can use the
  codespace's own `GITHUB_TOKEN` to run commands in every other codespace of
  the same identity and repository, and to open their private forwarded
  ports. Use a dedicated identity and sandbox repository, run one agent
  session per identity at a time, and keep your own codespaces off that
  identity.
- **Untrusted output.** Output can carry terminal escape sequences, and
  patches can add symlinks or edit CI and hook configuration.
- **No size limit on downloads.** `cp` downloads and `diff -o` write as much
  as the sandbox sends. A copy that fills the disk fails and removes what it
  staged, but other writes to that filesystem can fail meanwhile. Give the
  agent user its own filesystem or quota if that matters. `--timeout` bounds
  how long a sandbox can keep a transfer going.
- **SSH server.** A repository's own devcontainer config must provide an SSH
  server, for example with the `ghcr.io/devcontainers/features/sshd:1`
  feature. The default image, used when there is no config, has one.
- **Readiness.** runspace waits for the checkout, not for a repository's own
  `postCreateCommand`.
- **Loopback listener.** While connected, runspace listens on a `127.0.0.1`
  port that other local users can reach.
- **Platform limits.** A codespace runs for at most 12 hours at a time and
  idles out after 5 to 240 minutes. runspace works with github.com only.

## Contributing

To build runspace, run its checks, or change it, see
[docs/CONTRIBUTING.md](docs/CONTRIBUTING.md).

## License

runspace is available under the [MIT License](LICENSE).

`internal/csconn` contains code from the GitHub CLI, also under the MIT
License; see [internal/csconn/LICENSE](internal/csconn/LICENSE).

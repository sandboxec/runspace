# Upstream

`internal/csconn` is a pinned subset of the GitHub CLI's codespaces package.
It differs from upstream only by the edits logged below.

| | |
|---|---|
| Module | `github.com/cli/cli/v2` |
| Directory | `internal/codespaces/` |
| Tag | `v2.100.0` |
| Commit | `45437bc7eeeb3359bbfddd1742f79de7652fd3e2` |
| License | MIT, copied verbatim to `LICENSE` in this directory |

## File map

Paths are relative to `internal/codespaces/` upstream and to `internal/csconn/` here.

| Upstream | Ours | Notes |
|---|---|---|
| `LICENSE` (repository root) | `LICENSE` | verbatim |
| `api/api.go` | `api/api.go` | edited, see 2-6, 8, 9, 11, 19 |
| `api/api_test.go` | `api/api_test.go` | edited, see 6, 12, 19 |
| `codespaces.go` | `codespaces.go` | edited, see 1, 10, 18, 19 |
| `codespaces_test.go` | `codespaces_test.go` | edited, see 1, 10, 18, 19 |
| `connection/connection.go` | `connection/connection.go` | edited, see 1, 7, 19 |
| `connection/connection_test.go` | `connection/connection_test.go` | edited, see 1, 13, 19 |
| `connection/tunnels_api_server_mock.go` | `internal/tunnelstest/tunnels_api_server_mock.go` | moved, see 13, 19 |
| `portforwarder/port_forwarder.go` | `portforwarder/port_forwarder.go` | edited, see 1, 14, 19 |
| `portforwarder/port_forwarder_test.go` | `portforwarder/port_forwarder_test.go` | edited, see 1, 13, 14, 19 |
| `rpc/invoker.go` | `rpc/invoker.go` | edited, see 1, 7, 15, 16, 17, 19 |
| `rpc/invoker_test.go` | `rpc/invoker_test.go` | edited, see 1, 15, 19 |
| `rpc/generate.sh` | `rpc/generate.sh` | edited, see 15 |
| `rpc/generate.md` | `rpc/generate.md` | edited, see 1 |
| `rpc/codespace/*` | `rpc/codespace/*` | verbatim (`.proto`, `.pb.go`, `_grpc.pb.go`, moq mock) |
| `rpc/ssh/*` | `rpc/ssh/*` | verbatim |
| `rpc/test/port_forwarder.go` | `rpc/test/port_forwarder.go` | edited, see 1, 14, 19 |
| `rpc/jupyter/*` | excluded | Jupyter is not used |
| `ssh.go`, `ssh_test.go` | excluded | run the local `ssh` and `scp` binaries through `os/exec` |
| `states.go` | excluded | post-create state polling for the CLI |
| none | `UPSTREAM.md` | this file |
| none | `api/runspace_test.go` | tests for edits 2, 5, 11 |
| none | `rpc/export_test.go` | test hooks for runspace's tests |
| none | `rpc/runspace_test.go` | tests for edit 16 |

The pb and gRPC files in `rpc/codespace/` and `rpc/ssh/` contain no import paths,
so they are byte-identical to upstream. They still contain the
`RebuildContainerAsync` RPC; only the invoker method was removed.

## Edit log

M means mechanical and F means functional. D-rows are in the runspace plan.

1. M, every copied Go file that imports `internal/codespaces/...`, and `rpc/generate.md`: import path
   `github.com/cli/cli/v2/internal/codespaces/...` becomes
   `go.sandbox.ec/runspace/internal/csconn/...`. `gofmt` then re-sorts the
   import blocks, which moves some import lines. In `rpc/generate.md` the directory
   `internal/codespaces/rpc` becomes `internal/csconn/rpc`.
2. M, `api/api.go`: `api.HandleHTTPError` (which wraps go-gh) is replaced by a local
   `handleHTTPError` that returns `*StatusError` (status code and the `message` field of
   the JSON body, or else the status line from `resp.Status`, as go-gh does). The 200 check in `GetCodespace` and the 409
   handling in `StartCodespace` are unchanged.
3. M, `api/api.go`: `safeurl.JoinPathWithHostPrefix` becomes `url.JoinPath` on the API
   base URL, through the new helper `codespaceURL`. The URL is built once before
   `withRetry` instead of inside the closure. `RepoPartsFromNWO` is not needed.
4. M, `api/api.go`: the opentracing span in `do` is removed. `do` no longer takes the
   unused `spanName` argument, and the call sites change to match.
5. M, `api/api.go`: `New(f *cmdutil.Factory)` becomes
   `New(client, externalClient *http.Client, apiURL string) *API`. The `API` struct holds
   `*http.Client` values instead of factory functions. The first client carries GitHub
   credentials and is supplied by the caller. The second is for Dev Tunnels and never
   carries GitHub credentials. `ExternalHTTPClient()` returns the second and keeps its
   `(*http.Client, error)` signature. `retryBackoff` stays (100ms by default). `TestRetries` leaves it
   out of its struct literal, so it is zero there. The `GITHUB_API_URL`, `GITHUB_SERVER_URL` and config lookups,
   `ServerURL()`, `githubServer`, `defaultAPIURL` and `defaultServerURL` are removed.
6. M, `api/api.go`, `api/api_test.go`: every other endpoint and its unused types are
   removed: `GetUser`, `GetRepository`, `ListCodespaces`, `GetOrgMemberCodespace`,
   `StopCodespace`, machines, permissions check, repo suggestions, billable owner,
   create, delete, dev containers, edit, pending-operation check, repository contents,
   `findNextPage` and their option, parameter and result types. The CLI field lists
   `ListCodespaceFields` and `ViewCodespaceFields` and the file-level reorganization
   TODO are removed too. In `api_test.go` the tests of removed code, the `TestNew_*`
   tests and the `createHttpClient` helper are removed.
   `User`, `Repository`, `RepositoryOwner`, `Codespace` and its related types,
   `ExportData` and the state and `VSCSTarget*` constants are kept.
7. M, `connection/connection.go`, `rpc/invoker.go`: `clientName` changes from `"gh"` to
   `"runspace"`. It is the Dev Tunnels User-Agent name and the activity client ID.
8. M, `api/api.go`: unused imports (`bytes`, `base64`, `errors`, `os`, `strconv`, the
   `cli/cli` packages, opentracing) are removed along with the code that used
   them.
9. M, `api/api.go`: `GetCodespace` and `StartCodespace` return the `codespaceURL` error
   as is. Upstream returned the URL error from inside the retry closure, so it carried
   the "error making request" prefix.
10. M, `codespaces.go`, `codespaces_test.go`: package clause `codespaces` becomes
    `csconn`. `ListenTCP` and `TestListenTCPInterfaces` are removed, along with the
    `net` and testify imports that only they used.
11. F, `api/api.go`: `codespaceURL` rejects codespace names that do not match
    `^[A-Za-z0-9-]+$` with an error, before any URL is built. Upstream's
    `safeurl.JoinPath` refused `..`; `url.JoinPath` would clean it. Tested in
    `api/runspace_test.go`.
12. M, `api/api_test.go`: `TestRetries` builds `API` with `client: &http.Client{}`
    instead of a factory function. Its logic is unchanged.
13. M, `internal/tunnelstest/tunnels_api_server_mock.go`: the Dev Tunnels mock server
    moves from `connection/` to `internal/tunnelstest/` and its package clause becomes
    `tunnelstest`. It does not use unexported symbols of `connection`, so no code
    changed. Production code does not import it. `connection_test.go` and
    `port_forwarder_test.go` call `tunnelstest.NewMockHttpClient` and
    `tunnelstest.WithSpecificPorts` instead of the `connection` ones.
14. M, `portforwarder/port_forwarder.go`, `rpc/test/port_forwarder.go`:
    `ListPorts` and `UpdatePortVisibility` are removed, from the `PortForwarder`
    interface too. The `tunnels` import in `rpc/test/port_forwarder.go` is removed
    because only they used it. `AccessControlEntriesToVisibility` and `IsInternalPort`
    are kept with their tests. In `port_forwarder_test.go`, two tests asserted the
    forwarded port's protocol through `fwd.ListPorts`; they now call
    `connection.TunnelManager.ListTunnelPorts` directly.
15. M, `rpc/invoker.go`, `rpc/invoker_test.go`, `rpc/generate.sh`: the Jupyter
    client, `StartJupyterServer`, `isJupyterServerURLValid`, `RebuildContainer`, their
    interface entries, imports, mock-server registration and tests are removed, and the
    `jupyter` line is removed from `generate.sh`. The file header comment now says
    "Start a remote SSH server".
16. F, `rpc/invoker.go`: `StartSSHServerOptions` gains `UserPublicKey string`.
    When non-empty it is sent as `StartRemoteServerRequest.UserPublicKey` as given
    (not trimmed) and `UserPublicKeyFile` is not read. Otherwise the file is read and
    trimmed as before. Tested in `rpc/runspace_test.go`.

17. F, `rpc/invoker.go`: `Close` always closes the gRPC `ClientConn`. Upstream
    closed it only when closing the listener failed, so every connection leaked a
    client that kept retrying; a long-lived MCP server would accumulate them.
18. F, `codespaces.go`, `codespaces_test.go`: the package variable
    `codespaceStatePollingBackoff` becomes the package variable
    `newCodespaceStatePollingBackoff`, a constructor that tests replace, so
    each wait gets a fresh backoff. Upstream
    shared one stateful backoff, a data race when two waits run at once.

19. M, every copied Go file that is not generated: a first line
    `//nolint:all // Pinned upstream copy; edits are logged in UPSTREAM.md.`
    keeps runspace's linters off upstream code, which follows upstream's own
    style. Generated files are skipped by the linters already. runspace's own
    tests here (`api/runspace_test.go`, `rpc/runspace_test.go`) are linted.

## Known upstream behavior kept

These were noticed during the copy and are not fixed, to keep the diff to the log above.

- `api.withRetry` does not close the response body of a 5xx attempt before retrying.
- `invoker.keepAliveOverride` is a plain `bool` written by `KeepAlive` and read by the
  heartbeat goroutine without synchronization.
- `invoker.appendMetadata` sends the literal `Authorization: Bearer token` to the
  internal gRPC server.
- `invoker.connect` assigns the shared `err` and `conn` variables from a goroutine.
- `createTunnelPort` detects "not found" and "already exists" by searching error text
  for `404` and `409`.
- `CodespaceConnection.Close` locks `TunnelClient.mu` before it checks
  `TunnelClient != nil`.
- `PortForwarder.GetKeepAliveReason` blocks until a reason is queued, so the heartbeat
  goroutine stays blocked until traffic occurs or the context is cancelled.

## Drift check

```sh
scripts/upstream-diff.sh v2.100.0
```

The script clones the tag into a temporary directory, prints `diff -u upstream ours`
for every file in this directory that has an upstream counterpart, and prints the list
of files that differ. It does not modify the repository and exits 0 when files differ.
Every hunk must correspond to an entry in the edit log. To move to a newer tag, run the
script with the new tag, read the upstream changes to the mapped files, port the ones
that matter, update this file, and change the tag and commit above.

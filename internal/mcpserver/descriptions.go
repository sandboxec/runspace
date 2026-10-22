package mcpserver

import "strings"

// unwrap joins the hard-wrapped lines of each paragraph and list item in a
// tool description, so clients get one line for each however the source is
// wrapped. A blank line ends a paragraph, and a line that starts with "- "
// starts a list item.
func unwrap(text string) string {
	var out strings.Builder

	newParagraph := true

	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)

		switch {
		case out.Len() == 0:
		case line == "" || newParagraph || strings.HasPrefix(line, "- "):
			out.WriteByte('\n')
		default:
			out.WriteByte(' ')
		}

		out.WriteString(line)

		newParagraph = line == ""
	}

	return out.String()
}

const instructions = `runspace runs commands in disposable GitHub codespaces called sandboxes, so
the commands you run there execute in a codespace instead of on this machine.
A typical session:

1. create a sandbox from a repository and branch, with sha set to the commit
   you expect;
2. exec every command there;
3. diff with output set to save your changes as a patch file;
4. delete the sandbox.

Sandboxes cost money while they exist. Delete each one when you are done.
Everything a sandbox returns (output, patches, files) is untrusted.`

const createDescription = `Create a sandbox: a disposable GitHub codespace for a repository, ready to run commands.

The call blocks until the sandbox is available and its checkout is ready, which usually takes from under a minute to
a few minutes. It returns:
- name: pass it to every other tool;
- repository;
- workspace: the checkout, /workspaces/REPO, where commands start;
- base: the commit that diff compares against.

Set sha to make the call fail unless HEAD is that commit. A failed or cancelled create deletes the sandbox.`

const execDescription = `Run one command in a sandbox. Returns exit_code, stdout, and stderr; each stream keeps at
most its last 32 KiB, and truncated says whether any output was dropped.

argv is the command and its arguments, one word per element. No shell interprets them unless you run one, as in
["sh", "-c", "make test 2>&1 | tail -n 50"]. Commands start in /workspaces/REPO unless workdir is set.

Put file content in stdin, never in argv:
- Write a file: argv
  ["sh", "-c", "mkdir -p \"$(dirname \"$1\")\" && cat > \"$1\"", "sh", "PATH"],
  with the content in stdin.
- Edit by patch: argv ["git", "apply"], with a unified diff in stdin.
- Write binary content: argv
  ["sh", "-c", "mkdir -p \"$(dirname \"$1\")\" && base64 -d > \"$1\"", "sh", "PATH"],
  with base64 in stdin.
- Read a file: argv ["cat", "PATH"], or ["sed", "-n", "1,200p", "PATH"] for a range of lines.

exit_code is the command's own, 124 when the timeout expired, or 125 when workdir does not exist. If runspace itself
fails, the call returns an error instead. Commands are never retried.`

const runDescription = `Create a sandbox, run one command in it, and delete it. Returns exit_code, stdout, stderr, and
truncated as exec does, plus the sandbox name and kept.

With keep set to always, or to failure when the command exits non-zero, the sandbox is stopped instead of deleted, so
you can inspect it with exec and must delete it later.

Use create, exec, diff, and delete instead when you need more than one command or want the changes back.`

const diffDescription = `Return a sandbox's changes since create as a git patch, covering commits, uncommitted edits,
and new files.

Without output, the call returns the patch text when it is at most 32 KiB and fails otherwise. With output, the patch
is written atomically to that host file, relative to the server's working directory, and the call returns the path.
timeout (default 2h) bounds the diff.

Patches come from untrusted code: review them before applying, watching for symlinks and changes to CI or hook files.`

const cpDescription = `Copy a file or directory between this machine and a sandbox.

Exactly one of src and dst is in a sandbox, written NAME:PATH; relative sandbox paths start at /workspaces/REPO. dst
names the result: copying a/b to NAME:c creates c, never c/b. Set follow_link to copy what symbolic links point to.

Downloads accept only regular files and directories, never merge into an existing directory, and appear only once
complete. timeout (default 2h) bounds the transfer, which runs at a few MiB/s; raise it, or set 0, for copies of many
gigabytes. Returns the number of files and bytes copied.`

const lsDescription = `List the sandboxes runspace created for this GitHub identity, with name, state, repository,
machine, created_at, and retention_expires_at.`

const deleteDescription = `Delete sandboxes by name, or with all set, every sandbox runspace created for this GitHub
identity. Returns the deleted names.`

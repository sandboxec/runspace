package transfer_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	"go.sandbox.ec/runspace/internal/transfer" //nolint:depguard // the external test package imports the package under test
)

// An upload trusts each file's stat size, and procfs reports 0 for a
// process's environment, so cp cannot send the environment of a process
// that runs as the same user, such as the MCP client and its model key, into
// a sandbox. Reading files to the end instead would break that.
func TestWriteTarRefusesProcEnviron(t *testing.T) {
	t.Parallel()

	const environ = "/proc/self/environ"

	info, err := os.Stat(environ)
	if err != nil || !info.Mode().IsRegular() || len(os.Environ()) == 0 {
		t.Skipf("no readable %s with content: %v", environ, err)
	}

	var buf bytes.Buffer

	_, err = transfer.WriteTar(&buf, environ, "environ", false)
	contains(t, err, environ)

	if leaked := os.Environ()[0]; bytes.Contains(buf.Bytes(), []byte(leaked)) {
		t.Errorf("the archive holds %q from the environment", leaked)
	}

	if info.Size() != 0 {
		t.Errorf("%s reports size %d; this test assumes procfs reports 0", environ, info.Size())
	}
}

// A file that holds fewer bytes than its stat size, as sysfs attributes do
// and a file truncated during the upload would, fails naming that file, not
// the next entry.
func TestWriteTarShrunkFile(t *testing.T) {
	t.Parallel()

	const attr = "/sys/devices/system/cpu/online"

	info, err := os.Stat(attr)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		t.Skipf("no sysfs attribute %s with a stat size: %v", attr, err)
	}

	_, err = transfer.WriteTar(io.Discard, attr, "online", false)
	contains(t, err, attr, "file shrank while being read")
}

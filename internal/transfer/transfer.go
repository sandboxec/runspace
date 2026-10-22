// Package transfer moves files between the host and a sandbox as tar streams.
//
// Uploads carry the caller's own data, so [WriteTar] places no restrictions
// on content. Downloads carry content that the sandbox controls, so [Extract]
// stages it in a temporary directory next to the destination, writes only
// through an [os.Root], accepts only regular files and directories, and
// leaves nothing at the destination until [Staged.Commit].
package transfer

// Stats counts what a transfer carried.
type Stats struct {
	Files int   // regular files
	Bytes int64 // regular-file content bytes
}

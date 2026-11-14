//go:build windows

package sandboxjournal

// Windows local development does not provide the same directory fsync
// semantics as the Linux runner deployment target. File contents are still
// flushed with File.Sync; production runner durability requires Linux.
func syncDirectory(_ string) error {
	return nil
}

//go:build windows

package platform

// ponytail: ReadAtDirect on this platform is the cached ReadAt, so it observes
// the page cache, not what reached the device. Replace it when the platform
// gains its own direct read — F_NOCACHE on the descriptor on macOS,
// FILE_FLAG_NO_BUFFERING on a reopened handle on Windows — which the tail
// check after a crash needs to prove the end of a journal here.
func (f *file) readDirect(p []byte, off int64) (int, error) {
	return f.ReadAt(p, off)
}

func (f *file) closeDirectLocked() error {
	return nil
}

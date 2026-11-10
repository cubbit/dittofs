//go:build windows

package platform

// checkDirOS decides whether dir is suitable for a journal. It always
// answers nil here.
//
// decision nil as a placeholder, not as a verdict: on Windows a rule of
// the platform profile does apply — Open MUST refuse a volume whose
// write-cache buffer flushing is turned off by policy — and the package
// already holds its verdict, checkCacheProbe, which returns the same
// *refusal every rule there returns, so the directory's volume is reachable
// with GetVolumePathName, the property queried with
// IOCTL_STORAGE_QUERY_PROPERTY, and the verdict wired in; doing that is
// part of the Init/Open work that follows. Withdraw the no-op when
// checkCacheProbe is wired in here.
func checkDirOS(dir string) error {
	return nil
}

//go:build windows

package platform

// checkDirOS decides whether dir is suitable for a journal. It always
// answers nil here.
//
// ponytail: nil is a placeholder, not a verdict. On Windows a rule of the
// platform profile applies — Open MUST refuse a volume whose write-cache
// buffer flushing is turned off by policy — and it is not enforced here, so
// Init and Open accept such a volume and a write the caller believes durable
// may sit in a volatile cache. The package already holds its verdict,
// checkCacheProbe, which returns the same *refusal every rule there returns.
// Upgrade by reaching the directory's volume with GetVolumePathName, querying
// the write-cache property with IOCTL_STORAGE_QUERY_PROPERTY, building the
// probe input from it and running checkCacheProbe; withdraw the no-op then.
func checkDirOS(dir string) error {
	return nil
}

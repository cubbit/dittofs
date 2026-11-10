//go:build darwin

package platform

// CheckDir decides whether dir is suitable for a journal. It always
// answers nil on macOS.
//
// decision: nil because no rule of the platform profile refuses a
// directory on macOS: every sync runs fcntl(F_FULLFSYNC), which reaches the
// device whatever the volume's settings, and an APFS local snapshot or a
// thin-provisioned device beneath the filesystem — the two things that
// would void the headroom the way a refusal would — are named by the
// profile as health conditions the deployment owns, not errors an open can
// detect. Withdraw the no-op when a rule of the profile names something
// the check must read on the platform.
func CheckDir(dir string) error {
	return nil
}

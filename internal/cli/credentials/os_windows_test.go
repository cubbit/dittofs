//go:build windows

package credentials

import (
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The credentials file and its directory each carry a protected DACL whose
// one entry grants the current user.
func TestStoreACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dfsctl")
	path := filepath.Join(dir, "config.json")
	set(t, path, "a", &Context{ServerURL: "https://a.example", AccessToken: "secret"})
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, dir} {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatalf("%s: read its security: %v", p, err)
		}
		ctl, _, err := sd.Control()
		if err != nil {
			t.Fatal(err)
		}
		if ctl&windows.SE_DACL_PROTECTED == 0 {
			t.Errorf("%s: DACL not protected, want inherited entries refused", p)
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil {
			t.Fatalf("%s: DACL %v, %v", p, acl, err)
		}
		if acl.AceCount != 1 {
			t.Errorf("%s: %d entries, want 1, the current user's", p, acl.AceCount)
			continue
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(u.User.Sid) {
			t.Errorf("%s: entry %d for %v, want an allow for %v", p, ace.Header.AceType, sid, u.User.Sid)
		}
	}
}

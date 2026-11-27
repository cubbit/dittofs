package bootstrap

import (
	"io/fs"
	"os"
	"reflect"
)

// processUser is the user id this process runs as, the node's user that
// Validate hands to the state directory's check: the directory may belong to
// it as well as to root.
func processUser() uint32 {
	return uint32(os.Getuid())
}

// dirOwner reads the owner out of the facts the host reports for a file.
// It reports none where those facts carry no owner id, as on a platform
// whose file system records none.
//
// ponytail: the owner is read by field name, not by the unix type, because
// the frozen-surface check reads every file whatever its build tags, so one
// name cannot be declared once per platform; upgrade: a pair of tagged
// files once that check follows build constraints.
func dirOwner(info fs.FileInfo) (uint32, bool) {
	v := reflect.ValueOf(info.Sys())

	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return 0, false
	}

	uid := v.FieldByName("Uid")

	if !uid.IsValid() || !uid.CanUint() {
		return 0, false
	}

	return uint32(uid.Uint()), true
}

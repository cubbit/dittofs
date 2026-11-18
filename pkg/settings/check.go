package settings

import "github.com/cubbit/dittofs/pkg/metadata/entity"

// Write is one change Check judges: a put of a setting or the delete of one
// scope's value, which leaves the wider row or the default effective.
type Write struct {
	Scope entity.SettingScope
	Key   string
}

// Put is the write of s.
func Put(s entity.Setting) Write {
	panic(unbuilt("Put"))
}

// Delete is the removal of key's value at scope.
func Delete(scope entity.SettingScope, key string) Write {
	panic(unbuilt("Delete"))
}

// HoldsContent reports whether scope holds content, read by Check's caller in
// the write's transaction. Check calls it only for a change to a bound
// setting, and refuses the write when it returns an error, so a bound setting
// never fails open.
type HoldsContent func(scope entity.SettingScope) (bool, error)

// Check judges w for caller inside the write's transaction tx, reading only
// through tx, and returns the refusal or nil.
//
// decision: a secret's rotation never reaches Check, since a setting's
// reference names the secret, not one of its versions: a new version changes
// no setting. Check judges only a reference repointed at another secret.
func (t Table) Check(
	tx entity.ControlPlaneTx,
	caller entity.APIRole,
	w Write,
	holds HoldsContent,
) error {
	return unbuilt("Check")
}

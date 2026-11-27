package state

import (
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// ChangeMask names what a change does to a file or directory: what
// CheckChange judges, and what a watch filters on.
type ChangeMask uint16

const (
	ChangeRemove    ChangeMask = 1 << iota // a name of the file goes
	ChangeRename                           // the file is renamed, or a rename replaces it
	ChangeAddEntry                         // a name is added to the directory
	ChangeAttrs                            // the file's attributes are set
	ChangeSetID                            // a mode change sets the setuid or setgid bit
	ChangeSize                             // the file's size changes
	ChangeTimes                            // the file's times are set; through an open, on that open's grant (CheckChange)
	ChangeSecurity                         // the file's owner, group or ACL changes
	ChangeStreams                          // a named stream of the file is added, removed or written
	ChangeEnumerate                        // a watch's changes were dropped: its client lists the directory again
	// ChangeReplace: a rename replaces the file. An SMB adapter passes it,
	// with ChangeRename, for the target of a rename asking ReplaceIfExists,
	// which the renamer holds no open of; an NFS one need not, as ChangeRename
	// already judges an NFS caller against every open.
	ChangeReplace
)

// Change is one change a watch reports.
type Change struct {
	What   ChangeMask // the kind of change, matched against a watch's filter; ChangeEnumerate: changes were dropped, and the client lists the directory again
	Name   []byte     // the entry changed, as its path from the directory Changed names
	To     []byte     // a rename's new path; nil otherwise
	Nested bool       // made below the directory Changed names, not in it: only a recursive watch reports it
	// Dirs is a nested change's path by identity, as it stood when the
	// change committed: each directory below the one Changed names, down to
	// the one the change was made in. Nil for a change in that directory.
	Dirs []entity.FileID
}

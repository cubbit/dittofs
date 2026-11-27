package entity

// Namespace is one block namespace's own record: the remote store it is
// created on, named by that store configuration's scope ID, and its fields
// bound at creation. A share names it through its Namespaces; the store
// names it back through an index written and deleted with this record, so
// whether a remote store is named is one range read, and so is whether a
// share names the namespace.
type Namespace struct {
	ID         NamespaceID
	Store      [16]byte // the remote store's scope ID; zero names none
	Bound      []byte   // the bound fields, encoded by the settings schema
	Generation uint64   // raised by every put
}

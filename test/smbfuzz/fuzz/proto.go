package fuzz

// Request is one system call for a worker, already resolved: paths are
// absolute, sizes evaluated, descriptors marshalled.
type Request struct {
	Step  int    `json:"step"`
	Side  string `json:"side"`
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Path2 string `json:"path2,omitempty"`
	Slot  string `json:"slot,omitempty"`  // "ref:alice.h0"
	Slot2 string `json:"slot2,omitempty"` // second slot, or the slot an open fills

	Flags  int    `json:"flags,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Off    int64  `json:"off,omitempty"`
	Off2   int64  `json:"off2,omitempty"`
	Len    int64  `json:"len,omitempty"`
	Data   string `json:"data,omitempty"`
	Seed   uint64 `json:"seed,omitempty"` // the round, for the rand pattern
	Whence int    `json:"whence,omitempty"`
	On     bool   `json:"on,omitempty"`

	ATime TimeArg `json:"atime,omitempty"`
	MTime TimeArg `json:"mtime,omitempty"`
	Attrs uint32  `json:"attrs,omitempty"`
	BTime uint64  `json:"btime,omitempty"` // FILETIME

	XName  string `json:"xname,omitempty"`
	XValue []byte `json:"xvalue,omitempty"`

	LockType int16  `json:"locktype,omitempty"`
	Part     string `json:"part,omitempty"`
	SD       []byte `json:"sd,omitempty"`
	Index    string `json:"index,omitempty"` // aceadd, acedel, aceedit: 0, 1, last, end
	AceMask  uint32 `json:"ace_mask,omitempty"`
	AceFlags byte   `json:"ace_flags,omitempty"`
	UID      int    `json:"uid,omitempty"`
	GID      int    `json:"gid,omitempty"`
}

// TimeArg is one utimensat time.
type TimeArg struct {
	Sec  int64 `json:"sec,omitempty"`
	Nsec int64 `json:"nsec,omitempty"`
	Omit bool  `json:"omit,omitempty"`
	Now  bool  `json:"now,omitempty"`
}

// Reply is a worker's answer.
type Reply struct {
	Errno int     `json:"errno"`
	Out   *Output `json:"out,omitempty"`
	Fatal string  `json:"fatal,omitempty"` // the worker could not run the request at all
}

// Output is what an operation observed. Which fields are set depends on the
// operation.
type Output struct {
	N      int64    `json:"n,omitempty"`
	Hash   string   `json:"hash,omitempty"`   // SHA-256 of bytes read
	Blocks []string `json:"blocks,omitempty"` // per-64KiB hashes, to locate a difference
	Stat   *StatOut `json:"stat,omitempty"`
	Dir    []DirEnt `json:"dir,omitempty"`
	Bytes  []byte   `json:"bytes,omitempty"` // xattr value, descriptor, DOS attributes
	Names  []string `json:"names,omitempty"`
	Lock   *LockOut `json:"lock,omitempty"`
}

// StatOut is statx's answer.
type StatOut struct {
	Type     string `json:"type"`
	Size     int64  `json:"size"`
	Nlink    uint32 `json:"nlink"`
	Ino      uint64 `json:"ino"`
	UID      uint32 `json:"uid"`
	GID      uint32 `json:"gid"`
	Perm     uint32 `json:"perm"`
	Mtime    int64  `json:"mtime"`
	Ctime    int64  `json:"ctime"`
	Btime    int64  `json:"btime"`
	HasBtime bool   `json:"has_btime"`
	Blocks   int64  `json:"blocks"`
}

// DirEnt is one listing entry.
type DirEnt struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// LockOut is F_OFD_GETLK's answer.
type LockOut struct {
	Type  int16 `json:"type"`
	Start int64 `json:"start"`
	Len   int64 `json:"len"`
}

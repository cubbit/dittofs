package vfs

import "github.com/cubbit/dittofs/pkg/vfs/caps"

// Caps is the capabilities an adapter states on every call (package caps);
// Call carries it and open state's Caller receives it whole.
type Caps = caps.Caps

// The capabilities and the adapters' profiles, as package caps declares them.
const (
	PrivilegeOnEveryCall = caps.PrivilegeOnEveryCall
	OpenFilesKeepNames   = caps.OpenFilesKeepNames
	WantOpenID           = caps.WantOpenID
	GrantBelongsToOpen   = caps.GrantBelongsToOpen
	ClientIsMachine      = caps.ClientIsMachine
	HostCall             = caps.HostCall
	SeesPseudoFS         = caps.SeesPseudoFS

	NFSv3Caps = caps.NFSv3Caps
	NFSv4Caps = caps.NFSv4Caps
	MountCaps = caps.MountCaps
	NLMCaps   = caps.NLMCaps
	SMBCaps   = caps.SMBCaps
)

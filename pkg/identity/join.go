package identity

// JoinCredential is a directory account's name and password, as a domain join
// and the domain controller's account calls take it: the administrator an
// online join creates the computer account as, or the bind its reads use.
// User is what the directory's simple bind accepts, a distinguished name or a
// UPN. Whoever holds one zeroes Password when done with it.
type JoinCredential struct {
	User     string
	Password []byte
}

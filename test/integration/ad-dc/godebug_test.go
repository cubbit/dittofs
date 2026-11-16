//go:build ad_dc

//go:debug x509negativeserial=1

// Samba AD-DC ships TLS certs with negative serial numbers, which Go rejects at
// parse time since 1.23. The online join's LDAPS connection to the test DC
// needs this godebug to complete the handshake.
package ad_dc_test

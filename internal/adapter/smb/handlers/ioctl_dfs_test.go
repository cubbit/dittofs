package handlers

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/smb/types"
)

// Both DFS referral requests carry the all-0xFF FileId and no open, and are
// answered as a server with no DFS answers them, never as a closed handle.
func TestIoctl_DfsReferralsAnswerNoDfs(t *testing.T) {
	var sentinel [16]byte
	copy(sentinel[:], allFFFileID)
	for name, code := range map[string]uint32{
		"FSCTL_DFS_GET_REFERRALS":    FsctlDfsGetReferrals,
		"FSCTL_DFS_GET_REFERRALS_EX": FsctlDfsGetReferralsEx,
	} {
		t.Run(name, func(t *testing.T) {
			h := NewHandler()
			ctx := &SMBHandlerContext{Context: context.Background(), ClientAddr: "127.0.0.1:9999"}
			// A REQ_GET_DFS_REFERRAL: MaxReferralLevel 4, then the path.
			input := append([]byte{4, 0}, []byte("\\\x00s\x00r\x00v\x00\x00\x00")...)
			result, err := h.Ioctl(ctx, buildIoctlRequestBody(code, sentinel, input, 4096))
			if err != nil {
				t.Fatalf("Ioctl: %v", err)
			}
			if result.Status != types.StatusFsDriverRequired {
				t.Errorf("status = %v, want STATUS_FS_DRIVER_REQUIRED", result.Status)
			}
		})
	}
}

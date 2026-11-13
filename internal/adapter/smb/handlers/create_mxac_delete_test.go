package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/smb/types"
	"github.com/cubbit/dittofs/pkg/metadata"
)

// The MxAc reply offers DELETE on a file with no stored ACL exactly when the
// open grants it. The parent decides, for the file's owner as for another
// user: owning a file does not make it removable.
func TestCreate_NoACL_MxAcDeleteMatchesTheOpen(t *testing.T) {
	h, ctxA, ctxB, adir := setupNoACLDeleteShare(t)
	metaSvc := h.Registry.GetMetadataService()
	rootUID, rootGID := uint32(0), uint32(0)
	rootAuth := &metadata.AuthContext{Context: context.Background(), Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID}}

	// Inside A's adir: root's "rodir" (0755), which A cannot write, and A's
	// "pub" (0777), which B can. Each holds a 0644 file of A's.
	for _, d := range []struct {
		name      string
		mode, uid uint32
	}{{"rodir", 0o755, 0}, {"pub", 0o777, noACLOwnerUID}} {
		dir, _, err := metaSvc.CreateDirectory(rootAuth, adir, d.name, &metadata.FileAttr{
			Type: metadata.FileTypeDirectory, Mode: d.mode, UID: d.uid, GID: d.uid,
		})
		if err != nil {
			t.Fatalf("CreateDirectory %s: %v", d.name, err)
		}
		dirHandle, err := metadata.EncodeFileHandle(dir)
		if err != nil {
			t.Fatalf("EncodeFileHandle %s: %v", d.name, err)
		}
		if _, _, err := metaSvc.CreateFile(rootAuth, dirHandle, "a.txt", &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644, UID: noACLOwnerUID, GID: noACLOwnerUID,
		}); err != nil {
			t.Fatalf("CreateFile %s/a.txt: %v", d.name, err)
		}
	}

	cases := []struct {
		name string
		ctx  *SMBHandlerContext
		path string
		del  bool
	}{
		{"owner, a parent it cannot write", ctxA, "adir\\rodir\\a.txt", false},
		{"owner, its own parent", ctxA, "adir\\a.txt", true},
		{"other user, a parent it cannot write", ctxB, "adir\\a.txt", false},
		{"other user, a 0777 parent", ctxB, "adir\\pub\\a.txt", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			access := mxacMaximalAccess(t, h, tc.ctx, tc.path)
			if got := access&uint32(types.Delete) != 0; got != tc.del {
				t.Errorf("MxAc = 0x%08x: DELETE %v, want %v", access, got, tc.del)
			}

			resp, err := h.Create(tc.ctx, &CreateRequest{
				FileName:          tc.path,
				DesiredAccess:     uint32(types.Delete),
				ShareAccess:       0x07,
				CreateDisposition: types.FileOpen,
				CreateOptions:     types.FileNonDirectoryFile,
			})
			if err != nil {
				t.Fatalf("CREATE(DELETE): %v", err)
			}
			if resp.Status == types.StatusSuccess {
				closeNoACL(t, h, tc.ctx, resp.FileID)
			}
			if opened := resp.Status == types.StatusSuccess; opened != tc.del {
				t.Errorf("CREATE(DELETE) = %v, while DELETE is %v in the reply this test expects", resp.Status, tc.del)
			}
		})
	}
}

// mxacMaximalAccess opens path for FILE_READ_ATTRIBUTES with an MxAc create
// context and answers the maximal access in the reply.
func mxacMaximalAccess(t *testing.T, h *Handler, ctx *SMBHandlerContext, path string) uint32 {
	t.Helper()
	resp, err := h.Create(ctx, &CreateRequest{
		FileName:          path,
		DesiredAccess:     uint32(types.FileReadAttributes),
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
		CreateOptions:     types.FileNonDirectoryFile,
		CreateContexts:    []CreateContext{{Name: "MxAc"}},
	})
	if err != nil || resp.Status != types.StatusSuccess {
		t.Fatalf("CREATE(FILE_READ_ATTRIBUTES, MxAc) of %s: status=%v err=%v", path, resp.Status, err)
	}
	closeNoACL(t, h, ctx, resp.FileID)
	for _, c := range resp.CreateContexts {
		if c.Name != "MxAc" {
			continue
		}
		if len(c.Data) != 8 {
			t.Fatalf("MxAc reply of %d bytes, want 8", len(c.Data))
		}
		if status := binary.LittleEndian.Uint32(c.Data[0:4]); status != 0 {
			t.Fatalf("MxAc QueryStatus = 0x%08x", status)
		}
		return binary.LittleEndian.Uint32(c.Data[4:8])
	}
	t.Fatalf("CREATE of %s answered no MxAc context", path)
	return 0
}

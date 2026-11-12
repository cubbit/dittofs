// A Linux client unlinks a file that is still open with one compound: a second
// open, a SET_INFO FileBasicInformation that sets FILE_ATTRIBUTE_HIDDEN, a
// rename to a hidden name, a delete disposition, and a CLOSE. The file is then
// removed when the handle that was holding it closes. These tests hold the two
// steps of it that depend on something other than the compound's own handle.
package handlers

import (
	"context"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/smb/types"
)

// A caller who does not own the file sets HIDDEN and READONLY on it through a
// handle that carries FILE_WRITE_ATTRIBUTES, the right MS-FSA 2.1.5.15.2
// names, and both land.
func TestSetInfo_BasicInfo_WriteAttributesAuthorizesNonOwnerDOSAttributes(t *testing.T) {
	h, ctx, rootHandle := setupStreamsDisabledShare(t, false)

	open, fileHandle := storeForeignOwnedOpen(t, h, ctx, rootHandle, "foreign-attrs.txt", 0x01,
		uint32(types.FileWriteAttributes)|uint32(types.Delete))

	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileBasicInformation),
		FileID:        open.FileID,
		Buffer:        encodeBasicInfo(0, 0, 0, 0, types.FileAttributeHidden|types.FileAttributeReadonly),
	})
	if err != nil {
		t.Fatalf("SetInfo: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("SET_INFO FileAttributes on a non-owned file with FILE_WRITE_ATTRIBUTES = %v, want STATUS_SUCCESS", resp.Status)
	}
	file, err := h.Registry.GetMetadataService().GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !file.Hidden || file.Mode&modeDOSReadonly == 0 {
		t.Errorf("after the write: hidden %v, mode %#x; want hidden and read-only: it was accepted but did not land", file.Hidden, file.Mode)
	}
}

// The other direction: the same write through a handle without
// FILE_WRITE_ATTRIBUTES is refused, and changes nothing.
func TestSetInfo_BasicInfo_DOSAttributesWithoutWriteAttributesDenied(t *testing.T) {
	h, ctx, rootHandle := setupStreamsDisabledShare(t, false)

	open, fileHandle := storeForeignOwnedOpen(t, h, ctx, rootHandle, "foreign-noattr.txt", 0x02,
		uint32(types.FileReadAttributes)|uint32(types.Delete))

	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileBasicInformation),
		FileID:        open.FileID,
		Buffer:        encodeBasicInfo(0, 0, 0, 0, types.FileAttributeHidden|types.FileAttributeReadonly),
	})
	if err != nil {
		t.Fatalf("SetInfo: %v", err)
	}
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("SET_INFO FileAttributes without FILE_WRITE_ATTRIBUTES = %v, want STATUS_ACCESS_DENIED", resp.Status)
	}
	file, err := h.Registry.GetMetadataService().GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if file.Hidden || file.Mode&modeDOSReadonly != 0 {
		t.Errorf("after the refused write: hidden %v, mode %#x; want neither set", file.Hidden, file.Mode)
	}
}

// A file renamed and marked for deletion through one handle is removed, under
// its new name, when the other handle that kept it open closes, and that CLOSE
// succeeds.
func TestClose_DeletesAFileRenamedThroughAnotherHandle(t *testing.T) {
	h, ctx, rootHandle := setupStreamsDisabledShare(t, false)

	holder := dirTestCreate(t, h, ctx, "held.txt", types.FileOpenIf, 0)
	second := dirTestCreate(t, h, ctx, "held.txt", types.FileOpen, 0)

	dirTestRename(t, h, ctx, second, ".__smb0001")
	resp, err := h.SetInfo(ctx, &SetInfoRequest{
		InfoType:      types.SMB2InfoTypeFile,
		FileInfoClass: uint8(types.FileDispositionInformation),
		FileID:        second,
		Buffer:        encodeDispositionInfo(true),
	})
	if err != nil || resp.Status != types.StatusSuccess {
		t.Fatalf("the delete disposition: %v, %v; want STATUS_SUCCESS", resp.Status, err)
	}
	if c, err := h.Close(ctx, &CloseRequest{FileID: second}); err != nil || c.Status != types.StatusSuccess {
		t.Fatalf("CLOSE of the renaming handle: %v, %v; want STATUS_SUCCESS", c.Status, err)
	}
	if names := dirTestChildNames(t, h, rootHandle); !slices.Contains(names, ".__smb0001") {
		t.Fatalf("entries with the holder still open: %v, want the file under its new name", names)
	}

	c, err := h.Close(ctx, &CloseRequest{FileID: holder})
	if err != nil || c.Status != types.StatusSuccess {
		t.Errorf("CLOSE of the handle that held the file: %v, %v; want STATUS_SUCCESS", c.Status, err)
	}
	if names := dirTestChildNames(t, h, rootHandle); len(names) != 0 {
		t.Errorf("entries after the last close: %v, want none", names)
	}
}

package platform

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// The query the volume's write-cache property is reached with. The control
// code is CTL_CODE of FILE_DEVICE_MASS_STORAGE, 0x2d — the IOCTL_STORAGE_BASE
// of winioctl.h — with function 0x0500, METHOD_BUFFERED and FILE_ANY_ACCESS:
// 0x2d<<16 | 0x500<<2.
const ioctlStorageQueryProperty uint32 = 0x002d1400

// storageDeviceWriteCacheProperty is the STORAGE_PROPERTY_ID that asks for a
// STORAGE_WRITE_CACHE_PROPERTY, the structure cachepolicy.go decodes
// (STORAGE_PROPERTY_ID, ntddstor.h: the enumeration opens with
// StorageDeviceProperty = 0 and counts through StorageAdapterProperty,
// StorageDeviceIdProperty and StorageDeviceUniqueIdProperty to this one).
const storageDeviceWriteCacheProperty uint32 = 4

// propertyStandardQuery is STORAGE_PROPERTY_QUERY's QueryType,
// PropertyStandardQuery, with which "the caller ... is querying" for the
// property's data (STORAGE_QUERY_TYPE, ntddstor.h).
const propertyStandardQuery uint32 = 0

// The answer buffers of the volume calls, in the sizes their doc's
// answer-space asks for. GetVolumePathName's buffer holds at least the 512
// UTF-16 characters its doc says to give it ("a safer but slower way to set
// the size of the return buffer is to call GetFullPathName ... and then make
// sure that the buffer size is at least the same size as the full path that
// GetFullPathName returns", GetVolumePathNameW, fileapi.h) — and, when the
// directory's own path is longer, that path with its NUL, so a long-path
// journal directory does not fail the check on a buffer smaller than the
// path it names; a call that cannot return its answer fails.
// GetVolumeNameForVolumeMountPoint's buffer holds the 50 UTF-16 characters
// its doc calls "a reasonable size for the buffer to accommodate the largest
// possible volume GUID path" (GetVolumeNameForVolumeMountPointW, fileapi.h).
const (
	volumePathLen  = 512
	guidBufLen     = 50
	writeCacheTail = 512
)

// writeCacheQuery returns the query's input, STORAGE_PROPERTY_QUERY
// (ntddstor.h): a PropertyId, a STORAGE_PROPERTY_ID held as Microsoft C holds
// an enumeration ("A variable declared as enum is an int.",
// https://learn.microsoft.com/cpp/c-language/enum-type), then a QueryType,
// its second 4 bytes, then AdditionalParameters, "an array of bytes" the
// property asks none of — one byte ("BYTE AdditionalParameters[1]", the
// Learn page of STORAGE_PROPERTY_QUERY), carried to the structure's 4-byte
// alignment: the whole is 12 bytes, and a query whose input is shorter is
// refused by the driver with STATUS_INFO_LENGTH_MISMATCH, DeviceIoControl's
// Win32 error 24, since the input "must be >= sizeof(STORAGE_PROPERTY_QUERY)"
// (IOCTL_STORAGE_QUERY_PROPERTY).
func writeCacheQuery() []byte {
	query := make([]byte, 12)
	binary.LittleEndian.PutUint32(query[0:], storageDeviceWriteCacheProperty)
	binary.LittleEndian.PutUint32(query[4:], propertyStandardQuery)
	return query
}

// writeCacheAnswer returns the query's answer as the verdict reads it: the
// buffer's first returned bytes, clipped to what the buffer holds — a query
// that reports more than the buffer it was given clips to the buffer, whose
// bytes are all the answer holds — capped so nothing past the reported count
// is the answer's. The answer buffer holds the structure plus room past it,
// since Size "may include data that follows this structure" and
// bytesReturned counts it, both as STORAGE_WRITE_CACHE_PROPERTY's Learn page
// gives (cachepolicy.go cites it).
func writeCacheAnswer(out []byte, returned uint32) []byte {
	n := min(returned, uint32(len(out)))
	return out[:n:n]
}

// probeVolume gathers the inputs of the verdict checkCacheProbe writes about
// the volume dir is on, through the table c, exactly as the calls reach
// Windows: the mount point of dir, the volume GUID path of that mount point,
// and the answer of the write-cache property query on the volume opened from
// it. A failed call is returned as an error naming the call and the path it
// was made on, never as a refusal: a volume the probe could not ask says
// nothing about its suitability. The only failure that is an answer is the
// property query's, whose Win32 error becomes the probe's queryErr for the
// verdict to decide on. The volume's handle is closed whatever the query
// answered, and a failed close joins the error the Linux probe joins it:
// the one that happened as itself, both when both did.
//
// decision: the flushing policy the query answers is a physical disk's — the
// disk object's DEV_POWER_PROTECTED flag, as checkCacheProbe's comment traces
// — while the query is made on the volume: a Storage Spaces or spanned volume
// is answered for the virtual disk, or answers ERROR_INVALID_FUNCTION and
// passes as not reported, so a member disk whose flushing is off can pass the
// check. Withdraw this by naming each member disk's device, or when Microsoft
// grows a per-volume property.
func probeVolume(c calls, dir string) (cacheProbe, error) {
	p := cacheProbe{dir: dir}

	// GetVolumePathName answers with the root of the volume the directory's
	// path ends on, following junctions and mounted folders
	// (GetVolumePathNameW, fileapi.h), in a buffer at least the largest path
	// its doc asks for — and the directory's own length when that is longer.
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return p, &fs.PathError{Op: "GetVolumePathName", Path: dir, Err: err}
	}

	mountPathBuf := make([]uint16, max(volumePathLen, len(windows.StringToUTF16(dir))+1))

	if err := c.getVolumePathName(dirPtr, &mountPathBuf[0], uint32(len(mountPathBuf))); err != nil {
		return p, &fs.PathError{Op: "GetVolumePathName", Path: dir, Err: err}
	}

	p.mountPoint = windows.UTF16ToString(mountPathBuf)

	// decision: a mount point that names no volume — the mapped or network
	// locations GetVolumeNameForVolumeMountPoint refuses, like the remote
	// path its doc's example fails with error 123 (fileapi.h) — fails the
	// check closed: the error is returned rather than excused, because a
	// volume the probe cannot name it cannot ask. Withdraw this if a real
	// local volume turns out to answer an error here.
	mountPtr, err := windows.UTF16PtrFromString(p.mountPoint)
	if err != nil {
		return p, &fs.PathError{Op: "GetVolumeNameForVolumeMountPoint", Path: p.mountPoint, Err: err}
	}

	var volumeNameBuf [guidBufLen]uint16

	if err := c.getVolumeNameForVolumeMountPoint(mountPtr, &volumeNameBuf[0], guidBufLen); err != nil {
		return p, &fs.PathError{Op: "GetVolumeNameForVolumeMountPoint", Path: p.mountPoint, Err: err}
	}

	p.volume = windows.UTF16ToString(volumeNameBuf[:])

	// The volume is opened on its device, not its root directory: "When
	// opening a volume ... use the following form: \\.\X:. Do not use a
	// trailing backslash (\), which indicates the root directory of a
	// drive" — its table opens the C: volume as \\.\C: and the file system
	// of the C: volume as \\.\C:\ (CreateFileA, fileapi.h), and the volume
	// GUID path follows the same rule (Naming a Volume). The probe's GUID
	// answer, "\\?\Volume{GUID}\", ends with exactly that backslash
	// (GetVolumeNameForVolumeMountPoint, fileapi.h), so exactly one is cut;
	// p.volume keeps the answer as it came, for the message the verdict
	// names its volume by.
	opened := strings.TrimSuffix(p.volume, `\`)

	// The open asks for no access: the property query asks the
	// device for its attributes, which CreateFile's dwDesiredAccess
	// documents being able to with zero ("allowing the application to query
	// device attributes without accessing a device", CreateFileW,
	// fileapi.h). The share and disposition are the package's own opens'
	// share and disposition, for the same reason its decision on
	// FILE_SHARE_DELETE gives: a reader on this handle makes no call the
	// volume refuses.
	volumePtr, err := windows.UTF16PtrFromString(opened)
	if err != nil {
		return p, &fs.PathError{Op: "CreateFile", Path: opened, Err: err}
	}

	// windows.CreateFile answers an error whenever the handle it returns is
	// invalid (golang.org/x/sys v0.49.0, zsyscall_windows.go: the error is
	// set from CreateFileW's outcome exactly when the handle equals
	// INVALID_HANDLE_VALUE), so the error alone decides; Windows' own table
	// writes no success at an invalid handle.
	h, err := c.createFile(
		volumePtr,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		return p, &fs.PathError{Op: "CreateFile", Path: opened, Err: err}
	}

	// The query's input is writeCacheQuery's 12 bytes, and only the bytes the
	// writeCacheAnswer clipping keeps of the buffer and the byte count the
	// call reported reach the verdict.
	query := writeCacheQuery()

	var (
		out       [writeCacheTail]byte
		returned  uint32
		gatherErr error
	)

	if err := c.deviceIoControl(
		h,
		ioctlStorageQueryProperty,
		&query[0],
		uint32(len(query)),
		&out[0],
		uint32(len(out)),
		&returned,
		nil,
	); err != nil {
		var errno syscall.Errno

		if errors.As(err, &errno) {
			// The Win32 error of the query is an answer: the verdict decides
			// whether the device does not report its write cache or the
			// check fails (checkCacheProbe).
			p.queryErr = uint32(errno)
		} else {
			// An error that carries no Win32 code is no answer the verdict
			// knows; it fails the check naming the call, as every other
			// failed one here does.
			gatherErr = &fs.PathError{Op: "DeviceIoControl", Path: opened, Err: err}
		}
	} else {
		p.answer = writeCacheAnswer(out[:], returned)
	}

	closeErr := c.closeHandle(h)
	if closeErr != nil {
		closeErr = &fs.PathError{Op: "CloseHandle", Path: opened, Err: closeErr}

		// the one that happened as itself, both through errors.Join when
		// both did, as the Linux probe joins them (joinClose).
		gatherErr = errors.Join(gatherErr, closeErr)
	}

	return p, gatherErr
}

// checkDirVolume decides whether the volume of dir may hold a journal: it
// gathers the verdict's inputs through the table c with probeVolume and
// runs checkCacheProbe on them, so the refusal of RFC 28 Appendix A — a
// volume whose write-cache buffer flushing is turned off by policy — is
// answered at Init and at open. A gather error is returned as itself, never
// as a refusal.
func checkDirVolume(c calls, dir string) error {
	p, err := probeVolume(c, dir)
	if err != nil {
		return err
	}

	return checkCacheProbe(p)
}

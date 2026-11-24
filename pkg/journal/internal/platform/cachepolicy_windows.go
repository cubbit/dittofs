package platform

import (
	"encoding/binary"
	"errors"
	"io/fs"
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

// The answer buffers of the two volume calls, in the sizes their doc's
// answer-space asks for. GetVolumePathName's buffer holds 512 UTF-16
// characters, room for the largest full path its doc says to give it
// ("a safer but slower way to set the size of the return buffer is to call
// GetFullPathName ... and then make sure that the buffer size is at least
// the same size as the full path that GetFullPathName returns",
// GetVolumePathNameW, fileapi.h); a call that cannot return its answer
// fails. GetVolumeNameForVolumeMountPoint's buffer holds the 50 UTF-16
// characters its doc calls "a reasonable size for the buffer to accommodate
// the largest possible volume GUID path" (GetVolumeNameForVolumeMountPointW,
// fileapi.h).
const (
	volumePathLen  = 512
	volumeNameLen  = 50
	writeCacheTail = 512
)

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
func probeVolume(c calls, dir string) (cacheProbe, error) {
	p := cacheProbe{dir: dir}

	// GetVolumePathName answers with the root of the volume the directory's
	// path ends on, following junctions and mounted folders
	// (GetVolumePathNameW, fileapi.h).
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return p, &fs.PathError{Op: "GetVolumePathName", Path: dir, Err: err}
	}

	var mountPathBuf [volumePathLen]uint16

	if err := c.getVolumePathName(dirPtr, &mountPathBuf[0], volumePathLen); err != nil {
		return p, &fs.PathError{Op: "GetVolumePathName", Path: dir, Err: err}
	}

	p.mountPoint = windows.UTF16ToString(mountPathBuf[:])

	// decision: the volume GUID query of a directory whose mount point
	// names no volume — a mapped or network location — fails the check
	// closed, as the remote-path example of GetVolumeNameForVolumeMountPoint
	// (fileapi.h) does for an unreachable share. A volume the probe cannot
	// name it cannot ask, so the error is returned rather than passed
	// around. Withdraw this if a real local volume turns out to answer an
	// error here.
	mountPtr, err := windows.UTF16PtrFromString(p.mountPoint)
	if err != nil {
		return p, &fs.PathError{Op: "GetVolumeNameForVolumeMountPoint", Path: p.mountPoint, Err: err}
	}

	var volumeNameBuf [volumeNameLen]uint16

	if err := c.getVolumeNameForVolumeMountPoint(mountPtr, &volumeNameBuf[0], volumeNameLen); err != nil {
		return p, &fs.PathError{Op: "GetVolumeNameForVolumeMountPoint", Path: p.mountPoint, Err: err}
	}

	p.volume = windows.UTF16ToString(volumeNameBuf[:])

	// The volume is opened with no access: the property query asks the
	// device for its attributes, which CreateFile's dwDesiredAccess
	// documents being able to with zero ("allowing the application to query
	// device attributes without accessing a device", CreateFileW,
	// fileapi.h). The share and disposition are the package's own opens'
	// share and disposition, for the same reason its decision on
	// FILE_SHARE_DELETE gives: a reader on this handle makes no call the
	// volume refuses.
	volumePtr, err := windows.UTF16PtrFromString(p.volume)
	if err != nil {
		return p, &fs.PathError{Op: "CreateFile", Path: p.volume, Err: err}
	}

	h, err := c.createFile(
		volumePtr,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil || h == windows.InvalidHandle {
		if err == nil {
			err = windows.ERROR_INVALID_HANDLE
		}

		return p, &fs.PathError{Op: "CreateFile", Path: p.volume, Err: err}
	}

	// The query's input is STORAGE_PROPERTY_QUERY (ntddstor.h): a PropertyId,
	// a STORAGE_PROPERTY_ID held as Microsoft C holds an enumeration ("A
	// variable declared as enum is an int.",
	// https://learn.microsoft.com/cpp/c-language/enum-type), then a
	// QueryType, its second 4 bytes; the additional parameters the property
	// asks none of. The answer buffer holds the structure plus room past
	// it, since Size "may include data that follows this structure" and
	// bytesReturned counts it, both as STORAGE_WRITE_CACHE_PROPERTY's Learn
	// page gives (cachepolicy.go cites it); only bytesReturned's bytes
	// reach the verdict.
	query := make([]byte, 8)
	binary.LittleEndian.PutUint32(query[0:], storageDeviceWriteCacheProperty)
	binary.LittleEndian.PutUint32(query[4:], propertyStandardQuery)

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
			gatherErr = &fs.PathError{Op: "DeviceIoControl", Path: p.volume, Err: err}
		}
	} else {
		// A query that reports more than the buffer it was given clips to
		// the buffer, whose bytes are all the answer holds.
		returned = min(returned, uint32(len(out)))
		p.answer = out[:returned:returned]
	}

	closeErr := c.closeHandle(h)
	if closeErr != nil {
		closeErr = &fs.PathError{Op: "CloseHandle", Path: p.volume, Err: closeErr}

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

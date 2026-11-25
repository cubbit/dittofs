package platform

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/cubbit/dittofs/internal/prof"

	"golang.org/x/sys/windows"
)

// volumeCall is one entry of a volumeRecorder's log.
type volumeCall struct {
	// op is getVolumePathName, getVolumeNameForVolumeMountPoint, createFile,
	// deviceIoControl or closeHandle.
	op string
	// handle is the handle createFile answered, and the handle the query and
	// the close act on; windows.InvalidHandle for a failed createFile.
	handle windows.Handle
	// path is the text the call names: the journal directory for
	// getVolumePathName, the mount point for the volume GUID query, the
	// volume for createFile and every call on its handle.
	path string
	// buffer is the answer buffer's length in UTF-16 characters, for the two
	// volume calls; 0 for the calls that take none.
	buffer uint32
	// access, share and createmode are createFile's arguments, attrs its
	// flags.
	access, share, createmode, attrs uint32
	// code is the query's IOCTL code, inLen and outLen its input and answer
	// buffer lengths, and query the input bytes it carried.
	code   uint32
	inLen  uint32
	outLen uint32
	query  []byte

	// returns says whether the query answered, and returned is the byte
	// count it reported then.
	returns  bool
	returned uint32
}

func (c volumeCall) String() string {
	switch c.op {
	case "createFile":
		return fmt.Sprintf(
			"createFile(%s, access %#x, share %#x, createmode %d, attrs %#x) = %#x",
			c.path, c.access, c.share, c.createmode, c.attrs, c.handle,
		)
	case "getVolumePathName", "getVolumeNameForVolumeMountPoint":
		return fmt.Sprintf("%s(%s, buffer of %d)", c.op, c.path, c.buffer)
	case "deviceIoControl":
		what := fmt.Sprintf(
			"deviceIoControl(%#x, code %#x, in %d bytes % x, out %d bytes)",
			c.handle, c.code, c.inLen, c.query, c.outLen,
		)
		if c.returns {
			return fmt.Sprintf("%s = %d bytes", what, c.returned)
		}
		return what
	default:
		return fmt.Sprintf("%s(%#x)", c.op, c.handle)
	}
}

// volumeRecorder is a table for the volume probe that logs every call and
// makes it against the real system: the mount point of the journal directory
// GetVolumePathName reaches, the volume GUID path the mount point names, the
// volume's own device handle CreateFile opens and the property query's answer
// on it — the machine's own volume, no recorded answer standing in the system's
// place. An error queued for an op is returned by its next call and the call
// does not run, except closeHandle, which runs, so the test leaks no handle.
type volumeRecorder struct {
	mu   sync.Mutex
	log  []volumeCall
	fail map[string][]error // per op, what its next calls return, oldest first
}

// newVolumeRecorder returns a recorder over the real volume calls, with no
// call queued to fail.
func newVolumeRecorder() *volumeRecorder {
	return &volumeRecorder{}
}

// failNext makes the next calls of op return errs, one each, in order.
func (r *volumeRecorder) failNext(op string, errs ...error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.fail == nil {
		r.fail = map[string][]error{}
	}

	r.fail[op] = append(r.fail[op], errs...)
}

// queued reports whether an error is queued for op's next call.
func (r *volumeRecorder) queued(op string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	q := r.fail[op]

	return len(q) > 0
}

// add logs c and returns the error queued for its op, if any.
func (r *volumeRecorder) add(c volumeCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.log = append(r.log, c)

	q := r.fail[c.op]
	if len(q) == 0 {
		return nil
	}

	r.fail[c.op] = q[1:]

	return q[0]
}

// callsOf returns the logged calls of op, in order.
func (r *volumeRecorder) callsOf(op string) []volumeCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	var got []volumeCall
	for _, c := range r.log {
		if c.op == op {
			got = append(got, c)
		}
	}

	return got
}

// setReturned notes the byte count the last deviceIoControl call reported.
func (r *volumeRecorder) setReturned(n uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := len(r.log) - 1; i >= 0; i-- {
		if r.log[i].op == "deviceIoControl" {
			r.log[i].returns = true
			r.log[i].returned = n

			return
		}
	}
}

// volumeDevice is the volume of the GUID path volume as CreateFile opens it:
// the path without its trailing backslash, which names "the root directory of
// a drive" — CreateFileA's own table opens the C: volume as \\.\C: and the
// file system of the C: volume as \\.\C:\ (fileapi.h).
func volumeDevice(volume string) string {
	return strings.TrimSuffix(volume, `\`)
}

func (r *volumeRecorder) table() calls {
	return calls{
		getVolumePathName: func(fileName *uint16, volumePathName *uint16, bufferLength uint32) error {
			if err := r.add(volumeCall{
				op:     "getVolumePathName",
				path:   windows.UTF16PtrToString(fileName),
				buffer: bufferLength,
			}); err != nil {
				return err
			}

			return windows.GetVolumePathName(fileName, volumePathName, bufferLength)
		},
		getVolumeNameForVolumeMountPoint: func(volumeMountPoint *uint16, volumeName *uint16, bufferlength uint32) error {
			if err := r.add(volumeCall{
				op:     "getVolumeNameForVolumeMountPoint",
				path:   windows.UTF16PtrToString(volumeMountPoint),
				buffer: bufferlength,
			}); err != nil {
				return err
			}

			return windows.GetVolumeNameForVolumeMountPoint(volumeMountPoint, volumeName, bufferlength)
		},
		createFile: func(
			name *uint16,
			access uint32,
			mode uint32,
			sa *windows.SecurityAttributes,
			createmode uint32,
			attrs uint32,
			templatefile windows.Handle,
		) (windows.Handle, error) {
			c := volumeCall{
				op:         "createFile",
				handle:     windows.InvalidHandle,
				path:       windows.UTF16PtrToString(name),
				access:     access,
				share:      mode,
				createmode: createmode,
				attrs:      attrs,
			}

			if r.queued("createFile") {
				return windows.InvalidHandle, r.add(c)
			}

			h, err := windows.CreateFile(name, access, mode, sa, createmode, attrs, templatefile)
			if err == nil {
				c.handle = h
			}

			_ = r.add(c)

			return h, err
		},
		deviceIoControl: func(
			handle windows.Handle,
			ioControlCode uint32,
			inBuffer *byte,
			inBufferSize uint32,
			outBuffer *byte,
			outBufferSize uint32,
			bytesReturned *uint32,
			overlapped *windows.Overlapped,
		) error {
			c := volumeCall{
				op:     "deviceIoControl",
				handle: handle,
				code:   ioControlCode,
				inLen:  inBufferSize,
				outLen: outBufferSize,
			}
			if inBuffer != nil {
				c.query = append([]byte(nil), unsafe.Slice(inBuffer, inBufferSize)...)
			}
			if err := r.add(c); err != nil {
				return err
			}

			err := windows.DeviceIoControl(
				handle, ioControlCode, inBuffer, inBufferSize,
				outBuffer, outBufferSize, bytesReturned, overlapped,
			)
			if err == nil {
				r.setReturned(*bytesReturned)
			}

			return err
		},
		closeHandle: func(handle windows.Handle) error {
			queued := r.add(volumeCall{op: "closeHandle", handle: handle})
			err := windows.CloseHandle(handle)

			if queued != nil {
				return queued
			}

			return err
		},
	}
}

// wantCallFailure fails t unless err is the failure of the probe's call named
// op on path, caused by cause, and no refusal: a volume the probe could not
// ask says nothing about its suitability.
func wantCallFailure(t *testing.T, err error, op, path string, cause error) {
	t.Helper()

	var r *refusal
	if errors.As(err, &r) {
		t.Fatalf("got refusal %v, want a %s failure on %s", err, op, path)
	}

	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want a %s failure on %s", err, op, path)
	}

	if pe.Op != op || pe.Path != path {
		t.Fatalf("got %s on %s (%v), want %s on %s", pe.Op, pe.Path, err, op, path)
	}

	if !errors.Is(err, cause) {
		t.Fatalf("got %v, want it caused by %v", err, cause)
	}
}

// TestCheckWriteCachePolicyVolume runs the real gather through a table that
// passes each call to the system, so "Open MUST refuse a volume whose
// write-cache buffer flushing is turned off by policy, where the platform
// reports it", RFC 28 Appendix A's Windows profile, is exercised through the
// machine's own volume: no recorded answer stands in Windows' place. Each
// row and each failure is queued as one call's fault, so every step of the
// gather runs against the volume until the one the row fails.
//
// The refusal the "policy on" answer brings, the passing answer and the cut
// answers are pinned without the gather by the verdict's rows on
// checkCacheProbe, and the gather's own byte clipping by the direct rows of
// TestWriteCacheAnswer on the extracted function below, so no row here needs
// a scripted volume answer: the query's two "not reported" faults
// (ERROR_NOT_SUPPORTED and ERROR_INVALID_FUNCTION) run over a genuinely
// opened volume and hand the verdict a genuine Win32 code, and every other
// row fails one call of the chain and never reaches a recorded answer.
func TestCheckWriteCachePolicyVolume(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()

	// The paths the later calls are wanted on are the ones the probe itself
	// reads off the machine's volume, gathered once with no fault.
	base, err := probeVolume(realTable, dir)
	if err != nil {
		t.Fatalf("probeVolume of %s: got %v, want the volume's own answers", dir, err)
	}
	device := volumeDevice(base.volume)

	notReported := []struct {
		name  string
		cause error
	}{
		{
			// ERROR_NOT_SUPPORTED, 50: what a RAID or flash device answers
			// when it does not report its write cache (cachepolicy.go names
			// what Microsoft documents of it). Fails if the gather's error
			// stops the check where the verdict lets this query pass.
			name:  "a query not supported passes",
			cause: windows.ERROR_NOT_SUPPORTED,
		},
		{
			// ERROR_INVALID_FUNCTION, 1: what a device that does not handle
			// the request answers. Fails the same way.
			name:  "a query with an invalid function passes",
			cause: windows.ERROR_INVALID_FUNCTION,
		},
	}
	for _, tc := range notReported {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			r := newVolumeRecorder()
			r.failNext("deviceIoControl", tc.cause)

			wantPass(t, checkDirVolume(r.table(), dir))
		})
	}

	t.Run("a query that fails another way is an error", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		// ERROR_IO_DEVICE, 1117: the class driver sample's failure when the
		// disk cannot read its caching mode page
		// (ClasspWriteCacheProperty, cited in checkCacheProbe). Fails if the
		// gather reads any failed query as the verdict's passing errors,
		// or reports the volume refused rather than unread.
		r := newVolumeRecorder()
		r.failNext("deviceIoControl", windows.ERROR_IO_DEVICE)

		wantError(t, checkDirVolume(r.table(), dir), errCacheQueryFailed)
	})

	// One row per gather step failing: each is the failure of the call named
	// on the path it was made, never a silent pass and never a refusal — a
	// volume the probe could not ask says nothing about its suitability.
	failed := []struct {
		name     string
		op       string
		cause    error
		wantOp   string
		wantPath string
	}{
		{
			// The path names no volume the directory is on. Fails if the
			// gather reads the error as an accepted directory.
			name:     "getVolumePathName fails",
			op:       "getVolumePathName",
			cause:    windows.ERROR_PATH_NOT_FOUND,
			wantOp:   "GetVolumePathName",
			wantPath: dir,
		},
		{
			// The mount point names no volume GUID path — the mapped or
			// network location probeVolume's decision marker names.
			// ERROR_INVALID_NAME is the failure its Learn page's remote-path
			// example gives, error 123. Fails if the gather hides the
			// failure or reports it as a refusal.
			name:     "the volume GUID query fails",
			op:       "getVolumeNameForVolumeMountPoint",
			cause:    windows.ERROR_INVALID_NAME,
			wantOp:   "GetVolumeNameForVolumeMountPoint",
			wantPath: base.mountPoint,
		},
		{
			// The volume's device refuses its handle. Fails if the gather
			// reads an unopenable volume as an accepted directory, hands a
			// failed createFile's InvalidHandle to the query, or opens the
			// GUID path with its trailing backslash — the root directory,
			// not the volume.
			name:     "createFile refuses the handle",
			op:       "createFile",
			cause:    windows.ERROR_ACCESS_DENIED,
			wantOp:   "CreateFile",
			wantPath: device,
		},
	}
	for _, tc := range failed {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			r := newVolumeRecorder()
			r.failNext(tc.op, tc.cause)

			wantCallFailure(t, checkDirVolume(r.table(), dir), tc.wantOp, tc.wantPath, tc.cause)
		})
	}

	t.Run("closeHandle fails after a good query", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		// The query ran against the machine's volume and the recorder closed
		// its handle; only the close's answer is the queued fault. The
		// gather returns the close failure as the Linux probe joins it, and
		// the verdict never runs: the check fails on the call, never as a
		// silent pass. Fails if the gather swallows a close error, or
		// reaches the verdict anyway on a handle it leaked.
		r := newVolumeRecorder()
		r.failNext("closeHandle", windows.ERROR_INVALID_HANDLE)

		wantCallFailure(t, checkDirVolume(r.table(), dir), "CloseHandle", device, windows.ERROR_INVALID_HANDLE)
	})
}

// TestCheckWriteCachePolicyVolumeCalls fails a probe that asks a call the
// wrong question, takes the wrong buffer sizes or fails to close the volume:
// the mount point is asked of the directory, the GUID path of the mount point,
// the volume with the share and disposition the package's own opens use and no
// access, the property query with the property and the standard query type in
// its 12-byte input; and the answer handed to the verdict is exactly the bytes
// the call reported, clipped to the buffer. The calls run against the machine's
// own volume and every assertion below reads the recorder's log or the probe's
// own answers; the machine's policy decides nothing here.
func TestCheckWriteCachePolicyVolumeCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := newVolumeRecorder()

	p, err := probeVolume(r.table(), dir)
	if err != nil {
		t.Fatalf("probeVolume of %s: got %v, want the volume's own answers gathered", dir, err)
	}

	// The gather fills every field from the calls' answers: none is left as it
	// came.
	if p.dir != dir || p.mountPoint == "" || p.volume == "" {
		t.Fatalf("got dir %q, mount point %q, volume %q, want %q and the answers of the machine's volume", p.dir, p.mountPoint, p.volume, dir)
	}

	// GetVolumePathName answers with the root of the volume the directory is
	// on; its buffer is the larger of the largest path the doc's "safer but
	// slower" form asks for (GetVolumePathNameW, fileapi.h) and the
	// directory's own path with its NUL, so a long-path directory does not
	// fail the call its mount point is asked in.
	got := r.callsOf("getVolumePathName")
	if len(got) != 1 || got[0].path != dir {
		t.Fatalf("got %v, want one getVolumePathName on %s", got, dir)
	}
	if want := uint32(max(volumePathLen, len(windows.StringToUTF16(dir))+1)); got[0].buffer != want {
		t.Errorf("%v: buffer of %d, want exactly %d UTF-16 characters", got[0], got[0].buffer, want)
	}

	// GetVolumeNameForVolumeMountPoint answers the volume GUID path of the
	// mount point, in the buffer of "50 characters" its doc calls reasonable
	// for "the largest possible volume GUID path"
	// (GetVolumeNameForVolumeMountPointW, fileapi.h). Fails if the mount
	// point stated first is not the one the path call answered.
	got = r.callsOf("getVolumeNameForVolumeMountPoint")
	if len(got) != 1 || got[0].path != p.mountPoint {
		t.Fatalf("got %v, want one getVolumeNameForVolumeMountPoint on %s", got, p.mountPoint)
	}
	if want := uint32(guidBufLen); got[0].buffer != want {
		t.Errorf("%v: buffer of %d, want exactly %d UTF-16 characters", got[0], got[0].buffer, want)
	}

	// The volume is opened on its device — the GUID path without the
	// trailing backslash that names its root directory, the form
	// CreateFileA's own table rules (fileapi.h, and see volumeDevice above) —
	// with access 0, which CreateFile's dwDesiredAccess documents for "query
	// device attributes without accessing a device" (CreateFileW, fileapi.h),
	// and the share and disposition the package's own opens set
	// (os_windows.go's decision on FILE_SHARE_DELETE). Fails if any of them
	// differs — this is the pin of the volume device against the root
	// directory's path — or the query and the close do not act on its
	// handle.
	opens := r.callsOf("createFile")
	if len(opens) != 1 || opens[0].path != volumeDevice(p.volume) {
		t.Fatalf("got %v, want one createFile on %s", opens, volumeDevice(p.volume))
	}
	open := opens[0]
	if want := uint32(0); open.access != want {
		t.Errorf("%v: access %#x, want %#x", open, open.access, want)
	}
	if want := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE); open.share != want {
		t.Errorf("%v: share %#x, want %#x", open, open.share, want)
	}
	if open.createmode != windows.OPEN_EXISTING {
		t.Errorf("%v: createmode %d, want OPEN_EXISTING (%d)", open, open.createmode, windows.OPEN_EXISTING)
	}
	if open.attrs != 0 {
		t.Errorf("%v: attrs %#x, want 0", open, open.attrs)
	}

	queries := r.callsOf("deviceIoControl")
	if len(queries) != 1 || queries[0].handle != open.handle {
		t.Fatalf("got %v, want one deviceIoControl on the opened handle %#x", queries, open.handle)
	}
	query := queries[0]
	if query.code != ioctlStorageQueryProperty {
		t.Errorf("%v: code %#x, want IOCTL_STORAGE_QUERY_PROPERTY %#x", query, query.code, ioctlStorageQueryProperty)
	}

	// The input is STORAGE_PROPERTY_QUERY, pinned by TestWriteCacheQuery from
	// the same bytes; here it is what the call carried: the property and the
	// query type as two 4-byte enumerations, and 12 bytes the whole. An input
	// shorter than the two enumerations fails on its own assertion below
	// rather than a panic.
	if query.inLen != 12 {
		t.Errorf("%v: input of %d bytes, want the 12 of STORAGE_PROPERTY_QUERY", query, query.inLen)
	}
	if len(query.query) < 8 {
		t.Errorf("%v: input carried %d bytes, want at least the 8 of PropertyId and QueryType", query, len(query.query))
	} else {
		if got := binary.LittleEndian.Uint32(query.query[0:]); got != storageDeviceWriteCacheProperty {
			t.Errorf("%v: PropertyId %d, want StorageDeviceWriteCacheProperty (%d)", query, got, storageDeviceWriteCacheProperty)
		}
		if got := binary.LittleEndian.Uint32(query.query[4:]); got != propertyStandardQuery {
			t.Errorf("%v: QueryType %d, want PropertyStandardQuery (%d)", query, got, propertyStandardQuery)
		}
		for i, b := range query.query[8:] {
			if b != 0 {
				t.Errorf("%v: AdditionalParameters byte %d is %#x, want 0", query, 8+i, b)
			}
		}
	}

	// The answer buffer holds the structure and room past it, since Size
	// "may include data that follows this structure" and bytesReturned counts
	// it. Fails if a Size past the structure is clipped short of its bytes.
	if want := uint32(writeCacheTail); query.outLen != want {
		t.Errorf("%v: answer buffer of %d bytes, want exactly %d so data the Size counts is not clipped", query, query.outLen, want)
	}

	// The verdict reads exactly the bytes the call reported, clipped to the
	// buffer — writeCacheAnswer's twin assertion, over what reached the
	// probe. When the query failed, no byte reached it.
	if p.queryErr == 0 {
		if want := int(min(query.returned, query.outLen)); len(p.answer) != want {
			t.Errorf("%v: verdict read %d bytes, want the %d the call reported", query, len(p.answer), want)
		}
	} else if len(p.answer) != 0 {
		t.Errorf("a failed query (Win32 error %d) handed the verdict %d bytes, want none", p.queryErr, len(p.answer))
	}

	// The handle is closed exactly once, by the same one the open answered.
	closes := r.callsOf("closeHandle")
	if len(closes) != 1 || closes[0].handle != open.handle {
		t.Errorf("got %v, want one closeHandle on the opened handle %#x", closes, open.handle)
	}
}

// TestWriteCacheQuery pins the bytes the gather sends the property query, the
// pure input of the gather's one call (TestWriteCacheAnswer below pins its
// output): the whole STORAGE_PROPERTY_QUERY, so a shorter input — which the
// driver refuses with STATUS_INFO_LENGTH_MISMATCH — goes red, as does a wrong
// property, a wrong query type or a non-empty AdditionalParameters.
func TestWriteCacheQuery(t *testing.T) {
	prof.Watch(t, prof.Options{})

	query := writeCacheQuery()

	if len(query) != 12 {
		t.Fatalf("got a query of %d bytes, want the 12 of STORAGE_PROPERTY_QUERY", len(query))
	}
	if got := binary.LittleEndian.Uint32(query[0:]); got != storageDeviceWriteCacheProperty {
		t.Errorf("got PropertyId %d, want StorageDeviceWriteCacheProperty (%d)", got, storageDeviceWriteCacheProperty)
	}
	if got := binary.LittleEndian.Uint32(query[4:]); got != propertyStandardQuery {
		t.Errorf("got QueryType %d, want PropertyStandardQuery (%d)", got, propertyStandardQuery)
	}
	for i, b := range query[8:] {
		if b != 0 {
			t.Errorf("got AdditionalParameters byte %d = %#x, want 0", 8+i, b)
		}
	}
}

// TestWriteCacheAnswer pins the clipping of the query's answer, the pure
// output of the gather's one call: the verdict reads exactly the bytes the
// call said it returned, never more than the buffer holds, and nothing past
// the reported count is the answer's to append to. The buffer starts as the
// flushing-on answer with every byte past the structure made garbage, so a
// clip that keeps them goes red.
func TestWriteCacheAnswer(t *testing.T) {
	prof.Watch(t, prof.Options{})

	out := make([]byte, writeCacheTail)
	for i := range out {
		out[i] = 0xAA
	}
	copy(out, flushingOn())

	for _, tc := range []struct {
		name     string
		returned uint32
		wantLen  int
	}{
		{"the bytes the call counted", writeCacheFirstWhole, writeCacheFirstWhole},
		{"the buffer's whole, when the call counted past it", writeCacheTail + 8, writeCacheTail},
		{"nothing, when the call counted none", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := writeCacheAnswer(out, tc.returned)

			if len(got) != tc.wantLen {
				t.Fatalf("got %d bytes, want %d: the answer is the reported count clipped to the buffer", len(got), tc.wantLen)
			}
			if cap(got) != tc.wantLen {
				t.Errorf("got capacity %d, want %d: nothing past the reported count is the answer's to own", cap(got), tc.wantLen)
			}
			if want := flushingOn()[:min(tc.wantLen, len(flushingOn()))]; !bytes.HasPrefix(got, want) {
				t.Errorf("got % x, want the buffer's first bytes % x", got, want)
			}
		})
	}
}

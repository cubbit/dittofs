package platform

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"unicode/utf16"
	"unsafe"

	"github.com/cubbit/dittofs/internal/prof"

	"golang.org/x/sys/windows"
)

// volumeCall is one entry of a fakeVolume's log.
type volumeCall struct {
	// op is getVolumePathName, getVolumeNameForVolumeMountPoint, createFile,
	// deviceIoControl or closeHandle.
	op string
	// path is the text the call names: the journal directory for
	// getVolumePathName, the mount point for the volume GUID query, the
	// volume for createFile and every call on its handle.
	path string
	// buffer is the answer buffer's length in UTF-16 characters, for the two
	// volume calls; 0 for the calls that take none.
	buffer uint32
	// handle is the handle createFile answered, and the handle the query and
	// the close act on.
	handle windows.Handle
	// access, share, createmode and attrs are createFile's arguments.
	access, share, createmode, attrs uint32
	// code is the query's IOCTL code, inLen and outLen its input and answer
	// buffer lengths, and query the input bytes it carried.
	code          uint32
	inLen, outLen uint32
	query         []byte
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
		return fmt.Sprintf(
			"deviceIoControl(%#x, code %#x, in %d bytes % x, out %d bytes)",
			c.handle, c.code, c.inLen, c.query, c.outLen,
		)
	default:
		return fmt.Sprintf("%s(%#x)", c.op, c.handle)
	}
}

// fakeVolume is a recorded table for the volume probe. Every entry logs its
// call with the arguments the probe gave it, and answers what the test
// recorded, so no case depends on the host's volumes: getVolumePathName
// answers the mount point, getVolumeNameForVolumeMountPoint the volume GUID
// path of that mount point, createFile a handle, deviceIoControl the recorded
// answer with its recorded byte count or its recorded error, and closeHandle
// nil. An error queued for an op is returned by its next call in place of
// its answer. The answers describe the write cache of a disk whose flushing
// is on, so a case changes only the entry it exercises.
type fakeVolume struct {
	mu sync.Mutex
	// log holds a call per entry, in the order the probe made them.
	log []volumeCall
	// fail holds, per op, the errors its next calls return in place of their
	// answers, oldest first.
	fail map[string][]error

	// mountPoint, volume and handle are the recorded answers of
	// getVolumePathName, getVolumeNameForVolumeMountPoint and createFile.
	mountPoint string
	volume     string
	handle     windows.Handle
	// queryAnswer is the bytes deviceIoControl writes into the answer
	// buffer, and queryReturned the byte count it reports; 0 reports
	// len(queryAnswer). queryErr is the error it returns in place of an
	// answer, as the device answers a query it does not complete.
	queryAnswer   []byte
	queryReturned uint32
	queryErr      error
}

// newFakeVolume returns a fake whose answers describe testDir on a disk
// whose flushing is on, volume and mount point the constants the verdict's
// own tests use.
func newFakeVolume() *fakeVolume {
	return &fakeVolume{
		mountPoint:  testMountPoint,
		volume:      testVolume,
		handle:      windows.Handle(0x42424242),
		queryAnswer: flushingOn(),
	}
}

func (f *fakeVolume) table() calls {
	return calls{
		getVolumePathName:                f.getVolumePathName,
		getVolumeNameForVolumeMountPoint: f.getVolumeNameForVolumeMountPoint,
		createFile:                       f.createFile,
		deviceIoControl:                  f.deviceIoControl,
		closeHandle:                      f.closeHandle,
	}
}

// failNext makes the next calls of op return errs in place of their
// answers, one each, in order.
func (f *fakeVolume) failNext(op string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail == nil {
		f.fail = map[string][]error{}
	}

	f.fail[op] = append(f.fail[op], errs...)
}

// take returns, and forgets, the next error queued for op.
func (f *fakeVolume) take(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	errs := f.fail[op]
	if len(errs) == 0 {
		return nil
	}

	f.fail[op] = errs[1:]

	return errs[0]
}

func (f *fakeVolume) add(c volumeCall) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.log = append(f.log, c)
}

// callsOf returns the logged calls of op, in order.
func (f *fakeVolume) callsOf(op string) []volumeCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	var got []volumeCall
	for _, c := range f.log {
		if c.op == op {
			got = append(got, c)
		}
	}

	return got
}

// writeAnswer writes the UTF-16 encoding of answer, with its NUL, into the
// buffer of length the call was given.
func writeAnswer(buffer *uint16, length uint32, answer string) {
	buf := unsafe.Slice(buffer, length)
	units := utf16.Encode([]rune(answer))

	buf[copy(buf, units)] = 0
}

func (f *fakeVolume) getVolumePathName(fileName *uint16, volumePathName *uint16, bufferLength uint32) error {
	c := volumeCall{
		op:     "getVolumePathName",
		path:   windows.UTF16PtrToString(fileName),
		buffer: bufferLength,
	}
	f.add(c)

	if err := f.take(c.op); err != nil {
		return err
	}

	writeAnswer(volumePathName, bufferLength, f.mountPoint)

	return nil
}

func (f *fakeVolume) getVolumeNameForVolumeMountPoint(volumeMountPoint *uint16, volumeName *uint16, bufferlength uint32) error {
	c := volumeCall{
		op:     "getVolumeNameForVolumeMountPoint",
		path:   windows.UTF16PtrToString(volumeMountPoint),
		buffer: bufferlength,
	}
	f.add(c)

	if err := f.take(c.op); err != nil {
		return err
	}

	writeAnswer(volumeName, bufferlength, f.volume)

	return nil
}

func (f *fakeVolume) createFile(
	name *uint16,
	access uint32,
	mode uint32,
	sa *windows.SecurityAttributes,
	createmode uint32,
	attrs uint32,
	templatefile windows.Handle,
) (windows.Handle, error) {
	f.mu.Lock()
	handle := f.handle
	f.mu.Unlock()

	f.add(volumeCall{
		op:         "createFile",
		path:       windows.UTF16PtrToString(name),
		handle:     handle,
		access:     access,
		share:      mode,
		createmode: createmode,
		attrs:      attrs,
	})

	if err := f.take("createFile"); err != nil {
		f.mu.Lock()
		f.log[len(f.log)-1].handle = windows.InvalidHandle
		f.mu.Unlock()

		return windows.InvalidHandle, err
	}

	return handle, nil
}

func (f *fakeVolume) deviceIoControl(
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
	f.add(c)

	if err := f.take(c.op); err != nil {
		return err
	}

	if f.queryErr != nil {
		return f.queryErr
	}

	// The bytes past the answer hold what an earlier query's answer left in
	// the buffer; the probe hands the verdict answer[:bytesReturned], so
	// those byte counts name exactly what the verdict reads.
	out := unsafe.Slice(outBuffer, outBufferSize)
	for i := range out {
		out[i] = 0xAA
	}
	copy(out, f.queryAnswer)

	reported := f.queryReturned
	if reported == 0 {
		reported = uint32(len(f.queryAnswer))
	}
	*bytesReturned = reported

	return nil
}

func (f *fakeVolume) closeHandle(handle windows.Handle) error {
	f.add(volumeCall{op: "closeHandle", handle: handle})

	return f.take("closeHandle")
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

// TestCheckWriteCachePolicyVolume runs the verdict on the real gather through
// a recorded table, so what the platform reads decides: "Open MUST refuse a
// volume whose write-cache buffer flushing is turned off by policy, where the
// platform reports it", through the exact calls of RFC 28 Appendix A's
// Windows profile. Each case changes the entry it exercises, the recorded
// answers doing all the rest; the constants are the verdict's own tests'. The
// refusal itself, the passed cases and the failed query decoded here are the
// verdict's; this test pins the gather that reaches them.
func TestCheckWriteCachePolicyVolume(t *testing.T) {
	t.Run("flushing off by policy refuses", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		// The recorded deviceIoControl answers flushingOff. Fails if the
		// gather is not reached at all — the answer nil, its bytes changed —
		// or the refusal does not carry the call's answers: the directory,
		// the mount point and the volume the verdict names in its message.
		f := newFakeVolume()
		f.queryAnswer = flushingOff()

		wantRefusal(t, checkDirVolume(f.table(), testDir), ruleCachePolicy, testDir, testMountPoint, "", testVolume)
	})

	t.Run("flushing on passes", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		// The recorded answer of the same volume with the policy clear, the
		// passing twin of the row above: only its byte 25 changes. Fails if
		// the gather reports the policy on an answer whose policy is off.
		f := newFakeVolume()

		wantPass(t, checkDirVolume(f.table(), testDir))
	})

	notReported := []struct {
		name     string
		queryErr error
	}{
		{
			// ERROR_NOT_SUPPORTED, 50: what a RAID or flash device answers
			// when it does not report its write cache (cachepolicy.go names
			// what Microsoft documents of it). Fails if the gather's error
			// stops the check where the verdict lets this query pass.
			name:     "a query not supported passes",
			queryErr: windows.ERROR_NOT_SUPPORTED,
		},
		{
			// ERROR_INVALID_FUNCTION, 1: what a device that does not handle
			// the request answers. Fails the same way.
			name:     "a query with an invalid function passes",
			queryErr: windows.ERROR_INVALID_FUNCTION,
		},
	}
	for _, tc := range notReported {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			f := newFakeVolume()
			f.queryErr = tc.queryErr

			wantPass(t, checkDirVolume(f.table(), testDir))
		})
	}

	t.Run("a query that fails another way is an error", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		// ERROR_IO_DEVICE, 1117: the class driver sample's failure when the
		// disk cannot read its caching mode page
		// (ClasspWriteCacheProperty, cited in checkCacheProbe). Fails if
		// the gather reads any failed query as the verdict's passing errors,
		// or reports the volume refused rather than unread.
		f := newFakeVolume()
		f.queryErr = windows.ERROR_IO_DEVICE

		wantError(t, checkDirVolume(f.table(), testDir), errCacheQueryFailed)
	})

	// The answer's byte count, bytesReturned, names exactly the bytes the
	// verdict reads; the gather hands it answer[:bytesReturned] and nothing
	// else.
	t.Run("an answer longer than its structure refuses", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		// The recorded answer holds Size 32 with 32 bytes reported: "Size
		// may include data that follows this structure" (the verdict's own
		// twin row, through the gather). Fails if the gather hands the
		// verdict fewer bytes than the answer's Size counts.
		f := newFakeVolume()
		f.queryAnswer = append(with(flushingOff(), 4, 0x20), 0x00, 0x00, 0x00, 0x00)

		wantRefusal(t, checkDirVolume(f.table(), testDir), ruleCachePolicy, testDir, testMountPoint, "", testVolume)
	})

	t.Run("bytes returned past the bytes filled refuse", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		// The recorded query fills 28 bytes whose Size says 32 and reports
		// 32 returned; the four past them are the 0xAA the buffer held. The
		// verdict reads all 32 and refuses. Fails a gather that hands just
		// the bytes filled, which would cut the answer Size promises short.
		f := newFakeVolume()
		f.queryAnswer = with(flushingOff(), 4, 0x20)
		f.queryReturned = 32

		wantRefusal(t, checkDirVolume(f.table(), testDir), ruleCachePolicy, testDir, testMountPoint, "", testVolume)
	})

	t.Run("a header alone is an error", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		// The class driver sample answers with the 8-byte header alone when
		// the caller's buffer is too small for the structure; the fake
		// reports 8 returned and fills 8. Fails a gather that hands the
		// verdict the whole answer buffer, whose stale bytes read as an
		// answer here, or the bytes filled past what the count returned.
		f := newFakeVolume()
		f.queryAnswer = flushingOn()[:8]

		wantError(t, checkDirVolume(f.table(), testDir), errCacheAnswerShort)
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
			wantPath: testDir,
		},
		{
			// The mount point names no volume GUID path — the mapped or
			// network location probeVolume's decision: marker names.
			// ERROR_INVALID_NAME is the failure its Learn page's remote-path
			// example gives, error 123. Fails if the gather hides the
			// failure or reports it as a refusal.
			name:     "the volume GUID query fails",
			op:       "getVolumeNameForVolumeMountPoint",
			cause:    windows.ERROR_INVALID_NAME,
			wantOp:   "GetVolumeNameForVolumeMountPoint",
			wantPath: testMountPoint,
		},
		{
			// The volume refuses its handle. Fails if the gather reads an
			// unopenable volume as an accepted directory, or hands a failed
			// createFile's InvalidHandle to the query.
			name:     "createFile refuses the handle",
			op:       "createFile",
			cause:    windows.ERROR_ACCESS_DENIED,
			wantOp:   "CreateFile",
			wantPath: testVolume,
		},
	}
	for _, tc := range failed {
		t.Run(tc.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			f := newFakeVolume()
			f.failNext(tc.op, tc.cause)

			wantCallFailure(t, checkDirVolume(f.table(), testDir), tc.wantOp, tc.wantPath, tc.cause)
		})
	}

	t.Run("closeHandle fails after a good query", func(t *testing.T) {
		prof.Watch(t, prof.Options{})

		// The query answered the flushing-on answer; only the close failed.
		// The gather returns the close failure as the Linux probe joins it,
		// and the verdict never runs: the check fails on the call, never as
		// a silent pass. Fails if the gather swallows a close error, or
		// reaches the verdict anyway on an answer whose probe leaked its
		// handle.

		f := newFakeVolume()
		f.failNext("closeHandle", windows.ERROR_INVALID_HANDLE)

		wantCallFailure(t, checkDirVolume(f.table(), testDir), "CloseHandle", testVolume, windows.ERROR_INVALID_HANDLE)
	})
}

// TestCheckWriteCachePolicyVolumeCalls fails a probe that asks a call the
// wrong question, takes the wrong buffer sizes or fails to close the volume:
// the mount point is asked of the directory, the GUID path of the mount
// point, the volume with the share and disposition the package's own opens
// use and no access, the property query with the property and the standard
// query type in its input; and the answer is long enough that no Size that
// counts data past the structure is clipped, while the close runs exactly
// once on the handle the open answered.
func TestCheckWriteCachePolicyVolumeCalls(t *testing.T) {
	prof.Watch(t, prof.Options{})

	f := newFakeVolume()

	p, err := probeVolume(f.table(), testDir)
	if err != nil {
		t.Fatalf("probeVolume: got %v, want the recorded answers gathered", err)
	}

	// Fails if the probe fills any field from the wrong call.
	if p.dir != testDir || p.mountPoint != testMountPoint || p.volume != testVolume {
		t.Errorf("got dir %q, mount point %q, volume %q; want %q, %q, %q",
			p.dir, p.mountPoint, p.volume, testDir, testMountPoint, testVolume)
	}
	if p.queryErr != 0 {
		t.Errorf("got query error %d, want 0 from the recorded answer", p.queryErr)
	}
	if want := flushingOn(); string(p.answer) != string(want) {
		t.Errorf("got answer % x, want the %d bytes returned % x", p.answer, len(want), want)
	}

	// GetVolumePathName answers with the root of the volume the directory is
	// on; the buffer takes the largest path it may return, since a call
	// that cannot return its answer fails (GetVolumePathNameW, fileapi.h).
	got := f.callsOf("getVolumePathName")
	if len(got) != 1 || got[0].path != testDir {
		t.Fatalf("got %v, want one getVolumePathName on %s", got, testDir)
	}
	if got[0].buffer < 512 {
		t.Errorf("%v: buffer of %d, want at least 512 UTF-16 characters", got[0], got[0].buffer)
	}

	// GetVolumeNameForVolumeMountPoint answers the volume GUID path of the
	// mount point, in the buffer "50 characters" its doc calls reasonable
	// for "the largest possible volume GUID path"
	// (GetVolumeNameForVolumeMountPointW, fileapi.h). Fails if the mount
	// point stated first is not the one PathName answered.
	got = f.callsOf("getVolumeNameForVolumeMountPoint")
	if len(got) != 1 || got[0].path != testMountPoint {
		t.Fatalf("got %v, want one getVolumeNameForVolumeMountPoint on %s", got, testMountPoint)
	}
	if got[0].buffer < 50 {
		t.Errorf("%v: buffer of %d, want at least 50 UTF-16 characters", got[0], got[0].buffer)
	}

	// The volume is opened on the handle the property query needs: access 0,
	// which CreateFile's dwDesiredAccess documents for "query device
	// attributes without accessing a device" (CreateFileW, fileapi.h), and
	// the share and disposition the package's own opens set
	// (os_windows.go's decision on FILE_SHARE_DELETE). Fails if any of them
	// differs, or the query and the close do not act on its handle.
	opens := f.callsOf("createFile")
	if len(opens) != 1 || opens[0].path != testVolume {
		t.Fatalf("got %v, want one createFile on %s", opens, testVolume)
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

	queries := f.callsOf("deviceIoControl")
	if len(queries) != 1 || queries[0].handle != open.handle {
		t.Fatalf("got %v, want one deviceIoControl on the opened handle %#x", queries, open.handle)
	}
	query := queries[0]
	if query.code != ioctlStorageQueryProperty {
		t.Errorf("%v: code %#x, want IOCTL_STORAGE_QUERY_PROPERTY %#x", query, query.code, ioctlStorageQueryProperty)
	}

	// The input is STORAGE_PROPERTY_QUERY: the property the answer decodes
	// in cachepolicy.go and the standard query, as two 4-byte enumerations
	// (ntddstor.h). Fails if the property or the query type is wrong, or
	// the input is not the two enumerations.
	if query.inLen != 8 {
		t.Errorf("%v: input of %d bytes, want the 8 of STORAGE_PROPERTY_QUERY", query, query.inLen)
	}
	if len(query.query) >= 8 {
		if got := binary.LittleEndian.Uint32(query.query[0:]); got != storageDeviceWriteCacheProperty {
			t.Errorf("%v: PropertyId %d, want StorageDeviceWriteCacheProperty (%d)", query, got, storageDeviceWriteCacheProperty)
		}
		if got := binary.LittleEndian.Uint32(query.query[4:]); got != propertyStandardQuery {
			t.Errorf("%v: QueryType %d, want PropertyStandardQuery (%d)", query, got, propertyStandardQuery)
		}
	}

	// The answer buffer holds the structure and room past it, since Size
	// "may include data that follows this structure" and bytesReturned
	// counts it. Fails if a Size past 28 is clipped short of its bytes.
	if query.outLen < 512 {
		t.Errorf("%v: answer buffer of %d bytes, want at least 512 so data the Size counts is not clipped", query, query.outLen)
	}

	// The handle is closed exactly once, by the same one the open answered.
	closes := f.callsOf("closeHandle")
	if len(closes) != 1 || closes[0].handle != open.handle {
		t.Errorf("got %v, want one closeHandle on the opened handle %#x", closes, open.handle)
	}
}

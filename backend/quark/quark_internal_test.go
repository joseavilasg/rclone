package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/quark/api"
	"github.com/rclone/rclone/fs"
	fshash "github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitRemote(t *testing.T) {
	for _, tc := range []struct {
		in            string
		wantDir, want string
	}{
		{in: "", wantDir: "", want: ""},
		{in: "file.txt", wantDir: "", want: "file.txt"},
		{in: "dir/file.txt", wantDir: "dir", want: "file.txt"},
		{in: "/a/b/c.txt", wantDir: "a/b", want: "c.txt"},
	} {
		dir, leaf := splitRemote(tc.in)
		assert.Equal(t, tc.wantDir, dir, tc.in)
		assert.Equal(t, tc.want, leaf, tc.in)
	}
}

func TestCookieRotation(t *testing.T) {
	old := "__pus=ab; __puus=xyz"
	rotated := api.RotateCookie(old, map[string]string{"__puus": "NEW"})
	assert.Contains(t, rotated, "__puus=NEW")
	assert.False(t, strings.Contains(rotated, "__puus=xyz"))
	assert.Contains(t, rotated, "__pus=ab")
}

func TestNameEncoding(t *testing.T) {
	enc := encoder.MultiEncoder(encoder.Display | encoder.EncodeInvalidUtf8)
	// Quark accepts most characters; standard escaping must survive a roundtrip
	for _, name := range []string{"simple.txt", "ünïcode file.txt", "per%cent.txt", "sym(bol)file.txt"} {
		got := enc.ToStandardName(enc.FromStandardName(name))
		assert.Equal(t, name, got, name)
	}
	// path separators are not legal file names and are always escaped
	escaped := enc.FromStandardName("a/b.txt")
	assert.Equal(t, "a／b.txt", escaped)
}

func TestResolveRootCache(t *testing.T) {
	f := &Fs{
		dirIDs: map[string]string{},
		opt:    Options{RootFolderID: "0"},
	}
	id, err := f.resolveDir(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "0", id)
	assert.Equal(t, "0", f.dirIDs[""])
}

// testCDN is a fake quark API + CDN. /file/download signs a download URL that
// points back at /obj, which honors Range requests. It records every signing
// and every Range header so tests can pin URL reuse and segmentation.
type testCDN struct {
	data        []byte
	ignoreRange bool // answer 200 with the whole body instead of 206 ranges
	failFirst   bool // reject the first /obj request with 403 (stale signature)

	mu       sync.Mutex
	signings int
	ranges   []string
}

func (c *testCDN) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/file/download"):
			c.recordSigning()
			io.WriteString(w, `{"status":200,"code":0,"data":[{"download_url":"http://`+r.Host+`/obj"}]}`)
		case r.URL.Path == "/obj":
			c.serveObject(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// recordSigning counts a download URL signing request.
func (c *testCDN) recordSigning() {
	c.mu.Lock()
	c.signings++
	c.mu.Unlock()
}

// serveObject serves c.data honoring Range requests, replaying the edge
// behaviors the tests pin down: failFirst rejects the first /obj request
// with 403 and ignoreRange answers 200 with the whole body.
func (c *testCDN) serveObject(w http.ResponseWriter, r *http.Request) {
	failFirst := c.failFirst
	c.mu.Lock()
	c.ranges = append(c.ranges, r.Header.Get("Range"))
	first := len(c.ranges) == 1
	c.mu.Unlock()
	if failFirst && first {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if c.ignoreRange {
		w.Header().Set("Content-Length", strconv.Itoa(len(c.data)))
		_, _ = w.Write(c.data)
		return
	}
	var start, end int64
	if _, err := fmt.Sscanf(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "%d-%d", &start, &end); err != nil || start < 0 {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end > int64(len(c.data)-1) {
		end = int64(len(c.data) - 1)
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(c.data)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(c.data[start : end+1])
}

func (c *testCDN) signingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.signings
}

func (c *testCDN) allRanges() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ranges...)
}

// newRangeTestObject wires an Object to a testCDN through a real-ish client so
// Open exercises the full API signing + segmented download path.
func newRangeTestObject(t *testing.T, cdn *testCDN) (*Object, *api.Client) {
	srv := httptest.NewServer(cdn.handler())
	t.Cleanup(srv.Close)
	client := api.NewClient(context.Background(), srv.Client(), srv.Client(), srv.URL+"/1/clouddrive", "https://pan.quark.cn", "ucpro", "")
	o := &Object{
		fs:     &Fs{client: client},
		remote: "big.mkv",
		info:   api.File{Fid: "abc", Size: int64(len(cdn.data))},
	}
	return o, client
}

// TestOpenSegmentsFullSpan pins that a full-file read is served as a chain of
// <=10 MiB ranged GETs over one signed URL.
func TestOpenSegmentsFullSpan(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 25*1024*1024)
	cdn := &testCDN{data: data}
	o, _ := newRangeTestObject(t, cdn)

	rc, err := o.Open(context.Background())
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	assert.Equal(t, data, got)
	assert.Equal(t, []string{
		"bytes=0-10485759",
		"bytes=10485760-20971519",
		"bytes=20971520-26214399",
	}, cdn.allRanges(), "the span must be served as 10MiB segments")
	assert.Equal(t, 1, cdn.signingCount(), "one signed URL must serve every segment")
}

// TestOpenSegmentsPartialSpan pins that a requested sub-range maps to exact
// segment GETs starting at the offset.
func TestOpenSegmentsPartialSpan(t *testing.T) {
	data := bytes.Repeat([]byte{0xCD}, 30*1024*1024)
	cdn := &testCDN{data: data}
	o, _ := newRangeTestObject(t, cdn)

	rc, err := o.Open(context.Background(), &fs.RangeOption{Start: 3 * 1024 * 1024, End: 22*1024*1024 - 1})
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	assert.Equal(t, data[3*1024*1024:22*1024*1024], got)
	assert.Equal(t, []string{
		"bytes=3145728-13631487",
		"bytes=13631488-23068671",
	}, cdn.allRanges())
}

// TestOpenReusesSignedURL pins that concurrent chunk opens of one object share
// a single signed URL (the whole point of the URL cache).
func TestOpenReusesSignedURL(t *testing.T) {
	data := bytes.Repeat([]byte{0x11}, 5*1024*1024)
	cdn := &testCDN{data: data}
	o, _ := newRangeTestObject(t, cdn)

	rc1, err := o.Open(context.Background())
	require.NoError(t, err)
	rc2, err := o.Open(context.Background())
	require.NoError(t, err)
	defer rc1.Close()
	defer rc2.Close()

	assert.Equal(t, 1, cdn.signingCount(), "concurrent chunk opens must share one signed URL")
}

// TestOpenResignsOnStaleURL pins that a 4xx from the CDN drops the cached URL
// and re-signs exactly once instead of failing the transfer.
func TestOpenResignsOnStaleURL(t *testing.T) {
	data := bytes.Repeat([]byte{0x77}, 12*1024*1024)
	cdn := &testCDN{data: data, failFirst: true}
	o, _ := newRangeTestObject(t, cdn)

	rc, err := o.Open(context.Background())
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	assert.Equal(t, data, got)
	assert.Equal(t, 2, cdn.signingCount(), "a rejected URL must drop the cache and re-sign once")
	assert.Equal(t, 3, len(cdn.allRanges()), "1 rejected GET + 2 good segments")
}

// TestOpenRangeIgnored pins that a 200 answer to a Range request surfaces
// fs.ErrorRangeIgnored (rclone then falls back to a non-multithreaded copy).
func TestOpenRangeIgnored(t *testing.T) {
	data := bytes.Repeat([]byte{0x42}, 2*1024*1024)
	cdn := &testCDN{data: data, ignoreRange: true}
	o, _ := newRangeTestObject(t, cdn)

	rc, err := o.Open(context.Background())
	require.NoError(t, err)
	defer rc.Close()
	buf := make([]byte, 1024)
	_, err = rc.Read(buf)
	assert.Equal(t, fs.ErrorRangeIgnored, err, "a 200 answer to a Range request must surface ErrorRangeIgnored")
}

// TestDownloadURLIsCachedPerFid pins the client-level URL cache: same fid
// reuses the signature, DropDownloadURL forces a fresh signing.
func TestDownloadURLIsCachedPerFid(t *testing.T) {
	cdn := &testCDN{data: []byte("x")}
	srv := httptest.NewServer(cdn.handler())
	t.Cleanup(srv.Close)
	client := api.NewClient(context.Background(), srv.Client(), srv.Client(), srv.URL+"/1/clouddrive", "https://pan.quark.cn", "ucpro", "")

	u1, err := client.DownloadURL(context.Background(), "fid-1")
	require.NoError(t, err)
	u2, err := client.DownloadURL(context.Background(), "fid-1")
	require.NoError(t, err)
	assert.Equal(t, u1, u2)
	assert.Equal(t, 1, cdn.signingCount())

	client.DropDownloadURL("fid-1")
	u3, err := client.DownloadURL(context.Background(), "fid-1")
	require.NoError(t, err)
	assert.Equal(t, u1, u3)
	assert.Equal(t, 2, cdn.signingCount(), "dropping the cache must force a fresh signing")
}

// TestUploadCommitDedupeVisible pins that the instant-dedupe path waits the
// visibility throttle before returning, so the object is findable right after
// the commit. The upload edge creates the entry asynchronously: the fake
// /file/sort only lists the file 500 ms after the hash check, while the full
// commit path already slept 1 s. Without the throttle on the dedupe branch the
// lookup fails with "failed to find object after copy".
func TestUploadCommitDedupeVisible(t *testing.T) {
	type apiState struct {
		mu       sync.Mutex
		hashTime time.Time
	}
	st := &apiState{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/file/upload/pre"):
			io.WriteString(w, `{"status":200,"code":0,"data":{"task_id":"t1","upload_id":"up1","obj_key":"o1","upload_url":"x","fid":"f1","bucket":"b","auth_info":"a"},"metadata":{"part_size":4194304}}`)
		case strings.Contains(r.URL.Path, "/file/update/hash"):
			st.mu.Lock()
			st.hashTime = time.Now()
			st.mu.Unlock()
			io.WriteString(w, `{"status":200,"code":0,"data":{"finish":true,"fid":"f1"}}`)
		case strings.Contains(r.URL.Path, "/file/sort"):
			st.mu.Lock()
			visible := !st.hashTime.IsZero() && time.Since(st.hashTime) >= 500*time.Millisecond
			st.mu.Unlock()
			list := "[]"
			if visible {
				list = `[{"fid":"f1","file_name":"dup.bin","size":12,"file":true}]`
			}
			data := `{"status":200,"code":0,"data":{"list":` + list + `},"metadata":{"_size":100,"_page":1,"_count":0,"_total":0,"way":"normal"}}`
			io.WriteString(w, data)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ctx, _ := fs.AddConfig(context.Background())
	client := api.NewClient(ctx, srv.Client(), srv.Client(), srv.URL+"/1/clouddrive", "https://pan.quark.cn", "ucpro", "")
	f := &Fs{
		client: client,
		dirIDs: map[string]string{},
		opt:    Options{RootFolderID: "0"},
	}

	pre, err := f.client.UploadPre(ctx, "dup.bin", "0", 12, "text/plain")
	require.NoError(t, err)

	require.NoError(t, f.uploadCommit(ctx, pre, "d41d8cd98f00b204e9800998ecf8427e", "da39a3ee5e6b4b0d3255bfef95601890afd80709", []string{"etag1"}))

	o, err := f.NewObject(ctx, "dup.bin")
	require.NoError(t, err, "object must be findable immediately after an instant-deduped commit")
	assert.Equal(t, int64(12), o.Size())
}

// newUploadAPIFake is a fake quark upload API: pre + hash pretending a task.
// Every OSS-flavored endpoint (auth/commit/finish) fails loudly so any
// accidental network to them trips the test. The listing advertises one object
// so NewObject succeeds after an instant-dedupe.
func newUploadAPIFake(t *testing.T, hashFinish bool, hashCount *int) (*Fs, context.Context) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/file/upload/pre"):
			io.WriteString(w, `{"status":200,"code":0,"data":{"task_id":"t1","upload_id":"up1","obj_key":"o1","upload_url":"x","fid":"f1","bucket":"b","auth_info":"a"},"metadata":{"part_size":4194304}}`)
		case strings.Contains(r.URL.Path, "/file/update/hash"):
			if hashCount != nil {
				*hashCount++
			}
			finish := "false"
			if hashFinish {
				finish = "true"
			}
			io.WriteString(w, `{"status":200,"code":0,"data":{"finish":`+finish+`,"fid":"f1"}}`)
		case strings.Contains(r.URL.Path, "/file/sort"):
			io.WriteString(w, `{"status":200,"code":0,"data":{"list":[{"fid":"f1","file_name":"dup.bin","size":12,"file":true}]},"metadata":{"_size":100,"_page":1,"_count":0,"_total":0,"way":"normal"}}`)
		default:
			t.Errorf("unexpected upload API call: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	ctx, _ := fs.AddConfig(context.Background())
	client := api.NewClient(ctx, srv.Client(), srv.Client(), srv.URL+"/1/clouddrive", "https://pan.quark.cn", "ucpro", "")
	f := &Fs{
		client: client,
		dirIDs: map[string]string{},
		opt:    Options{RootFolderID: "0"},
	}
	return f, ctx
}

func dupSrc() fs.ObjectInfo {
	return object.NewStaticObjectInfo("dup.bin", time.Now(), 12, true, map[fshash.Type]string{
		fshash.MD5:  "bf13fc19e5151ac57d4252e0e0f87abe",
		fshash.SHA1: "3ab6543c08a75f292a5ecedac87ec41642d12166",
	}, nil)
}

func noHashSrc() fs.ObjectInfo {
	return object.NewStaticObjectInfo("dup.bin", time.Now(), 12, true, nil, nil)
}

// readCounter counts how many bytes a wrapped reader actually provided, to
// prove a stream was (or was not) consumed.
type readCounter struct {
	r io.Reader
	n *int64
}

func (rc *readCounter) Read(p []byte) (int, error) {
	n, err := rc.r.Read(p)
	if rc.n != nil {
		*rc.n += int64(n)
	}
	return n, err
}

// TestPutPrecheckInstantDedupe pins that Put asks the server with the
// source-provided hashes BEFORE touching the stream: on a hit the entry is
// created instantly, no part is uploaded, the source stream is never read and
// the object comes back from a fresh lookup.
func TestPutPrecheckInstantDedupe(t *testing.T) {
	var hashCount int
	f, ctx := newUploadAPIFake(t, true, &hashCount)

	var reads int64
	counted := &readCounter{r: bytes.NewReader([]byte("hello world!")), n: &reads}
	o, err := f.Put(ctx, counted, dupSrc())
	require.NoError(t, err)
	assert.Equal(t, int64(12), o.Size())
	assert.Equal(t, int64(0), reads, "the source stream must never be read on a dedupe hit")
	assert.Equal(t, 1, hashCount, "update/hash runs exactly once, as the pre-check")
}

// TestOpenChunkWriterPrecheckDedupe pins that OpenChunkWriter runs the
// pre-check before any chunk is handed in and, on a hit, returns a writer that
// consumes and discards chunks without ever uploading a part.
func TestOpenChunkWriterPrecheckDedupe(t *testing.T) {
	var hashCount int
	f, ctx := newUploadAPIFake(t, true, &hashCount)

	info, w, err := f.OpenChunkWriter(ctx, "dup.bin", dupSrc())
	require.NoError(t, err)
	cw, ok := w.(*chunkWriter)
	require.True(t, ok, "OpenChunkWriter must return a *chunkWriter")
	assert.True(t, cw.deduped, "on a hit the writer must run in dedupe mode")
	alreadyExists, ok := w.(fs.ChunkWriterAlreadyExistser)
	require.True(t, ok, "the chunk writer must implement fs.ChunkWriterAlreadyExistser")
	assert.True(t, alreadyExists.AlreadyExists(), "a dedupe-hit writer must report AlreadyExists so the core skips the transfer")
	assert.Equal(t, int64(4194304), info.ChunkSize)

	n, err := w.WriteChunk(ctx, 0, bytes.NewReader([]byte("hello world!")))
	require.NoError(t, err)
	assert.Equal(t, int64(12), n, "the chunk must be consumed so the read loop keeps working")
	require.NoError(t, w.Close(ctx))

	assert.Equal(t, 1, hashCount, "the pre-check is the only hash round trip")
}

// TestOpenChunkWriterPrecheckMiss pins that on a miss (or when the source has
// no hashes) OpenChunkWriter is NOT in dedupe mode and the upload proceeds
// through the normal writer.
func TestOpenChunkWriterPrecheckMiss(t *testing.T) {
	// miss with source hashes: the pre-check runs once, then a normal writer
	var hashCount int
	f, ctx := newUploadAPIFake(t, false, &hashCount)
	_, w, err := f.OpenChunkWriter(ctx, "dup.bin", dupSrc())
	require.NoError(t, err)
	assert.False(t, w.(*chunkWriter).deduped, "a miss must keep the normal upload path")
	assert.False(t, w.(fs.ChunkWriterAlreadyExistser).AlreadyExists(), "a miss must not report AlreadyExists or the core would skip a real upload")
	assert.Equal(t, 1, hashCount)

	// source without hashes: the pre-check is skipped entirely
	var noHashCount int
	f2, ctx2 := newUploadAPIFake(t, true, &noHashCount)
	_, w2, err := f2.OpenChunkWriter(ctx2, "dup.bin", noHashSrc())
	require.NoError(t, err)
	assert.False(t, w2.(*chunkWriter).deduped)
	assert.Equal(t, 0, noHashCount, "a hash-less source must skip the pre-check entirely")
}

// seekHostileReader fails any Seek: readChunkFull must consume a chunk in one
// forward pass so the core may stream live network readers unbuffered.
type seekHostileReader struct {
	io.Reader
	seeks int
}

func (r *seekHostileReader) Seek(offset int64, whence int) (int64, error) {
	r.seeks++
	return 0, fmt.Errorf("seek not supported")
}

// TestReadChunkFullNoSeek pins the ChunkWriterDoesntSeek contract: a full
// chunk is consumed without a single seek.
func TestReadChunkFullNoSeek(t *testing.T) {
	data := []byte("chunk-data-12345")
	r := &seekHostileReader{Reader: bytes.NewReader(data)}
	buf, err := readChunkFull(r)
	require.NoError(t, err)
	assert.Equal(t, data, buf)
	assert.Equal(t, 0, r.seeks, "chunk reads must never seek")
}

// TestPumpUploadsInOrder pins the ordered-pump contract: parts staged out of
// order upload strictly ascending, hash exactly like a sequential pass and
// land etags by position, so the commit matches a single-stream upload.
func TestPumpUploadsInOrder(t *testing.T) {
	const parts = 4
	chunks := make([][]byte, parts)
	var sequential []byte
	for i := range chunks {
		chunks[i] = []byte(fmt.Sprintf("part-%d-payload-", i) + strings.Repeat("x", 1000+i))
		sequential = append(sequential, chunks[i]...)
	}
	wantMD5 := md5.Sum(sequential)

	w := newChunkWriter(nil, api.UpPreResp{}, "", false, parts)
	var mu sync.Mutex
	var order []int
	w.upload = func(ctx context.Context, idx int, buf []byte) (string, error) {
		mu.Lock()
		order = append(order, idx)
		mu.Unlock()
		assert.Equal(t, chunks[idx], buf)
		return fmt.Sprintf("etag-%d", idx), nil
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	// Stage in reverse from goroutines; uploads must still run 0,1,2,3.
	for i := parts - 1; i >= 0; i-- {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			require.NoError(t, w.stageAndPump(ctx, idx, chunks[idx]))
		}(i)
	}
	wg.Wait()

	assert.Equal(t, []int{0, 1, 2, 3}, order, "parts must upload in chunk order")
	assert.Equal(t, wantMD5[:], w.md5Sum.Sum(nil), "pumped parts must hash like a sequential pass")
	for i := 0; i < parts; i++ {
		assert.Equal(t, fmt.Sprintf("etag-%d", i), w.etags[i], "etags must land by chunk position")
	}
	assert.Equal(t, 0, w.buffered)
	assert.Equal(t, parts, w.nextUpload)
}

// TestPumpBoundDrains pins the memory bound: past maxBufferedParts a stage
// blocks until the missing head arrives and drains.
func TestPumpBoundDrains(t *testing.T) {
	const parts = 10
	w := newChunkWriter(nil, api.UpPreResp{}, "", false, parts)
	var mu sync.Mutex
	var order []int
	w.upload = func(ctx context.Context, idx int, buf []byte) (string, error) {
		mu.Lock()
		order = append(order, idx)
		mu.Unlock()
		return fmt.Sprintf("etag-%d", idx), nil
	}
	ctx := context.Background()
	payload := func(i int) []byte { return fmt.Appendf(nil, "p%d", i) }
	// Stage 1..8 without the head: buffered but nothing uploads.
	for i := 1; i <= 8; i++ {
		require.NoError(t, w.stageAndPump(ctx, i, payload(i)))
	}
	// The 9th stage must block past the bound until the head drains.
	blocked := make(chan error, 1)
	go func() { blocked <- w.stageAndPump(ctx, 9, payload(9)) }()
	select {
	case err := <-blocked:
		t.Fatalf("stage past the bound returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// The head arrives and drains everything in order.
	require.NoError(t, w.stageAndPump(ctx, 0, payload(0)))
	select {
	case err := <-blocked:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("bound waiter never woke after the head drained")
	}
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, order)
	assert.Equal(t, 0, w.buffered)
}

// TestPumpFailureWakes pins failure liveness: a failed part wakes bound
// waiters with an error instead of hanging them.
func TestPumpFailureWakes(t *testing.T) {
	const parts = 10
	w := newChunkWriter(nil, api.UpPreResp{}, "", false, parts)
	w.upload = func(ctx context.Context, idx int, buf []byte) (string, error) {
		if idx == 0 {
			return "", fmt.Errorf("boom")
		}
		return fmt.Sprintf("etag-%d", idx), nil
	}
	ctx := context.Background()
	payload := func(i int) []byte { return fmt.Appendf(nil, "p%d", i) }
	for i := 1; i <= 8; i++ {
		require.NoError(t, w.stageAndPump(ctx, i, payload(i)))
	}
	blocked := make(chan error, 1)
	go func() { blocked <- w.stageAndPump(ctx, 9, payload(9)) }()
	// The head fails: both its own stage and the bound waiter must error out.
	err := w.stageAndPump(ctx, 0, payload(0))
	require.Error(t, err)
	select {
	case err := <-blocked:
		require.Error(t, err, "a bound waiter must wake with the pump failure")
	case <-time.After(10 * time.Second):
		t.Fatal("bound waiter hung on a failed pump")
	}
}

package baidunetdisk

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/baidunetdisk/api"
	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	fshash "github.com/rclone/rclone/fs/hash"
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

// testAPI is a fake Baidu Netdisk API + CDN. It serves listings, the three
// download-link mechanisms (official via a no-follow HEAD chase, crack, and
// crack_video), token refresh, quota, filemanager, and the full upload
// sequence (uinfo, rapid create, precreate, slices, final create,
// locateupload). The CDN /final honors Range requests and records every Range
// header so tests can pin URL reuse.
type testAPI struct {
	data []byte

	mu      sync.Mutex
	calls   []string
	created map[string]bool // absolute dir paths created via filemanager create
	ranges  []string

	vip          int             // vip_type answered by uinfo
	rapid        map[string]bool // content md5s the server already has (秒传)
	files        map[string]int64
	nextID       int
	precreates   int
	superfiles   []string // "uploadid:partseq:bodybytes" per uploaded slice
	sliceData    map[string][]byte
	failUploadID string // uploadid whose slices answer "uploadid expired"
	locateCalls  int
}

func (a *testAPI) recordCall(r *http.Request) {
	a.mu.Lock()
	a.calls = append(a.calls, r.URL.RequestURI())
	a.mu.Unlock()
}

func (a *testAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.recordCall(r)
		host := "http://" + r.Host
		switch {
		case r.URL.Path == "/renew":
			// online token refresh proxy
			io.WriteString(w, `{"refresh_token":"rt2","access_token":"at2"}`)
		case r.URL.Path == "/rest/2.0/xpan/file" && r.URL.Query().Get("method") == "list":
			a.serveList(w, r)
		case r.URL.Path == "/rest/2.0/xpan/file" && r.URL.Query().Get("method") == "create":
			a.serveCreate(w, r)
		case r.URL.Path == "/rest/2.0/xpan/file" && r.URL.Query().Get("method") == "precreate":
			a.servePrecreate(w, r)
		case r.URL.Path == "/rest/2.0/xpan/file" && r.URL.Query().Get("method") == "filemanager":
			a.serveFilemanager(w, r)
		case r.URL.Path == "/rest/2.0/xpan/nas":
			// uinfo: account tier driving the slice size
			io.WriteString(w, `{"errno":0,"vip_type":`+strconv.Itoa(a.vip)+`}`)
		case r.URL.Path == "/rest/2.0/pcs/file":
			// locateupload: the first server wins
			a.mu.Lock()
			a.locateCalls++
			a.mu.Unlock()
			io.WriteString(w, `{"servers":[{"server":"`+host+`"}]}`)
		case r.URL.Path == "/rest/2.0/pcs/superfile2":
			a.serveSuperfile2(w, r)
		case r.URL.Path == "/rest/2.0/xpan/multimedia":
			// official: dlink points at the redirect chase endpoint
			io.WriteString(w, `{"errno":0,"list":[{"dlink":"`+host+`/dl?sign=abc"}]}`)
		case r.URL.Path == "/api/filemetas":
			// crack: direct dlink to the CDN
			io.WriteString(w, `{"errno":0,"info":[{"dlink":"`+host+`/final"}]}`)
		case r.URL.Path == "/api/mediainfo":
			// crack_video: payload wrapped in errno 31023
			io.WriteString(w, `{"errno":31023,"info":[{"dlink":"`+host+`/final"}]}`)
		case r.URL.Path == "/api/quota":
			io.WriteString(w, `{"errno":0,"total":1000,"used":200}`)
		case r.URL.Path == "/dl":
			// HEAD chase: never follow, return the CDN Location
			w.Header().Set("Location", host+"/final")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/final":
			a.serveFinal(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// serveList lists the children of an absolute dir path (anchored at "/"):
// the drive root always has subdir and file.bin, and every dir created through
// the filemanager is listed under its parent.
func (a *testAPI) serveList(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	a.mu.Lock()
	created := make([]string, 0)
	for p := range a.created {
		parent, leaf := splitRemote(p)
		if parent == strings.Trim(dir, "/") {
			created = append(created, leaf)
		}
	}
	a.mu.Unlock()

	var out strings.Builder
	if dir == "/" || dir == "" {
		out.WriteString(`{"fs_id":10,"server_filename":"subdir","isdir":1,"path":"/subdir","server_mtime":1700000000,"size":0},`)
		fmt.Fprintf(&out, `{"fs_id":11,"server_filename":"file.bin","isdir":0,"path":"/file.bin","server_mtime":1700000001,"size":%d},`, len(a.data))
	}
	for i, leaf := range created {
		out.WriteString(fmt.Sprintf(`{"fs_id":%d,"server_filename":%q,"isdir":1,"path":"%s","server_mtime":1700000002,"size":0}`,
			1000+i, leaf, "/"+strings.Trim(dir, "/")+"/"+leaf))
		out.WriteString(",")
	}
	// Uploaded files recorded through the upload creates.
	a.mu.Lock()
	fid := 3000
	for p, size := range a.files {
		parent, leaf := splitRemote(strings.Trim(p, "/"))
		if parent == strings.Trim(dir, "/") {
			out.WriteString(fmt.Sprintf(`{"fs_id":%d,"server_filename":%q,"isdir":0,"path":%q,"server_mtime":1700000003,"size":%d}`,
				fid, leaf, p, size))
			out.WriteString(",")
			fid++
		}
	}
	a.mu.Unlock()
	resp := `{"errno":0,"list":[` + strings.TrimSuffix(out.String(), ",") + `]}`
	io.WriteString(w, resp)
}

// serveCreate routes method=create between directory creation (isdir=1) and
// file creation (the rapid and the final upload creates).
func (a *testAPI) serveCreate(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.PostForm.Get("isdir") == "1" {
		a.serveCreateDir(w, r)
		return
	}
	a.serveCreateFile(w, r)
}

// serveCreateDir creates a directory via method=create (form body has the path).
func (a *testAPI) serveCreateDir(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	dir := strings.Trim(r.PostForm.Get("path"), "/")
	if dir == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	if a.created == nil {
		a.created = map[string]bool{}
	}
	a.created[dir] = true
	a.mu.Unlock()
	io.WriteString(w, `{"errno":0}`)
}

// nextFsID hands out fake fs_ids for uploaded files.
func (a *testAPI) nextFsID() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextID++
	return 2000 + a.nextID
}

// recordFile registers an uploaded file so listings find it.
func (a *testAPI) recordFile(absPath string, size int64) int {
	id := a.nextFsID()
	a.mu.Lock()
	if a.files == nil {
		a.files = map[string]int64{}
	}
	a.files[absPath] = size
	a.mu.Unlock()
	return id
}

// fileJSON renders a File body for create/precreate answers.
func fileJSON(id int, absPath string, size int64) string {
	leaf := absPath[strings.LastIndex(absPath, "/")+1:]
	return fmt.Sprintf(`{"errno":0,"fs_id":%d,"path":%q,"size":%d,"server_filename":%q,"isdir":0,"server_mtime":1700000003}`, id, absPath, size, leaf)
}

// serveCreateFile answers the upload creates. A create without uploadid is a
// rapid create (秒传): it succeeds only when the single block md5 is one the
// server already has, else errno 3111. A create with uploadid is the final
// create, which records the file and succeeds.
func (a *testAPI) serveCreateFile(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := r.PostForm
	absPath := form.Get("path")
	size, _ := strconv.ParseInt(form.Get("size"), 10, 64)
	uploadID := form.Get("uploadid")
	var blocks []string
	_ = json.Unmarshal([]byte(form.Get("block_list")), &blocks)
	if uploadID == "" {
		a.mu.Lock()
		known := len(blocks) == 1 && a.rapid[blocks[0]]
		a.mu.Unlock()
		if !known {
			io.WriteString(w, `{"errno":3111}`)
			return
		}
		id := a.recordFile(absPath, size)
		io.WriteString(w, fileJSON(id, absPath, size))
		return
	}
	id := a.recordFile(absPath, size)
	io.WriteString(w, fileJSON(id, absPath, size))
}

// servePrecreate answers the pre-upload step. When the content-md5 is one the
// server already has it answers return_type 2 (instant, with the File);
// otherwise return_type 1 with a fresh uploadid and one partseq per slice.
func (a *testAPI) servePrecreate(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := r.PostForm
	absPath := form.Get("path")
	size, _ := strconv.ParseInt(form.Get("size"), 10, 64)
	var blocks []string
	_ = json.Unmarshal([]byte(form.Get("block_list")), &blocks)
	a.mu.Lock()
	a.precreates++
	uploadID := fmt.Sprintf("up-%d", a.precreates)
	rapid := a.rapid[form.Get("content-md5")]
	a.mu.Unlock()
	if rapid && form.Get("content-md5") != "" {
		id := a.recordFile(absPath, size)
		io.WriteString(w, `{"errno":0,"return_type":2,"info":`+fileJSON(id, absPath, size)+`}`)
		return
	}
	seqs := make([]string, len(blocks))
	for i := range blocks {
		seqs[i] = strconv.Itoa(i)
	}
	io.WriteString(w, `{"errno":0,"return_type":1,"path":`+strconv.Quote(absPath)+`,"uploadid":`+strconv.Quote(uploadID)+`,"block_list":[`)
	io.WriteString(w, strings.Join(seqs, ","))
	io.WriteString(w, `]}`)
}

// serveSuperfile2 receives one slice per call and records
// "uploadid:partseq:bodybytes". The raw slice bytes are extracted from the
// multipart file part so tests can reassemble the upload. The flagged
// uploadid answers the expired marker so the client recreates the upload from
// scratch.
func (a *testAPI) serveSuperfile2(w http.ResponseWriter, r *http.Request) {
	uploadID := r.URL.Query().Get("uploadid")
	partseq := r.URL.Query().Get("partseq")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	// Extract the raw slice bytes from the multipart file part so tests can
	// reassemble the upload byte-for-byte.
	if _, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil {
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			if part.FormName() == "file" {
				if data, err := io.ReadAll(io.LimitReader(part, 64<<20)); err == nil {
					a.mu.Lock()
					if a.sliceData == nil {
						a.sliceData = map[string][]byte{}
					}
					a.sliceData[uploadID+":"+partseq] = data
					a.mu.Unlock()
				}
			}
		}
	}
	a.mu.Lock()
	fail := a.failUploadID != "" && uploadID == a.failUploadID
	a.superfiles = append(a.superfiles, fmt.Sprintf("%s:%s:%d", uploadID, partseq, len(body)))
	a.mu.Unlock()
	if fail {
		io.WriteString(w, `{"errno":31034,"info":"uploadid expired, please recreate"}`)
		return
	}
	io.WriteString(w, `{"errno":0}`)
}

// serveFilemanager handles delete with a JSON filelist.
func (a *testAPI) serveFilemanager(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	_ = r.PostForm.Get("opera")
	io.WriteString(w, `{"errno":0}`)
}

// serveFinal serves a.data honoring Range requests, matching the quark CDN
// fake so rclone's multithread copy path is exercised faithfully.
func (a *testAPI) serveFinal(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.ranges = append(a.ranges, r.Header.Get("Range"))
	a.mu.Unlock()
	var start, end int64
	if _, err := fmt.Sscanf(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), "%d-%d", &start, &end); err != nil || start < 0 {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end > int64(len(a.data)-1) {
		end = int64(len(a.data) - 1)
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(a.data)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(a.data[start : end+1])
}

func (a *testAPI) allRanges() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ranges...)
}

// newTestAPI wires a fake API server and an Fs pointed at it. mode selects
// the download API ("official", "crack", "crack_video") the client is
// configured with; "" defaults to official.
func newTestAPI(t *testing.T, data []byte, mode string) (*testAPI, *Fs, *api.Client) {
	if mode == "" {
		mode = "official"
	}
	a := &testAPI{data: data}
	srv := httptest.NewServer(a.handler())
	t.Cleanup(srv.Close)
	client := api.NewClient(context.Background(), srv.Client(), srv.Client(), srv.URL, api.Config{
		RefreshToken: "rt1",
		UseOnlineAPI: true,
		APIAddress:   srv.URL + "/renew",
		DownloadAPI:  mode,
		LocateBase:   srv.URL,
	})
	f := &Fs{
		client: client,
		root:   "",
		opt: Options{
			Enc:                 encoder.MultiEncoder(encoder.Display | encoder.EncodeInvalidUtf8),
			UploadAPI:           srv.URL,
			UseDynamicUploadAPI: false,
			UploadThread:        2,
			UploadTimeout:       60,
		},
	}
	return a, f, client
}

func TestList(t *testing.T) {
	_, f, _ := newTestAPI(t, []byte("0123456789"), "official")

	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, entries, 2)

	d := entries[0].(*fs.Dir)
	assert.Equal(t, "subdir", d.Remote())

	o := entries[1].(*Object)
	assert.Equal(t, "file.bin", o.Remote())
	assert.Equal(t, int64(10), o.Size())
	id, err := o.ID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "11", id)
}

func TestNewObject(t *testing.T) {
	_, f, _ := newTestAPI(t, []byte("0123456789"), "official")

	o, err := f.NewObject(context.Background(), "file.bin")
	require.NoError(t, err)
	assert.Equal(t, "file.bin", o.Remote())
	assert.Equal(t, int64(10), o.Size())

	_, err = f.NewObject(context.Background(), "missing.bin")
	assert.Equal(t, fs.ErrorObjectNotFound, err)

	_, err = f.NewObject(context.Background(), "subdir")
	assert.Equal(t, fs.ErrorIsDir, err)
}

// TestOpenOfficial pins the official download: dlink + neighbor chase with a
// no-follow HEAD, then a ranged GET reusing the resolved URL.
func TestOpenOfficial(t *testing.T) {
	data := []byte("official-download-bytes")
	a, f, _ := newTestAPI(t, data, "official")

	o, err := f.NewObject(context.Background(), "file.bin")
	require.NoError(t, err)

	rc, err := o.Open(context.Background(), &fs.RangeOption{Start: 2, End: 10})
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	assert.Equal(t, data[2:11], got)
	assert.Equal(t, []string{"bytes=2-10"}, a.allRanges())
	assert.Contains(t, strings.Join(a.calls, " "), "/dl", "the HEAD chase must hit the redirect endpoint")
}

// TestOpenCrack pins the crack download: a direct dlink from /api/filemetas,
// no HEAD chase.
func TestOpenCrack(t *testing.T) {
	data := []byte("crack-download-bytes")
	a, f, _ := newTestAPI(t, data, "crack")

	o, err := f.NewObject(context.Background(), "file.bin")
	require.NoError(t, err)

	rc, err := o.Open(context.Background(), &fs.RangeOption{Start: 0, End: 5})
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	assert.Equal(t, data[:6], got)
	assert.Equal(t, []string{"bytes=0-5"}, a.allRanges())
	assert.Contains(t, strings.Join(a.calls, " "), "/api/filemetas")
	assert.NotContains(t, strings.Join(a.calls, " "), "/dl", "crack must not chase redirects")
}

// TestOpenCrackVideo pins the crack_video download: mediainfo wraps its
// payload in errno 31023, which the unit must pass through unmodified.
func TestOpenCrackVideo(t *testing.T) {
	data := []byte("crack-video-bytes")
	a, f, _ := newTestAPI(t, data, "crack_video")

	o, err := f.NewObject(context.Background(), "file.bin")
	require.NoError(t, err)

	rc, err := o.Open(context.Background(), &fs.RangeOption{Start: 1, End: 4})
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	assert.Equal(t, data[1:5], got)
	assert.Contains(t, strings.Join(a.calls, " "), "/api/mediainfo")
}

// TestDownloadURLReuse pins the URL cache: the same fs_id reuses the
// signature and Open streams several ranged GETs over one link.
func TestDownloadURLReuse(t *testing.T) {
	a, f, _ := newTestAPI(t, []byte("0123456789"), "crack")

	o, err := f.NewObject(context.Background(), "file.bin")
	require.NoError(t, err)

	rc1, err := o.Open(context.Background(), &fs.RangeOption{Start: 0, End: 2})
	require.NoError(t, err)
	rc2, err := o.Open(context.Background(), &fs.RangeOption{Start: 3, End: 5})
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, rc1)
	_, _ = io.Copy(io.Discard, rc2)
	require.NoError(t, rc1.Close())
	require.NoError(t, rc2.Close())

	apiCalls := strings.Join(a.calls, " ")
	assert.Equal(t, 1, strings.Count(apiCalls, "/api/filemetas"), "one resolved URL must serve every ranged GET")
	assert.Equal(t, []string{"bytes=0-2", "bytes=3-5"}, a.allRanges())
}

// TestTokenRefreshPins the refresh dance: an errno 111 API answer triggers an
// online refresh with the new tokens persisted, and the request retries.
func TestTokenRefreshPins(t *testing.T) {
	// The /me endpoint answers errno 111 until the access token is set, then
	// returns a body proving it saw the refreshed token.
	var mu sync.Mutex
	gotAccess := ""
	creds := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/renew":
			creds = true
			io.WriteString(w, `{"refresh_token":"rt2","access_token":"at2"}`)
		case "/api/quota":
			if r.URL.Query().Get("access_token") == "" {
				io.WriteString(w, `{"errno":111}`)
				return
			}
			gotAccess = r.URL.Query().Get("access_token")
			io.WriteString(w, `{"errno":0,"total":10,"used":2}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client := api.NewClient(context.Background(), srv.Client(), srv.Client(), srv.URL, api.Config{
		RefreshToken: "rt1",
		UseOnlineAPI: true,
		APIAddress:   srv.URL + "/renew",
	})
	persisted := ""
	client.SetTokenPersister(func(rt string) { persisted = rt })

	total, used, err := client.Quota(context.Background())
	if err != nil {
		t.Fatalf("Quota after refresh failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, creds, "the online API must be asked for a fresh token")
	assert.Equal(t, "at2", client.AccessToken(), "the access token must be minted from the refreshed pair")
	assert.Equal(t, "rt2", client.RefreshToken(), "the rotated refresh token must be recorded")
	assert.Equal(t, "rt2", persisted, "the rotated refresh token must be persisted")
	assert.Equal(t, "at2", gotAccess, "the retried request must carry the fresh access token")
	assert.Equal(t, int64(10), total)
	assert.Equal(t, int64(2), used)
}

// TestRefreshTokenValidationPins that a misconfigured refresh mode fails with
// an explicit message instead of falling through to the other path: online
// without api_url_address, and OAuth without client credentials.
func TestRefreshTokenValidationPins(t *testing.T) {
	// The API answers errno 111, forcing a token refresh that then surfaces
	// the configuration error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"errno":111}`)
	}))
	t.Cleanup(srv.Close)

	client := api.NewClient(context.Background(), srv.Client(), srv.Client(), srv.URL, api.Config{
		RefreshToken: "rt1",
		UseOnlineAPI: true,
	})
	if _, _, err := client.Quota(context.Background()); err == nil {
		t.Fatal("expected an error for online refresh without api_url_address")
	} else if !strings.Contains(err.Error(), "api_url_address") {
		t.Fatalf("expected an explicit api_url_address error, got: %v", err)
	}

	oauth := api.NewClient(context.Background(), srv.Client(), srv.Client(), srv.URL, api.Config{
		RefreshToken: "rt1",
		UseOnlineAPI: false,
	})
	if _, _, err := oauth.Quota(context.Background()); err == nil {
		t.Fatal("expected an error for OAuth refresh without client_id/client_secret")
	} else if !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("expected an explicit client_id error, got: %v", err)
	}
}

// TestMkdirPins directory creation through method=create and the listing that
// follows: mkdir both creates the dir and makes it visible.
func TestMkdirPins(t *testing.T) {
	_, f, _ := newTestAPI(t, []byte("0123456789"), "official")

	require.NoError(t, f.Mkdir(context.Background(), "newdir"))

	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	remotes := make([]string, 0)
	for _, e := range entries {
		remotes = append(remotes, e.Remote())
	}
	assert.Contains(t, remotes, "newdir", "a freshly created dir must be listed")
}

// TestAboutPins quota surfaced as fs.Usage.
func TestAboutPins(t *testing.T) {
	_, f, _ := newTestAPI(t, []byte("0123456789"), "official")

	usage, err := f.About(context.Background())
	require.NoError(t, err)
	require.NotNil(t, usage.Total)
	assert.Equal(t, int64(1000), *usage.Total)
	require.NotNil(t, usage.Used)
	assert.Equal(t, int64(200), *usage.Used)
}

// TestNewFsRefreshValidationPins the conditional refresh configuration: online
// mode without api_url_address and OAuth mode without client credentials both
// fail at NewFs time with an explicit message.
func TestNewFsRefreshValidationPins(t *testing.T) {
	base := map[string]string{"refresh_token": "rt"}

	for _, tc := range []struct {
		name, want string
		cfg        map[string]string
	}{
		{
			name: "online without address",
			want: "use_online_api is true but the api_url_address option is empty",
			cfg:  map[string]string{"use_online_api": "true"},
		},
		{
			name: "oauth without credentials",
			want: "use_online_api is false but the client_id/client_secret options are empty",
			cfg:  base,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := configmap.Simple{}
			for k, v := range base {
				m[k] = v
			}
			for k, v := range tc.cfg {
				m[k] = v
			}
			_, err := NewFs(context.Background(), "baidunetdisk", "", m)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// testSrc is an fs.ObjectInfo with optional md5 support for the upload tests.
type testSrc struct {
	remote string
	data   []byte
	md5hex string // "" means the source cannot provide an md5
}

func (s *testSrc) Remote() string                        { return s.remote }
func (s *testSrc) ModTime(ctx context.Context) time.Time { return time.Unix(1700000000, 0) }
func (s *testSrc) Size() int64                           { return int64(len(s.data)) }
func (s *testSrc) Fs() fs.Info                           { return nil }
func (s *testSrc) String() string                        { return s.remote }
func (s *testSrc) Storable() bool                        { return true }
func (s *testSrc) Hash(ctx context.Context, ty fshash.Type) (string, error) {
	if ty == fshash.MD5 && s.md5hex != "" {
		return s.md5hex, nil
	}
	return "", fshash.ErrUnsupported
}

func md5hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func uploadCalls(a *testAPI) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.calls, " ")
}

// TestPutRapidHit pins the 秒传 path: when the server already has the content
// md5, Put creates the entry with a single rapid create and never touches
// precreate or the slice endpoint.
func TestPutRapidHit(t *testing.T) {
	data := []byte("rapid-upload-bytes-0123456789")
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	a.mu.Lock()
	if a.rapid == nil {
		a.rapid = map[string]bool{}
	}
	a.rapid[md5hex(data)] = true
	a.mu.Unlock()

	src := &testSrc{remote: "rapid.bin", data: data, md5hex: md5hex(data)}
	o, err := f.Put(context.Background(), io.NopCloser(bytes.NewReader(data)), src)
	require.NoError(t, err)
	assert.Equal(t, "rapid.bin", o.Remote())
	assert.Equal(t, int64(len(data)), o.Size())

	calls := uploadCalls(a)
	assert.Contains(t, calls, "method=create")
	assert.NotContains(t, calls, "method=precreate", "rapid hit must skip precreate")
	assert.NotContains(t, calls, "superfile2", "rapid hit must upload no slice")
}

// TestPutFullUpload pins the miss path: rapid create misses, precreate runs
// once with the slice md5, the single slice uploads with partseq 0, and the
// final create records the file.
func TestPutFullUpload(t *testing.T) {
	data := []byte("full-upload-bytes-0123456789abcdef")
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")

	src := &testSrc{remote: "full.bin", data: data, md5hex: md5hex(data)}
	o, err := f.Put(context.Background(), io.NopCloser(bytes.NewReader(data)), src)
	require.NoError(t, err)
	assert.Equal(t, "full.bin", o.Remote())
	assert.Equal(t, int64(len(data)), o.Size())

	a.mu.Lock()
	precreates := a.precreates
	slices := append([]string(nil), a.superfiles...)
	a.mu.Unlock()
	assert.Equal(t, 1, precreates, "one precreate for a fresh upload")
	require.Len(t, slices, 1, "one slice for a sub-slice-size file")
	parts := strings.Split(slices[0], ":")
	require.Len(t, parts, 3)
	assert.Equal(t, "up-1", parts[0])
	assert.Equal(t, "0", parts[1], "the single slice uploads with partseq 0")
	n, err := strconv.Atoi(parts[2])
	require.NoError(t, err)
	assert.Greater(t, n, len(data), "the recorded body carries the multipart framing")
}

// TestPutUploadIDExpired pins the expired-uploadid restart: slices answered
// with the expired marker force a second precreate, and the upload completes
// against the fresh uploadid.
func TestPutUploadIDExpired(t *testing.T) {
	data := []byte("expired-uploadid-bytes")
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	a.mu.Lock()
	a.failUploadID = "up-1"
	a.mu.Unlock()

	src := &testSrc{remote: "exp.bin", data: data}
	o, err := f.Put(context.Background(), io.NopCloser(bytes.NewReader(data)), src)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), o.Size())

	a.mu.Lock()
	precreates := a.precreates
	slices := append([]string(nil), a.superfiles...)
	a.mu.Unlock()
	assert.Equal(t, 2, precreates, "an expired uploadid forces a second precreate")
	require.Len(t, slices, 2)
	assert.True(t, strings.HasPrefix(slices[0], "up-1:0:"), "first attempt hits the expired uploadid")
	assert.True(t, strings.HasPrefix(slices[1], "up-2:0:"), "retry continues on the fresh uploadid")
}

// TestOpenChunkWriterPrecheckDedupe pins that OpenChunkWriter runs the rapid
// pre-check before any chunk is read: a hit returns a deduped writer that the
// core skips entirely, and Close is instant.
func TestOpenChunkWriterPrecheckDedupe(t *testing.T) {
	data := []byte("chunk-dedupe-bytes")
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	a.mu.Lock()
	if a.rapid == nil {
		a.rapid = map[string]bool{}
	}
	a.rapid[md5hex(data)] = true
	a.mu.Unlock()

	src := &testSrc{remote: "dup.bin", data: data, md5hex: md5hex(data)}
	info, w, err := f.OpenChunkWriter(context.Background(), "dup.bin", src)
	require.NoError(t, err)
	assert.Equal(t, int64(4<<20), info.ChunkSize, "non-vip slice size drives the chunk size")
	cw, ok := w.(*chunkWriter)
	require.True(t, ok)
	assert.True(t, cw.AlreadyExists(), "a rapid hit must report the object as already existing")
	require.NoError(t, w.Close(context.Background()))

	calls := uploadCalls(a)
	assert.NotContains(t, calls, "method=precreate")
	assert.NotContains(t, calls, "superfile2")
}

// TestOpenChunkWriterMiss pins the miss path through the writer: the chunk is
// spooled, Close runs precreate → slices → create, and the entry is found.
func TestOpenChunkWriterMiss(t *testing.T) {
	data := []byte("chunk-miss-bytes-0123456789")
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")

	src := &testSrc{remote: "miss.bin", data: data, md5hex: md5hex(data)}
	_, w, err := f.OpenChunkWriter(context.Background(), "miss.bin", src)
	require.NoError(t, err)
	cw, ok := w.(*chunkWriter)
	require.True(t, ok)
	assert.False(t, cw.AlreadyExists())
	n, err := w.WriteChunk(context.Background(), 0, bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), n)
	require.NoError(t, w.Close(context.Background()))

	a.mu.Lock()
	slices := append([]string(nil), a.superfiles...)
	a.mu.Unlock()
	require.Len(t, slices, 1)
	assert.True(t, strings.HasPrefix(slices[0], "up-1:0:"))

	o, err := f.NewObject(context.Background(), "miss.bin")
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), o.Size())
}

// TestPutEmptyFile pins that zero-byte files are rejected: Baidu refuses
// empty files server-side.
func TestPutEmptyFile(t *testing.T) {
	_, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	src := &testSrc{remote: "empty.bin", data: []byte{}}
	_, err := f.Put(context.Background(), io.NopCloser(bytes.NewReader(nil)), src)
	require.Error(t, err)
	assert.ErrorIs(t, err, api.ErrBaiduEmptyFilesNotAllowed)
}

// TestSliceSizeTable pins the reference slice sizing: non-VIP is fixed at 4
// MiB, VIP tiers get their size, custom sizes apply to members only, and the
// low-bandwidth mode grows in 1 MiB steps.
func TestSliceSizeTable(t *testing.T) {
	const MiB = int64(1 << 20)
	for _, tc := range []struct {
		vip, custom int64
		size        int64
		low         bool
		want        int64
	}{
		{vip: 0, size: 100, want: 4 * MiB},
		{vip: 0, size: 100 << 30, custom: 8 * MiB, want: 4 * MiB},
		{vip: 1, size: 100, want: 16 * MiB},
		{vip: 2, size: 100, want: 32 * MiB},
		{vip: 2, size: 100, custom: 8 * MiB, want: 8 * MiB},
		{vip: 2, size: 100, custom: 2 * MiB, want: 4 * MiB},
		{vip: 1, size: 100, custom: 32 * MiB, want: 16 * MiB},
		{vip: 2, size: 6 * 1024 * MiB, low: true, want: 4 * MiB},
	} {
		got := baiduSliceSize(int(tc.vip), tc.size, tc.custom, tc.low)
		assert.Equal(t, tc.want, got, "vip=%d size=%d custom=%d low=%v", tc.vip, tc.size, tc.custom, tc.low)
	}
}

// TestVipTypeCached pins that the uinfo answer is fetched once: two slice
// sizings share the single lazy call.
func TestVipTypeCached(t *testing.T) {
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	_, err := f.sliceSize(context.Background(), 100)
	require.NoError(t, err)
	_, err = f.sliceSize(context.Background(), 200)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(uploadCalls(a), "/xpan/nas"), "uinfo must be fetched once")
}

// TestPutStream pins the unknown-size path: the stream is spooled, hashed and
// uploaded, and the entry is found.
func TestPutStream(t *testing.T) {
	data := []byte("putstream-bytes-0123456789")
	_, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	src := &testSrc{remote: "stream.bin", data: data}
	o, err := f.PutStream(context.Background(), io.NopCloser(bytes.NewReader(data)), src)
	require.NoError(t, err)
	assert.Equal(t, "stream.bin", o.Remote())
	assert.Equal(t, int64(len(data)), o.Size())
}

// TestUploadDynamicURL pins the locateupload path: with the dynamic lookup
// enabled the upload domain resolves through locateupload and the slices go
// to the resolved domain.
func TestUploadDynamicURL(t *testing.T) {
	data := []byte("dynamic-url-bytes")
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	f.opt.UseDynamicUploadAPI = true
	src := &testSrc{remote: "dyn.bin", data: data}
	o, err := f.Put(context.Background(), io.NopCloser(bytes.NewReader(data)), src)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), o.Size())
	a.mu.Lock()
	locates := a.locateCalls
	a.mu.Unlock()
	assert.Equal(t, 1, locates, "the dynamic lookup must resolve the upload domain")
	assert.Contains(t, uploadCalls(a), "locateupload")
}

// newLocalSrc builds a real local Fs over a temp dir holding data as name,
// returning the local object to use as an upload source.
func newLocalSrc(t *testing.T, name string, data []byte) fs.Object {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o666))
	lfs, err := local.NewFs(context.Background(), "testlocal", dir, configmap.Simple{})
	require.NoError(t, err)
	o, err := lfs.NewObject(context.Background(), name)
	require.NoError(t, err)
	return o
}

// noTempDir redirects every temp location os.CreateTemp consults at a
// nonexistent dir: any spool attempt fails loudly instead of landing on disk.
// Call it after every t.TempDir user (they need the real temp to exist).
func noTempDir(t *testing.T) {
	target := filepath.Join(t.TempDir(), "no-such-dir")
	t.Setenv("TMPDIR", target)
	t.Setenv("TEMP", target)
	t.Setenv("TMP", target)
}

// TestLocalSourceDetection pins the no-spool gate: a real local object is
// detected, anything else (here the hash-only mock) falls back to spooling.
func TestLocalSourceDetection(t *testing.T) {
	data := []byte("detection-bytes")
	o := newLocalSrc(t, "src.bin", data)
	obj, ok := localSource(o)
	require.True(t, ok, "a local backend object must take the no-spool path")
	assert.Equal(t, o.Remote(), obj.Remote())

	mock := &testSrc{remote: "mock.bin", data: data}
	_, ok = localSource(mock)
	assert.False(t, ok, "a non-local source must fall back to the spool path")
	_, ok = localSource(nil)
	assert.False(t, ok, "a nil source must fall back to the spool path")
}

// TestPutLocalNoSpool pins the local Put path over several slices: the stream
// is hashed while discarding it, the slices upload through range re-opens,
// and no temp file is created.
func TestPutLocalNoSpool(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 600*1024) // ~9.4 MiB, 3 slices
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	src := newLocalSrc(t, "big.bin", data)
	noTempDir(t)

	in, err := src.Open(context.Background())
	require.NoError(t, err)
	o, err := f.Put(context.Background(), in, src)
	require.NoError(t, err)
	assert.Equal(t, "big.bin", o.Remote())
	assert.Equal(t, int64(len(data)), o.Size())

	a.mu.Lock()
	slices := append([]string(nil), a.superfiles...)
	precreates := a.precreates
	reassembled := append([]byte(nil), a.sliceData["up-1:0"]...)
	reassembled = append(reassembled, a.sliceData["up-1:1"]...)
	reassembled = append(reassembled, a.sliceData["up-1:2"]...)
	a.mu.Unlock()
	assert.Equal(t, 1, precreates)
	require.Len(t, slices, 3, "one slice upload per 4 MiB slice")
	seqs := map[string]bool{}
	for _, s := range slices {
		seqs[s[strings.Index(s, ":")+1:strings.LastIndex(s, ":")]] = true
	}
	assert.Equal(t, map[string]bool{"0": true, "1": true, "2": true}, seqs, "every partseq uploads exactly once, in any order")
	assert.Equal(t, data, reassembled, "the concurrently uploaded slices must reassemble the source byte-for-byte")
}

// TestPutLocalDedupe pins that a local duplicate still short-circuits through
// the rapid pre-check without reading the stream for upload.
func TestPutLocalDedupe(t *testing.T) {
	data := []byte("local-dedupe-bytes")
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	a.mu.Lock()
	if a.rapid == nil {
		a.rapid = map[string]bool{}
	}
	a.rapid[md5hex(data)] = true
	a.mu.Unlock()
	src := newLocalSrc(t, "dup.bin", data)
	noTempDir(t)

	in, err := src.Open(context.Background())
	require.NoError(t, err)
	o, err := f.Put(context.Background(), in, src)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), o.Size())

	calls := uploadCalls(a)
	assert.NotContains(t, calls, "method=precreate")
	assert.NotContains(t, calls, "superfile2")
}

// TestOpenChunkWriterLocal pins the local chunk-writer path: chunks are
// hashed and discarded (no spool), Close uploads through range re-opens.
func TestOpenChunkWriterLocal(t *testing.T) {
	data := []byte("local-chunk-bytes-0123456789")
	a, f, _ := newTestAPI(t, []byte("0123456789"), "official")
	src := newLocalSrc(t, "wc.bin", data)
	noTempDir(t)

	_, w, err := f.OpenChunkWriter(context.Background(), "wc.bin", src)
	require.NoError(t, err)
	cw, ok := w.(*chunkWriter)
	require.True(t, ok)
	assert.NotNil(t, cw.local, "a local source must arm the no-spool writer")
	assert.Nil(t, cw.tmp, "a local source must not create a spool file")
	n, err := w.WriteChunk(context.Background(), 0, bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), n)
	require.NoError(t, w.Close(context.Background()))

	a.mu.Lock()
	slices := append([]string(nil), a.superfiles...)
	a.mu.Unlock()
	require.Len(t, slices, 1)
	assert.True(t, strings.HasPrefix(slices[0], "up-1:0:"))
}

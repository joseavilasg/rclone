// Package quark provides a native interface to Quark drive
// (drive.quark.cn), the Chinese consumer storage.
//
// It is a faithful port of the OpenList quark_uc driver. The upload flow
// streams parts straight to the Alibaba OSS endpoint obtained from the
// pre-upload call, so rclone progress reflects the real upload instead of
// ending at 100% while a webdav layer buffers the whole file.
package quark

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/quark/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/encoder"
)

const (
	apiURL  = "https://drive.quark.cn/1/clouddrive"
	referer = "https://pan.quark.cn/"
	pr      = "ucpro"

	// downloadSegmentSize is the length of each ranged GET issued against one
	// signed download URL. quark's CDN throttles long single-stream transfers
	// to roughly 100 KiB/s, while short ranges are served at full speed once
	// the signed URL has been exercised a few times. Sequential small segments
	// over one URL mirror OpenList's Downloader (Concurrency 3, PartSize 10MB)
	// and sustain multi-MiB/s throughput.
	downloadSegmentSize = 10 * 1024 * 1024
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "quark",
		Description: "Quark Drive",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:      "cookie",
			Help:      "The cookie used to log in, from your Quark account. It is rotated by rclone automatically when the API returns a new value.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:    "root_folder_id",
			Help:    "ID of the root folder. Blank means the default \"My Drive\" folder id 0.",
			Default: "0",
		}, {
			Name:    "encoding",
			Help:    "The encoding for the backend. See the encoding section of the docs for more info.",
			Default: (encoder.Display | encoder.EncodeInvalidUtf8),
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	Cookie       string               `config:"cookie"`
	RootFolderID string               `config:"root_folder_id"`
	Enc          encoder.MultiEncoder `config:"encoding"`
}

// Fs represents a remote Quark drive
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	m        configmap.Mapper
	client   *api.Client

	mu     sync.Mutex
	dirIDs map[string]string // remote dir path -> fid
}

// NewFs constructs an Fs from the path, bucket:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if opt.Cookie == "" {
		return nil, fmt.Errorf("quark: the cookie option is required")
	}
	root = strings.Trim(root, "/")
	// Two http clients mirror OpenList's quark_uc setup. API and OSS upload
	// traffic may negotiate HTTP/2 (OpenList uses resty's default transport,
	// ForceAttemptHTTP2 on), but ranged CDN downloads must run HTTP/1.1:
	// quark's edge resets long HTTP/2 streams on the download hosts and Go
	// would multiplex concurrent streams onto one H2 connection, collapsing
	// them into a single throttle bucket (~1.4 MiB/s flat). OpenList's
	// downloader is a bare &http.Transport{} (HTTP/1.1-only) for the same
	// reason.
	httpClient := fshttp.NewClient(ctx)
	downloadClient := fshttp.NewClientCustom(ctx, func(t *http.Transport) {
		t.ForceAttemptHTTP2 = false
	})
	// fshttp stamps every request with "rclone/" (fs/fshttp/http.go RoundTrip
	// "Force user agent"), so quark's per-request User-Agent would be wiped.
	// A request filter is the one hook that runs after that stamping and lets
	// us send the quark PC client UA, which the API requires: /file/download
	// refuses large files with code 23018 unless the request carries it.
	installQuarkUA := func(hc *http.Client) {
		if tr, ok := hc.Transport.(*fshttp.Transport); ok {
			tr.SetRequestFilter(func(req *http.Request) {
				req.Header.Set("User-Agent", api.UserAgent)
			})
		}
	}
	installQuarkUA(httpClient)
	installQuarkUA(downloadClient)
	f := &Fs{
		name:   name,
		root:   root,
		opt:    *opt,
		m:      m,
		client: api.NewClient(ctx, httpClient, downloadClient, apiURL, referer, pr, opt.Cookie),
		dirIDs: map[string]string{},
	}
	f.client.SetCookiePersister(func() {
		newCookie := f.client.Cookie()
		if newCookie == f.opt.Cookie {
			return
		}
		f.m.Set("cookie", newCookie)
		config.SaveConfig()
	})
	f.features = (&fs.Features{
		ReadMimeType:            true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	// Resolve the root path (anchored at the drive root) to distinguish a
	// directory from a file. A root that can't be found is NOT an error:
	// rclone needs NewFs to succeed so it can create it on demand with
	// mkdir/copy; List reports "directory not found" instead. Only the file
	// case is returned as ErrorIsFile with the Fs pointing at the parent.
	if root != "" {
		switch _, err := f.resolvePath(ctx, root); err {
		case nil:
			// root is an existing directory - nothing to do
		case fs.ErrorDirNotFound:
			parent, leaf := splitRemote(root)
			if _, err2 := f.resolvePath(ctx, parent); err2 != nil {
				// the parent itself doesn't exist yet: leave the root
				// untouched so rclone creates the whole path lazily
				return f, nil
			}
			f.root = parent
			if leaf != "" {
				if _, err3 := f.newObject(ctx, leaf); err3 == nil {
					return f, fs.ErrorIsFile
				}
			}
			f.root = root
		default:
			return nil, err
		}
	}
	return f, nil
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("Quark drive root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Precision returns the precision of this remote
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Hashes returns the supported hash set
func (f *Fs) Hashes() hash.Set {
	return hash.NewHashSet()
}

// splitRemote returns the parent dir and leaf name of a remote path
func splitRemote(remote string) (dir, leaf string) {
	remote = strings.Trim(remote, "/")
	slash := strings.LastIndex(remote, "/")
	if slash == -1 {
		return "", remote
	}
	return remote[:slash], remote[slash+1:]
}

// purgeDirs drops the resolved directory fid cache after a tree mutation
func (f *Fs) purgeDirs() {
	f.mu.Lock()
	f.dirIDs = map[string]string{}
	f.mu.Unlock()
}

// dirKey returns the drive-root-anchored path for a dir relative to the
// root of this Fs. "" (the empty remote path) is the root of this Fs.
func (f *Fs) dirKey(dir string) string {
	return strings.Trim(path.Join(strings.Trim(f.root, "/"), strings.Trim(dir, "/")), "/")
}

// resolveDir returns the fid for a remote dir path ("" = the root of this
// Fs) relative to the root of this Fs. Nothing is created.
func (f *Fs) resolveDir(ctx context.Context, dir string) (string, error) {
	return f.resolvePath(ctx, f.dirKey(dir))
}

// resolveDirCreate returns the fid for a remote dir path, creating any
// missing ancestors first so uploads can target not-yet-existing trees.
func (f *Fs) resolveDirCreate(ctx context.Context, dir string) (string, error) {
	id, err := f.resolveDir(ctx, dir)
	if err == nil {
		return id, nil
	}
	if err != fs.ErrorDirNotFound {
		return "", err
	}
	if err := f.Mkdir(ctx, dir); err != nil {
		return "", err
	}
	return f.resolveDir(ctx, dir)
}

// resolvePath returns the fid for a dir path anchored at the drive root
// ("" = root folder). Resulting ids are cached. Nothing is created.
func (f *Fs) resolvePath(ctx context.Context, full string) (string, error) {
	full = strings.Trim(full, "/")
	f.mu.Lock()
	if id, ok := f.dirIDs[full]; ok {
		f.mu.Unlock()
		return id, nil
	}
	f.mu.Unlock()
	if full == "" {
		id := f.opt.RootFolderID
		f.mu.Lock()
		f.dirIDs[""] = id
		f.mu.Unlock()
		return id, nil
	}
	parent, leaf := splitRemote(full)
	parentID, err := f.resolvePath(ctx, parent)
	if err != nil {
		return "", err
	}
	id, err := f.findDirChild(ctx, parentID, f.opt.Enc.FromStandardName(leaf))
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.dirIDs[full] = id
	f.mu.Unlock()
	return id, nil
}

// findDirChild returns the fid of the directory entry named name inside
// parentID, or fs.ErrorDirNotFound
func (f *Fs) findDirChild(ctx context.Context, parentID, name string) (string, error) {
	files, err := f.client.GetFiles(ctx, parentID)
	if err != nil {
		return "", err
	}
	for i := range files {
		if !files[i].File && files[i].FileName == name {
			return files[i].Fid, nil
		}
	}
	return "", fs.ErrorDirNotFound
}

// List the objects and directories in dir into entries
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	id, err := f.resolveDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	files, err := f.client.GetFiles(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("quark: list %q: %w", dir, err)
	}
	base := strings.Trim(dir, "/")
	for i := range files {
		file := &files[i]
		name := f.opt.Enc.ToStandardName(file.FileName)
		remote := name
		if base != "" {
			remote = base + "/" + name
		}
		if file.File {
			entries = append(entries, newObject(f, remote, *file))
		} else {
			entries = append(entries, fs.NewDir(remote, time.UnixMilli(file.UpdatedAt)))
		}
	}
	return entries, nil
}

// newObject returns an Object for remote or fs.ErrorObjectNotFound
func (f *Fs) newObject(ctx context.Context, remote string) (*Object, error) {
	dir, leaf := splitRemote(remote)
	if leaf == "" {
		return nil, fs.ErrorObjectNotFound
	}
	parentID, err := f.resolveDir(ctx, dir)
	if err != nil {
		// A missing parent means the object cannot exist either. Report
		// object-not-found so rclone's copy flow proceeds and creates the
		// directory on demand; resolveDirCreate handles it at upload time.
		return nil, fs.ErrorObjectNotFound
	}
	files, err := f.client.GetFiles(ctx, parentID)
	if err != nil {
		return nil, err
	}
	name := f.opt.Enc.FromStandardName(leaf)
	for i := range files {
		file := &files[i]
		if file.FileName != name {
			continue
		}
		if file.File {
			return newObject(f, remote, *file), nil
		}
		return nil, fs.ErrorIsDir
	}
	return nil, fs.ErrorObjectNotFound
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	return f.newObject(ctx, remote)
}

// Mkdir creates the directory if it doesn't exist, creating parents as
// needed. dir is relative to the root of this Fs; "" means the root itself.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	full := strings.Trim(path.Join(strings.Trim(f.root, "/"), strings.Trim(dir, "/")), "/")
	if full == "" {
		return nil
	}
	parts := strings.Split(full, "/")
	cur := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		next := strings.Trim(strings.Join([]string{cur, part}, "/"), "/")
		if _, err := f.resolvePath(ctx, next); err == nil {
			cur = next
			continue
		} else if err != fs.ErrorDirNotFound {
			return err
		}
		parentID, err := f.resolvePath(ctx, cur)
		if err != nil {
			return err
		}
		fid, err := f.client.MakeDir(ctx, part, parentID)
		if err != nil {
			return fmt.Errorf("quark: mkdir %q: %w", next, err)
		}
		f.mu.Lock()
		f.dirIDs[next] = fid
		f.mu.Unlock()
		// ported throttle from OpenList
		time.Sleep(time.Second)
		cur = next
	}
	return nil
}

// Rmdir removes the directory if it is empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	id, err := f.resolveDir(ctx, dir)
	if err != nil {
		return err
	}
	if err := f.client.Delete(ctx, id); err != nil {
		return fmt.Errorf("quark: rmdir %q: %w", dir, err)
	}
	f.purgeDirs()
	return nil
}

// Purge removes the directory dir and all its contents. Quark deletes a
// folder (and everything under it) server-side in a single call.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	id, err := f.resolveDir(ctx, dir)
	if err != nil {
		return err
	}
	if err := f.client.Delete(ctx, id); err != nil {
		return fmt.Errorf("quark: purge %q: %w", dir, err)
	}
	f.purgeDirs()
	return nil
}

// DirCacheFlush resets the directory cache - used in testing
func (f *Fs) DirCacheFlush() {
	f.purgeDirs()
}

// Move moves src to this remote, or within this remote
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	o, ok := src.(*Object)
	if !ok {
		return nil, fmt.Errorf("quark: can't move non-quark object %T", src)
	}
	srcDir, srcLeaf := splitRemote(o.remote)
	dstDir, dstLeaf := splitRemote(remote)
	leaf := f.opt.Enc.FromStandardName(dstLeaf)
	if srcDir != dstDir || srcLeaf != dstLeaf {
		if srcLeaf != dstLeaf {
			if err := f.client.Rename(ctx, o.info.Fid, leaf); err != nil {
				return nil, fmt.Errorf("quark: rename %q: %w", src, err)
			}
		}
		if srcDir != dstDir {
			dstDirID, err := f.resolveDir(ctx, dstDir)
			if err != nil {
				return nil, err
			}
			if err := f.client.Move(ctx, o.info.Fid, dstDirID); err != nil {
				return nil, fmt.Errorf("quark: move %q: %w", src, err)
			}
		}
	}
	f.purgeDirs()
	oo := *o
	oo.remote = remote
	return &oo, nil
}

// DirMove moves a directory
func (f *Fs) DirMove(ctx context.Context, srcFs fs.Fs, srcRemote, dstRemote string) error {
	if srcFs != f {
		return fs.ErrorCantDirMove
	}
	srcID, err := f.resolveDir(ctx, srcRemote)
	if err != nil {
		return err
	}
	srcDir, _ := splitRemote(srcRemote)
	dstDir, dstLeaf := splitRemote(dstRemote)
	leaf := f.opt.Enc.FromStandardName(dstLeaf)
	if err := f.client.Rename(ctx, srcID, leaf); err != nil {
		return fmt.Errorf("quark: dir rename %q: %w", srcRemote, err)
	}
	if srcDir != dstDir {
		dstDirID, err := f.resolveDir(ctx, dstDir)
		if err != nil {
			return err
		}
		if err := f.client.Move(ctx, srcID, dstDirID); err != nil {
			return fmt.Errorf("quark: dir move %q: %w", srcRemote, err)
		}
	}
	f.purgeDirs()
	return nil
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	m, err := f.client.Member(ctx)
	if err != nil {
		return nil, err
	}
	return &fs.Usage{
		Total: fs.NewUsageValue(m.Data.TotalCapacity),
		Used:  fs.NewUsageValue(m.Data.UseCapacity),
	}, nil
}

// Object describes a file
type Object struct {
	fs     *Fs
	remote string
	info   api.File
}

func newObject(f *Fs, remote string, info api.File) *Object {
	return &Object{fs: f, remote: remote, info: info}
}

// Fs returns read only access to the Fs that this object is part of
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// String returns a description of the Object
func (o *Object) String() string {
	return o.remote
}

// ModTime returns the time the object was modified
//
// This is the server assigned updated_at. Quark has no endpoint to set the
// modification time, which is why the backend reports ModTimeNotSupported
// precision: this value is informational only and never used for decisions.
func (o *Object) ModTime(ctx context.Context) time.Time {
	return time.UnixMilli(o.info.UpdatedAt)
}

// Size returns the size of the object
func (o *Object) Size() int64 {
	return o.info.Size
}

// Storable says whether this object can be stored
func (o *Object) Storable() bool {
	return true
}

// SetModTime sets the metadata on the object to set the modification date
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	return fs.ErrorCantSetModTime
}

// Hash returns the SHA-, MD5- or MurmurHash3 sum of a file
func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// ID returns the ID of the Object if known, or "" if it does not exist
func (o *Object) ID(ctx context.Context) (string, error) {
	return o.info.Fid, nil
}

// MimeType returns the MIME type of an object
func (o *Object) MimeType(ctx context.Context) string {
	return mime.TypeByExtension(path.Ext(o.remote))
}

// Remove this object
func (o *Object) Remove(ctx context.Context) error {
	if err := o.fs.client.Delete(ctx, o.info.Fid); err != nil {
		return fmt.Errorf("quark: delete %q: %w", o.remote, err)
	}
	o.fs.purgeDirs()
	return nil
}

// Update uploads new content for an existing object. Quark cannot modify a
// file in place, so the new version is uploaded first and the old entry is
// removed afterwards.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	size := src.Size()
	if size < 0 {
		return fmt.Errorf("quark: cannot update object %q with unknown size", o.remote)
	}
	oldFid := o.info.Fid
	if err := o.fs.uploadStream(ctx, o.remote, size, fs.MimeType(ctx, src), in); err != nil {
		return err
	}
	if err := o.fs.client.Delete(ctx, oldFid); err != nil {
		return fmt.Errorf("quark: delete old version of %q: %w", o.remote, err)
	}
	o.fs.purgeDirs()
	no, err := o.fs.newObject(ctx, o.remote)
	if err == nil {
		*o = *no
	}
	return err
}

// Open opens the file for read. The requested span is served as a chain of
// small ranged GETs (see downloadSegmentSize) over one signed URL cached per
// fid, so the CDN never throttles the transfer to its cold rate.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	start := int64(0)
	end := o.info.Size - 1
	for _, option := range options {
		switch x := option.(type) {
		case *fs.RangeOption:
			start = x.Start
			if x.End >= 0 {
				end = x.End
			}
		case *fs.SeekOption:
			start = x.Offset
		}
	}
	signedURL, err := o.fs.client.DownloadURL(ctx, o.info.Fid)
	if err != nil {
		return nil, err
	}
	return &segmentDownload{ctx: ctx, o: o, url: signedURL, offset: start, end: end}, nil
}

// segmentDownload serves the byte span [offset, end] of a quark object by
// chaining small ranged GETs over a single signed download URL. Keeping one
// URL per file lets it warm up: quark's CDN serves short ranges at full speed
// once a signature has been used a few times, instead of the ~100 KiB/s rate
// it enforces on cold long-lived transfers.
type segmentDownload struct {
	ctx      context.Context
	o        *Object
	url      string // signed download URL (replaced when re-signed after a 4xx)
	offset   int64  // absolute offset of the next byte to serve
	end      int64  // absolute end of the requested span (inclusive)
	body     io.ReadCloser
	segStart int64 // absolute start of the current segment
	segEnd   int64 // absolute end of the current segment
}

// Read serves the next bytes, starting the next segment when the current
// ranged response is exhausted.
func (r *segmentDownload) Read(p []byte) (int, error) {
	for {
		if r.body == nil {
			if r.offset > r.end {
				return 0, io.EOF
			}
			r.segStart = r.offset
			r.segEnd = min(r.offset+downloadSegmentSize-1, r.end, r.o.info.Size-1)
			res, url, err := r.o.downloadSegment(r.ctx, r.url, r.segStart, r.segEnd)
			if err != nil {
				return 0, err
			}
			r.body = res.Body
			r.url = url
		}
		n, err := r.body.Read(p)
		if n > 0 {
			r.offset += int64(n)
			if err == io.EOF {
				err = nil
			}
			return n, err
		}
		_ = r.body.Close()
		r.body = nil
		if err != io.EOF {
			return 0, err
		}
		if r.offset <= r.segEnd {
			return 0, fmt.Errorf("quark: download segment %d-%d truncated at byte %d", r.segStart, r.segEnd, r.offset)
		}
	}
}

// Close aborts the active segment response.
func (r *segmentDownload) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

// downloadSegment issues one ranged GET over a signed download URL. A 4xx
// answer (for example a stale or cold signature) drops the cached URL, signs a
// fresh one and retries the segment once; any other status that is not 206
// means the server ignored the Range and is reported as fs.ErrorRangeIgnored.
func (o *Object) downloadSegment(ctx context.Context, url string, start, end int64) (*http.Response, string, error) {
	headers := make(http.Header)
	headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	for attempt := 0; ; attempt++ {
		res, err := o.fs.client.Download(ctx, url, headers)
		if err != nil {
			return nil, "", err
		}
		if res.StatusCode != http.StatusPartialContent {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode >= 400 && res.StatusCode < 500 && attempt == 0 {
				o.fs.client.DropDownloadURL(o.info.Fid)
				fresh, err := o.fs.client.DownloadURL(ctx, o.info.Fid)
				if err != nil {
					return nil, "", err
				}
				url = fresh
				continue
			}
			return nil, "", fs.ErrorRangeIgnored
		}
		return res, url, nil
	}
}

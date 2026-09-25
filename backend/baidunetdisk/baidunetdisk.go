// Package baidunetdisk provides access to Baidu Netdisk
// (pan.baidu.com), the Chinese consumer storage.
//
// It is a faithful port of the OpenList baidu_netdisk driver. The API is
// path-based (absolute paths anchored at the drive root "/") rather than
// ID-based, download links resolve through one of three mechanisms (official,
// crack, crack_video), and the access token is minted from the refresh token
// through an online proxy by default or the direct OAuth endpoint.
package baidunetdisk

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/baidunetdisk/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/encoder"
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "baidunetdisk",
		Description: "Baidu Netdisk",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:      "refresh_token",
			Help:      "The refresh token used to mint access tokens. See the OpenList wiki for how to obtain one.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:    "download_api",
			Help:    "Which download API to use to resolve download URLs.",
			Default: "official",
			Examples: []fs.OptionExample{{
				Value: "official",
				Help:  "official: multimedia dlink with access_token, chases the CDN redirect (needs an active account)",
			}, {
				Value: "crack",
				Help:  "crack: /api/filemetas dlna dlink, works on non-active accounts",
			}, {
				Value: "crack_video",
				Help:  "crack_video: /api/mediainfo dlink, best for video files",
			}},
		}, {
			Name:    "use_online_api",
			Help:    "Whether to refresh the access token through the online api_url_address endpoint (true) or through the direct OAuth flow with client_id/client_secret (false). When true, api_url_address is required; when false, client_id and client_secret are required.",
			Default: false,
		}, {
			Name: "api_url_address",
			Help: "Online API address used when use_online_api is true. This is a token refresh proxy provided by the OpenList project; supply your own if you use this mode.",
		}, {
			Name: "client_id",
			Help: "Baidu OAuth client_id, required when use_online_api is false.",
		}, {
			Name: "client_secret",
			Help: "Baidu OAuth client_secret, required when use_online_api is false.",
		}, {
			Name:    "custom_crack_ua",
			Help:    "User-Agent used for the crack and crack_video download APIs.",
			Default: "netdisk",
		}, {
			Name:    "upload_api",
			Help:    "Upload API domain base. Used as fallback when use_dynamic_upload_api is true and the dynamic lookup fails.",
			Default: "https://d.pcs.baidu.com",
		}, {
			Name:    "use_dynamic_upload_api",
			Help:    "Dynamically fetch the upload domain via locateupload before uploading. The upload_api setting is used as fallback when the lookup fails.",
			Default: true,
		}, {
			Name:    "upload_thread",
			Help:    "Number of parallel slices uploaded at once (1-32).",
			Default: 3,
		}, {
			Name:    "upload_timeout",
			Help:    "Per-slice upload timeout in seconds.",
			Default: 60,
		}, {
			Name:     "custom_upload_part_size",
			Help:     "Custom upload slice size in bytes, 0 for automatic. Only applies to VIP accounts.",
			Default:  0,
			Advanced: true,
		}, {
			Name:     "low_bandwith_upload_mode",
			Help:     "Grow the slice size in 1 MiB steps until the slice count fits MaxSliceNum (for low bandwidth).",
			Default:  false,
			Advanced: true,
		}, {
			Name:     "encoding",
			Help:     "The encoding for the backend. See the encoding section of the docs for more info.",
			Advanced: true,
			Default:  (encoder.Display | encoder.EncodeInvalidUtf8),
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	RefreshToken          string               `config:"refresh_token"`
	DownloadAPI           string               `config:"download_api"`
	UseOnlineAPI          bool                 `config:"use_online_api"`
	APIAddress            string               `config:"api_url_address"`
	ClientID              string               `config:"client_id"`
	ClientSecret          string               `config:"client_secret"`
	CustomCrackUA         string               `config:"custom_crack_ua"`
	UploadAPI             string               `config:"upload_api"`
	UseDynamicUploadAPI   bool                 `config:"use_dynamic_upload_api"`
	UploadThread          int                  `config:"upload_thread"`
	UploadTimeout         int                  `config:"upload_timeout"`
	CustomUploadPartSize  int64                `config:"custom_upload_part_size"`
	LowBandwithUploadMode bool                 `config:"low_bandwith_upload_mode"`
	Enc                   encoder.MultiEncoder `config:"encoding"`
}

// Fs represents a remote Baidu Netdisk
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	m        configmap.Mapper
	client   *api.Client
}

// NewFs constructs an Fs from the path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if err := validateRefreshOptions(opt); err != nil {
		return nil, err
	}
	root = strings.Trim(root, "/")
	// Two http clients mirror OpenList's setup. API traffic (token minting,
	// listing, filemanager, dlink resolution) uses the default client. CDN
	// downloads use a dedicated client stamped with the User-Agent that each
	// download API expects (pan.baidu.com for official, the custom crack UA
	// otherwise). fshttp stamps every request with "rclone/" after the
	// headers are built, so a request filter is the hook that forces the
	// download UA.
	httpClient := fshttp.NewClient(ctx)
	downloadClient := newDownloadClient(ctx, opt)
	f := &Fs{
		name: name,
		root: root,
		opt:  *opt,
		m:    m,
		client: api.NewClient(ctx, httpClient, downloadClient, "", api.Config{
			RefreshToken:       opt.RefreshToken,
			UseOnlineAPI:       opt.UseOnlineAPI,
			APIAddress:         opt.APIAddress,
			ClientID:           opt.ClientID,
			ClientSecret:       opt.ClientSecret,
			DownloadAPI:        opt.DownloadAPI,
			CustomCrackUA:      opt.CustomCrackUA,
			UploadSliceTimeout: opt.UploadTimeout,
		}),
	}
	f.client.SetTokenPersister(func(newRefreshToken string) {
		if newRefreshToken == "" || newRefreshToken == f.opt.RefreshToken {
			return
		}
		f.m.Set("refresh_token", newRefreshToken)
		config.SaveConfig()
	})
	f.features = (&fs.Features{
		ReadMimeType:            true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	if root == "" {
		return f, nil
	}
	return f.resolveRoot(ctx, root)
}

// validateRefreshOptions enforces the conditional refresh configuration:
// refresh_token is always required, and use_online_api selects exactly one of
// the online proxy address or the OAuth client credentials.
func validateRefreshOptions(opt *Options) error {
	if opt.RefreshToken == "" {
		return fmt.Errorf("baidunetdisk: the refresh_token option is required")
	}
	switch {
	case opt.UseOnlineAPI && strings.TrimSpace(opt.APIAddress) == "":
		return fmt.Errorf("baidunetdisk: use_online_api is true but the api_url_address option is empty")
	case !opt.UseOnlineAPI && (opt.ClientID == "" || opt.ClientSecret == ""):
		return fmt.Errorf("baidunetdisk: use_online_api is false but the client_id/client_secret options are empty")
	}
	return nil
}

// newDownloadClient builds the CDN download client stamped with the
// User-Agent each download API expects.
func newDownloadClient(ctx context.Context, opt *Options) *http.Client {
	downloadClient := fshttp.NewClient(ctx)
	downloadUA := "pan.baidu.com"
	if opt.DownloadAPI != "official" {
		downloadUA = opt.CustomCrackUA
	}
	if tr, ok := downloadClient.Transport.(*fshttp.Transport); ok {
		customUA := downloadUA
		tr.SetRequestFilter(func(req *http.Request) {
			req.Header.Set("User-Agent", customUA)
		})
	}
	return downloadClient
}

// resolveRoot distinguishes a directory root from a file root. A root that
// can't be found is NOT an error: rclone needs NewFs to succeed so it can
// create it on demand with mkdir/copy; List reports "directory not found"
// instead. Only the file case is returned as ErrorIsFile with the Fs pointing
// at the parent.
func (f *Fs) resolveRoot(ctx context.Context, root string) (fs.Fs, error) {
	switch err := f.resolvePath(ctx, root); err {
	case nil:
		// root is an existing directory - nothing to do
		return f, nil
	case fs.ErrorDirNotFound:
		return f.resolveMissingRoot(ctx, root)
	default:
		return nil, err
	}
}

// resolveMissingRoot handles a root that does not exist yet: when its parent
// exists the root may be a file waiting to be found or a directory to create
// lazily; otherwise the root is left untouched for rclone to create whole.
func (f *Fs) resolveMissingRoot(ctx context.Context, root string) (fs.Fs, error) {
	parent, leaf := splitRemote(root)
	if err2 := f.resolvePath(ctx, parent); err2 != nil {
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
	return fmt.Sprintf("Baidu Netdisk root '%s'", f.root)
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

// dirKey returns the drive-root-anchored dir path for a dir relative to the
// root of this Fs. "" (the empty remote path) is the root of this Fs.
func (f *Fs) dirKey(dir string) string {
	return strings.Trim(path.Join(strings.Trim(f.root, "/"), strings.Trim(dir, "/")), "/")
}

// absPath returns the absolute server path for a remote (object or dir),
// applying the configured encoding to each component. The drive root is "/".
func (f *Fs) absPath(remote string) string {
	abs := f.dirKey(remote)
	if abs == "" {
		return "/"
	}
	return "/" + f.opt.Enc.FromStandardPath(abs)
}

// resolvePath checks whether the directory path (anchored at the drive root,
// "" = drive root) exists, returning nil, fs.ErrorDirNotFound, or the list
// error. Paths are examined by listing the parent and matching the leaf name.
func (f *Fs) resolvePath(ctx context.Context, full string) error {
	if full == "" {
		return nil // the drive root always exists
	}
	parent, leaf := splitRemote(full)
	files, err := f.client.GetFiles(ctx, f.absPath(parent))
	if err != nil {
		return fs.ErrorDirNotFound
	}
	leafEnc := f.opt.Enc.FromStandardName(leaf)
	for i := range files {
		if files[i].Name() == leafEnc && files[i].Isdir != 0 {
			return nil
		}
	}
	return fs.ErrorDirNotFound
}

// List the objects and directories in dir into entries
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	files, err := f.client.GetFiles(ctx, f.absPath(dir))
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: list %q: %w", dir, err)
	}
	base := strings.Trim(dir, "/")
	for i := range files {
		file := &files[i]
		name := f.opt.Enc.ToStandardName(file.Name())
		remote := name
		if base != "" {
			remote = base + "/" + name
		}
		if file.Isdir != 0 {
			entries = append(entries, fs.NewDir(remote, file.ModTime()))
		} else {
			entries = append(entries, newObject(f, remote, *file))
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
	files, err := f.client.GetFiles(ctx, f.absPath(dir))
	if err != nil {
		// A missing parent means the object cannot exist either. Report
		// object-not-found so rclone's copy flow proceeds and creates the
		// directory on demand.
		return nil, fs.ErrorObjectNotFound
	}
	name := f.opt.Enc.FromStandardName(leaf)
	for i := range files {
		file := &files[i]
		if file.Name() != name {
			continue
		}
		if file.Isdir != 0 {
			return nil, fs.ErrorIsDir
		}
		return newObject(f, remote, *file), nil
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
	full := f.dirKey(dir)
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
		if err := f.resolvePath(ctx, next); err == nil {
			cur = next
			continue
		} else if err != fs.ErrorDirNotFound {
			return err
		}
		if err := f.client.CreateDir(ctx, f.absPath(next)); err != nil {
			return fmt.Errorf("baidunetdisk: mkdir %q: %w", next, err)
		}
		cur = next
	}
	return nil
}

// Rmdir removes the directory if it is empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if err := f.client.Manage(ctx, "delete", []map[string]any{{"path": f.absPath(dir)}}); err != nil {
		return fmt.Errorf("baidunetdisk: rmdir %q: %w", dir, err)
	}
	return nil
}

// Purge removes the directory dir and all its contents. Baidu deletes a
// folder (and everything under it) server-side, so this mirrors Rmdir.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	if err := f.client.Manage(ctx, "delete", []map[string]any{{"path": f.absPath(dir)}}); err != nil {
		return fmt.Errorf("baidunetdisk: purge %q: %w", dir, err)
	}
	return nil
}

// Move moves src to this remote, or within this remote
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	o, ok := src.(*Object)
	if !ok {
		return nil, fmt.Errorf("baidunetdisk: can't move non-baidunetdisk object %T", src)
	}
	srcDir, srcLeaf := splitRemote(o.remote)
	dstDir, dstLeaf := splitRemote(remote)
	if srcDir == dstDir && srcLeaf == dstLeaf {
		return o, nil
	}
	if srcDir != dstDir {
		if err := f.client.Manage(ctx, "move", []map[string]any{{
			"path":    o.fs.absPath(o.remote),
			"dest":    f.absPath(dstDir),
			"newname": f.opt.Enc.FromStandardName(dstLeaf),
		}}); err != nil {
			return nil, fmt.Errorf("baidunetdisk: move %q: %w", src, err)
		}
	} else {
		// same directory: a rename of the leaf
		if err := f.client.Manage(ctx, "rename", []map[string]any{{
			"path":    o.fs.absPath(o.remote),
			"newname": f.opt.Enc.FromStandardName(dstLeaf),
		}}); err != nil {
			return nil, fmt.Errorf("baidunetdisk: rename %q: %w", src, err)
		}
	}
	oo := *o
	oo.remote = remote
	return &oo, nil
}

// DirMove moves a directory
func (f *Fs) DirMove(ctx context.Context, srcFs fs.Fs, srcRemote, dstRemote string) error {
	if srcFs != f {
		return fs.ErrorCantDirMove
	}
	srcDir, _ := splitRemote(srcRemote)
	dstDir, dstLeaf := splitRemote(dstRemote)
	if srcDir == dstDir {
		return nil
	}
	if err := f.client.Manage(ctx, "move", []map[string]any{{
		"path":    f.absPath(srcRemote),
		"dest":    f.absPath(dstDir),
		"newname": f.opt.Enc.FromStandardName(dstLeaf),
	}}); err != nil {
		return fmt.Errorf("baidunetdisk: dir move %q: %w", srcRemote, err)
	}
	return nil
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	total, used, err := f.client.Quota(ctx)
	if err != nil {
		return nil, err
	}
	return &fs.Usage{
		Total: fs.NewUsageValue(total),
		Used:  fs.NewUsageValue(used),
	}, nil
}

// Update uploads new content for an existing object.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	_, err := o.fs.Put(ctx, in, src, options...)
	return err
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
// This is the server mtime. Baidu has no endpoint to set the modification
// time, which is why the backend reports ModTimeNotSupported precision: the
// value is informational only and never used for decisions.
func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.info.ModTime()
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
	return o.info.ID(), nil
}

// MimeType returns the MIME type of an object
func (o *Object) MimeType(ctx context.Context) string {
	return mime.TypeByExtension(path.Ext(o.remote))
}

// Remove this object
func (o *Object) Remove(ctx context.Context) error {
	if err := o.fs.client.Manage(ctx, "delete", []map[string]any{{"path": o.fs.absPath(o.remote)}}); err != nil {
		return fmt.Errorf("baidunetdisk: delete %q: %w", o.remote, err)
	}
	return nil
}

// Open opens the file for read. The requested span is served as a single
// ranged GET over a download URL cached per fs_id.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	start, end, err := parseObjectRange(options, o.info.Size)
	if err != nil {
		return nil, err
	}
	return o.openSpan(ctx, start, end)
}

// parseObjectRange resolves the requested span from the open options,
// defaulting to the whole object.
func parseObjectRange(options []fs.OpenOption, size int64) (start, end int64, err error) {
	start = int64(0)
	end = size - 1
	for _, option := range options {
		switch x := option.(type) {
		case *fs.RangeOption:
			start = x.Start
			if x.End >= 0 {
				end = x.End
			}
		case *fs.SeekOption:
			return 0, 0, fs.ErrorNotImplemented
		}
	}
	if start > end {
		return 0, 0, io.EOF
	}
	return start, end, nil
}

// openSpan serves one span as a ranged GET. A 4xx on the cached signature
// means it went stale: it is dropped and resolved fresh once.
func (o *Object) openSpan(ctx context.Context, start, end int64) (io.ReadCloser, error) {
	for attempt := 0; ; attempt++ {
		signedURL, err := o.downloadURL(ctx)
		if err != nil {
			return nil, err
		}
		headers := http.Header{}
		if end >= 0 {
			headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		}
		res, err := o.fs.client.Download(ctx, signedURL, headers)
		if err != nil {
			return nil, err
		}
		if res.StatusCode >= 400 && res.StatusCode <= 499 && attempt == 0 {
			_ = res.Body.Close()
			o.fs.client.DropDownloadURL(o.info)
			continue
		}
		return spanBody(o.remote, res, o.info.Size)
	}
}

// spanBody interprets a download response: a full reply means the server
// ignored the range (drained so the connection is reusable, then reported so
// rclone core handles the mismatch), a partial reply is the span itself.
func spanBody(remote string, res *http.Response, size int64) (io.ReadCloser, error) {
	if res.StatusCode == http.StatusOK {
		if _, err := io.Copy(io.Discard, io.LimitReader(res.Body, size+1)); err != nil {
			_ = res.Body.Close()
			return nil, err
		}
		_ = res.Body.Close()
		return nil, fs.ErrorRangeIgnored
	}
	if res.StatusCode != http.StatusPartialContent {
		_ = res.Body.Close()
		return nil, fmt.Errorf("baidunetdisk: download %q: status %d", remote, res.StatusCode)
	}
	return res.Body, nil
}

// downloadURL resolves the download URL for this object, reusing the cached
// signature for the span of ranged GETs.
func (o *Object) downloadURL(ctx context.Context) (string, error) {
	return o.fs.client.DownloadURL(ctx, o.info)
}

// check interfaces
var (
	_ fs.Fs       = (*Fs)(nil)
	_ fs.Mover    = (*Fs)(nil)
	_ fs.DirMover = (*Fs)(nil)
	_ fs.Abouter  = (*Fs)(nil)
	_ fs.Purger   = (*Fs)(nil)

	_ fs.Object    = (*Object)(nil)
	_ fs.MimeTyper = (*Object)(nil)
)

package baidunetdisk

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/backend/baidunetdisk/api"
	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	fshash "github.com/rclone/rclone/fs/hash"
	"golang.org/x/sync/errgroup"
)

// Slice sizing mirrors the reference driver: regular users are fixed at 4
// MiB, VIP at 16 MiB and super VIP at 32 MiB, with at most MaxSliceNum
// slices per file.
const (
	defaultSliceSize int64 = 4 << 20  // 4 MiB
	vipSliceSize     int64 = 16 << 20 // 16 MiB
	svipSliceSize    int64 = 32 << 20 // 32 MiB

	maxSliceNum     = 2048
	sliceGrowStep   = 1 << 20 // 1 MiB
	firstHashWindow = 256 << 10

	uploadSliceAttempts = 3
	uploadPasses        = 2
)

// spooled accumulates the hashes Baidu demands before uploading a single
// byte: the md5 of every slice (block_list), the whole-file content-md5 and
// the md5 of the first 256 KB (slice-md5).
type spooled struct {
	leaf      string
	blockList []string
	content   hash.Hash
	firstHash hash.Hash
	firstRest int64
	mu        sync.Mutex
}

func newSpooled(leaf string, slices int) *spooled {
	return &spooled{
		leaf:      leaf,
		blockList: make([]string, slices),
		content:   md5.New(),
		firstHash: md5.New(),
		firstRest: firstHashWindow,
	}
}

// hashSlice records buf as slice idx of the accumulated hashes. It is safe
// for concurrent callers.
func (sp *spooled) hashSlice(idx int, buf []byte) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	_, _ = sp.content.Write(buf)
	if sp.firstRest > 0 {
		n := int64(len(buf))
		if n > sp.firstRest {
			n = sp.firstRest
		}
		_, _ = sp.firstHash.Write(buf[:n])
		sp.firstRest -= n
	}
	h := md5.New()
	_, _ = h.Write(buf)
	sp.blockList[idx] = hex.EncodeToString(h.Sum(nil))
}

// writeSlice hashes buf as slice idx and stores it at its offset in tmp. It
// is safe for concurrent callers (parallel WriteChunk calls land on disjoint
// slices, but the running hashes are shared).
func (sp *spooled) writeSlice(tmp io.WriterAt, sliceSize int64, idx int, buf []byte) error {
	sp.hashSlice(idx, buf)
	_, err := tmp.WriteAt(buf, int64(idx)*sliceSize)
	return err
}

// hashes freezes the accumulated hashes for the precreate call.
func (sp *spooled) hashes() (blockList, contentMd5, sliceMd5 string) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	list, err := json.Marshal(sp.blockList)
	if err != nil {
		list = []byte("[]")
	}
	return string(list), hex.EncodeToString(sp.content.Sum(nil)), hex.EncodeToString(sp.firstHash.Sum(nil))
}

// baiduSliceSize ports the reference driver's slice sizing: non-VIP users are
// fixed at 4 MiB; VIP users get their tier size, a custom size when
// configured, or a grown size in low-bandwidth mode.
func baiduSliceSize(vipType int, filesize, custom int64, lowBand bool) int64 {
	if vipType == 0 {
		return regularSliceSize(filesize, custom)
	}
	return memberSliceSize(vipType, filesize, custom, lowBand)
}

// regularSliceSize sizes slices for non-VIP users: always the 4 MiB default,
// warning when the custom size or the file itself cannot apply.
func regularSliceSize(filesize, custom int64) int64 {
	if custom != 0 {
		fs.LogLevelPrintf(fs.LogLevelWarning, nil, "baidunetdisk: custom_upload_part_size is not supported for non-vip user, use default slice size")
	}
	if filesize > maxSliceNum*defaultSliceSize {
		fs.LogLevelPrintf(fs.LogLevelWarning, nil, "baidunetdisk: file size (%d) is too large, may cause upload failure", filesize)
	}
	return defaultSliceSize
}

// memberSliceSize sizes slices for VIP users: a validated custom size when
// configured, a grown size in low-bandwidth mode, else the tier size.
func memberSliceSize(vipType int, filesize, custom int64, lowBand bool) int64 {
	if custom != 0 {
		return clampCustomSlice(vipType, custom)
	}
	maxSliceSize := maxTierSlice(vipType)
	if lowBand {
		return grownSliceSize(filesize, maxSliceSize)
	}
	if filesize > maxSliceNum*maxSliceSize {
		fs.LogLevelPrintf(fs.LogLevelWarning, nil, "baidunetdisk: file size (%d) is too large, may cause upload failure", filesize)
	}
	return maxSliceSize
}

// clampCustomSlice validates a custom slice size against the tier cap,
// falling back with a warning when it is out of range.
func clampCustomSlice(vipType int, custom int64) int64 {
	if custom < defaultSliceSize {
		fs.LogLevelPrintf(fs.LogLevelWarning, nil, "baidunetdisk: custom_upload_part_size (%d) is less than default slice size (%d), use default", custom, defaultSliceSize)
		return defaultSliceSize
	}
	if tierCap := maxTierSlice(vipType); custom > tierCap {
		fs.LogLevelPrintf(fs.LogLevelWarning, nil, "baidunetdisk: custom_upload_part_size (%d) is greater than tier slice size (%d), use tier size", custom, tierCap)
		return tierCap
	}
	return custom
}

// maxTierSlice returns the slice size cap for an account tier.
func maxTierSlice(vipType int) int64 {
	switch vipType {
	case 1:
		return vipSliceSize
	case 2:
		return svipSliceSize
	}
	return defaultSliceSize
}

// grownSliceSize grows the slice in 1 MiB steps until the slice count fits
// MaxSliceNum, for low-bandwidth uploads.
func grownSliceSize(filesize, maxSliceSize int64) int64 {
	for size := defaultSliceSize; size <= maxSliceSize; size += sliceGrowStep {
		if filesize <= maxSliceNum*size {
			return size
		}
	}
	return maxSliceSize
}

// sliceSize resolves the upload slice size for a file: the account tier is
// fetched lazily on the first upload so listings and downloads never pay the
// extra uinfo call.
func (f *Fs) sliceSize(ctx context.Context, filesize int64) (int64, error) {
	vip, err := f.client.VipType(ctx)
	if err != nil {
		return 0, fmt.Errorf("baidunetdisk: account info: %w", err)
	}
	return baiduSliceSize(vip, filesize, f.opt.CustomUploadPartSize, f.opt.LowBandwithUploadMode), nil
}

// uploadThread clamps the configured parallel slice count to the 1-32 range
// the reference driver accepts.
func (f *Fs) uploadThread() int {
	if f.opt.UploadThread < 1 {
		return 1
	}
	if f.opt.UploadThread > 32 {
		return 32
	}
	return f.opt.UploadThread
}

// localSource returns the source as a re-readable object when it lives on a
// local backend. Local files are cheap to read twice, so the upload can hash
// them in one pass and serve the slices by re-opening ranges, without any
// temp spool. Anything else (including a local-looking ObjectInfo that is not
// a full object) reports false and the caller falls back to the spool path.
func localSource(src fs.ObjectInfo) (fs.Object, bool) {
	if src == nil {
		return nil, false
	}
	if _, ok := src.Fs().(*local.Fs); !ok {
		return nil, false
	}
	obj, ok := src.(fs.Object)
	if !ok {
		return nil, false
	}
	return obj, true
}

// rangeOpener serves slice bodies by re-opening ranges of a local source
// object. Each call opens a fresh reader, so retries get a new body.
func rangeOpener(ctx context.Context, obj fs.Object) func(offset, size int64) (io.Reader, error) {
	return func(offset, size int64) (io.Reader, error) {
		return obj.Open(ctx, &fs.RangeOption{Start: offset, End: offset + size - 1})
	}
}

// rapidMD5 returns the source's md5 when it can provide one without reading
// the stream, so the server can be asked up front whether the content already
// exists (秒传).
func (f *Fs) rapidMD5(ctx context.Context, src fs.ObjectInfo) (string, bool) {
	if src == nil {
		return "", false
	}
	fs.Infof(src, "baidunetdisk: computing source md5 to check if the upload already exists")
	md5Sum, err := src.Hash(ctx, fshash.MD5)
	if err != nil || md5Sum == "" {
		return "", false
	}
	return md5Sum, true
}

// rapidCreate tries the instant create: block_list carries the whole-file md5
// as its single entry, and a hit means the server already had the content.
// Any error (content unknown server-side, or a genuine failure) is a miss and
// the upload proceeds normally.
func (f *Fs) rapidCreate(ctx context.Context, absPath string, size int64, md5Sum string, ctime, mtime int64) (*api.File, bool) {
	list, _ := json.Marshal([]string{md5Sum})
	file, err := f.client.Create(ctx, absPath, size, "", string(list), ctime, mtime)
	if err != nil {
		fs.Debugf(absPath, "baidunetdisk: rapid create miss: %v", err)
		return nil, false
	}
	return file, true
}

// spoolAndHash reads size bytes from in in sliceSize pieces, hashing every
// slice inline as it streams and writing the data to tmp. The source is read
// exactly once; the slices are uploaded from tmp afterwards.
func (f *Fs) spoolAndHash(ctx context.Context, remote string, in io.Reader, size, sliceSize int64, tmp io.Writer) (*spooled, error) {
	count := 1
	if size > sliceSize {
		count = int((size + sliceSize - 1) / sliceSize)
	}
	lastSize := size % sliceSize
	if lastSize == 0 {
		lastSize = sliceSize
	}
	_, leaf := splitRemote(remote)
	sp := newSpooled(f.opt.Enc.FromStandardName(leaf), count)
	buf := make([]byte, sliceSize)
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		want := sliceSize
		if i == count-1 {
			want = lastSize
		}
		n, err := io.ReadFull(in, buf[:want])
		if err != nil {
			return nil, fmt.Errorf("baidunetdisk: read upload stream: %w", err)
		}
		fs.Debugf(remote, "baidunetdisk: hashing slice %d/%d", i+1, count)
		sp.hashSlice(i, buf[:n])
		if _, err := tmp.Write(buf[:n]); err != nil {
			return nil, fmt.Errorf("baidunetdisk: spool slice: %w", err)
		}
	}
	return sp, nil
}

// uploadSliceRetry uploads one slice, reopening a fresh body per attempt and
// retrying up to three times with a 1s/2s backoff like the reference driver.
// An expired uploadid surfaces as api.ErrUploadIDExpired so the caller
// recreates the upload from scratch.
func (f *Fs) uploadSliceRetry(ctx context.Context, uploadURL, absPath, uploadID string, partseq int, size int64, open func() (io.Reader, error)) error {
	params := url.Values{}
	params.Set("method", "upload")
	params.Set("type", "tmpfile")
	params.Set("path", absPath)
	params.Set("uploadid", uploadID)
	params.Set("partseq", strconv.Itoa(partseq))
	_, leaf := splitRemote(absPath)
	var lastErr error
	for attempt := 1; attempt <= uploadSliceAttempts; attempt++ {
		if attempt > 1 {
			delay := time.Duration(attempt-1) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		body, err := open()
		if err != nil {
			lastErr = fmt.Errorf("baidunetdisk: open slice %d: %w", partseq, err)
			fs.Debugf(absPath, "baidunetdisk: slice %d attempt %d failed: %v", partseq, attempt, lastErr)
			continue
		}
		if err := f.client.UploadSlice(ctx, uploadURL, params, leaf, body, size); err == nil {
			return nil
		} else {
			lastErr = err
			if errors.Is(err, api.ErrUploadIDExpired) || errors.Is(err, context.Canceled) {
				return err
			}
			fs.Debugf(absPath, "baidunetdisk: slice %d attempt %d failed: %v", partseq, attempt, err)
		}
	}
	return lastErr
}

// uploadJob carries one upload's fixed parameters from hashing through
// create, so the precreate → slices → create sequence fits in small helpers
// instead of one long function.
type uploadJob struct {
	f         *Fs
	remote    string
	absPath   string
	size      int64
	sliceSize int64
	ctime     int64
	mtime     int64
	sp        *spooled
}

// sectionOpener reads slice bodies back from a temp spool.
func sectionOpener(tmp io.ReaderAt) func(offset, size int64) (io.Reader, error) {
	return func(offset, size int64) (io.Reader, error) {
		return io.NewSectionReader(tmp, offset, size), nil
	}
}

// run executes the precreate → slices → create sequence over hashed content,
// fetching every slice body through open (called fresh on each attempt, so
// both spool sections and source range re-opens fit).
func (j *uploadJob) run(ctx context.Context, open func(offset, size int64) (io.Reader, error)) (*api.File, error) {
	blockList, contentMd5, sliceMd5 := j.sp.hashes()
	pre, file, err := j.precreate(ctx, blockList, contentMd5, sliceMd5)
	if err != nil || file != nil {
		return file, err
	}
	file, err = j.uploadLoop(ctx, pre, blockList, open)
	if err != nil || file != nil {
		return file, err
	}
	return j.finalize(ctx, pre, blockList)
}

// precreate runs one precreate step. A return_type of 2 means the server
// already has the content (秒传): the reply carries the created File and
// nothing is uploaded.
func (j *uploadJob) precreate(ctx context.Context, blockList, contentMd5, sliceMd5 string) (*api.PrecreateResp, *api.File, error) {
	pre, err := j.f.client.PreCreate(ctx, api.PreCreateArgs{
		Path:       j.absPath,
		BlockList:  blockList,
		ContentMd5: contentMd5,
		SliceMd5:   sliceMd5,
		Size:       j.size,
		Ctime:      j.ctime,
		Mtime:      j.mtime,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("baidunetdisk: precreate %q: %w", j.remote, err)
	}
	if pre.ReturnType == 2 {
		fs.Infof(j.remote, "baidunetdisk: content already present, instant-deduped (server hash match)")
		file := pre.File
		file.Ctime = j.ctime
		file.Mtime = j.mtime
		return pre, &file, nil
	}
	return pre, nil, nil
}

// uploadLoop uploads the missing slices, recreating the upload from scratch
// when the uploadid expires. It returns a file only when a re-precreate
// answered instant-dedupe.
func (j *uploadJob) uploadLoop(ctx context.Context, pre *api.PrecreateResp, blockList string, open func(offset, size int64) (io.Reader, error)) (*api.File, error) {
	lastSize := j.size % j.sliceSize
	if lastSize == 0 {
		lastSize = j.sliceSize
	}
	total := 0
	for _, partseq := range pre.BlockList {
		if partseq >= 0 {
			total++
		}
	}
	uploadURL := preUploadURL(ctx, j.f, j.absPath, pre)
	var lastErr error
	for pass := 0; pass < uploadPasses; pass++ {
		if uploadURL == "" {
			uploadURL = preUploadURL(ctx, j.f, j.absPath, pre)
		}
		err := j.uploadPass(ctx, pre, uploadURL, lastSize, total, open)
		if err == nil {
			return nil, nil
		}
		lastErr = err
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if !errors.Is(err, api.ErrUploadIDExpired) {
			return nil, fmt.Errorf("baidunetdisk: upload slices %q: %w", j.remote, err)
		}
		fs.Infof(j.remote, "baidunetdisk: uploadid expired, restarting from scratch")
		var file *api.File
		pre, file, err = j.precreate(ctx, blockList, "", "")
		if err != nil || file != nil {
			return file, err
		}
		uploadURL = ""
	}
	return nil, lastErr
}

// uploadPass uploads every missing slice concurrently. Each slice is marked
// done on the precreate response so a restarted pass skips it.
func (j *uploadJob) uploadPass(ctx context.Context, pre *api.PrecreateResp, uploadURL string, lastSize int64, total int, open func(offset, size int64) (io.Reader, error)) error {
	var completed atomic.Int64
	var nextLog atomic.Int64
	nextLog.Store(5)
	var markMu sync.Mutex
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(j.f.uploadThread())
	for i, partseq := range pre.BlockList {
		if partseq < 0 {
			continue
		}
		i, partseq := i, partseq
		offset := int64(partseq) * j.sliceSize
		sz := j.sliceSize
		if int64(partseq)+1 == int64(len(j.sp.blockList)) {
			sz = lastSize
		}
		g.Go(func() error {
			if err := j.f.uploadSliceRetry(gCtx, uploadURL, j.absPath, pre.Uploadid, partseq, sz,
				func() (io.Reader, error) { return open(offset, sz) }); err != nil {
				return err
			}
			// Second half of the split progress bar: the core counted the
			// source reads, the backend reports the destination uploads.
			accounting.AddUploadProgress(gCtx, sz)
			markMu.Lock()
			pre.BlockList[i] = -1
			markMu.Unlock()
			j.noteProgress(completed.Add(1), total, &nextLog)
			return nil
		})
	}
	return g.Wait()
}

// noteProgress logs the transfer every 5% so the upload stays visible (the
// core's progress bar only tracks source reads, which finished while
// hashing).
func (j *uploadJob) noteProgress(done int64, total int, nextLog *atomic.Int64) {
	if total <= 0 {
		return
	}
	if pct := done * 100 / int64(total); pct >= nextLog.Load() {
		nextLog.Store(pct + 5)
		fs.Infof(j.remote, "baidunetdisk: uploaded %d%% (%d/%d slices)", pct, done, total)
	}
}

// finalize creates the file entry after every slice is on the server.
func (j *uploadJob) finalize(ctx context.Context, pre *api.PrecreateResp, blockList string) (*api.File, error) {
	file, err := j.f.client.Create(ctx, j.absPath, j.size, pre.Uploadid, blockList, j.ctime, j.mtime)
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: create %q: %w", j.remote, err)
	}
	return file, nil
}

// uploadAPIBase is the fallback upload domain when the option is empty.
const uploadAPIBase = "https://d.pcs.baidu.com"

// uploadBase returns the configured upload_api setting with the OpenList
// fallback default.
func (f *Fs) uploadBase() string {
	if f.opt.UploadAPI == "" {
		return uploadAPIBase
	}
	return f.opt.UploadAPI
}

// preUploadURL picks the upload domain for a precreate: the dynamic lookup
// result when enabled, otherwise the configured upload_api fallback.
func preUploadURL(ctx context.Context, f *Fs, absPath string, pre *api.PrecreateResp) string {
	if !f.opt.UseDynamicUploadAPI || pre.Uploadid == "" {
		return f.uploadBase()
	}
	if uploadURL, err := f.client.LocateUpload(ctx, absPath, pre.Uploadid); err == nil {
		return uploadURL
	} else {
		fs.Debugf(absPath, "baidunetdisk: locateupload failed, using upload_api fallback: %v", err)
	}
	return f.uploadBase()
}

// finishUpload looks the object up after an upload: the entry may need a
// moment before it is visible, mirroring the ported visibility throttle.
func (f *Fs) finishUpload(ctx context.Context, remote string) (fs.Object, error) {
	time.Sleep(time.Second)
	return f.NewObject(ctx, remote)
}

// uploadTemp spools size bytes from in through spoolAndHash, uploads from the
// spool, and removes the temp file.
func (f *Fs) uploadTemp(ctx context.Context, remote string, in io.Reader, size, sliceSize int64, ctime, mtime int64) (fs.Object, error) {
	tmp, err := os.CreateTemp("", "rclone-baidunetdisk-*")
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: temp file: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	sp, err := f.spoolAndHash(ctx, remote, in, size, sliceSize, tmp)
	if err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("baidunetdisk: rewind spool: %w", err)
	}
	job := &uploadJob{f: f, remote: remote, absPath: f.absPath(remote), size: size, sliceSize: sliceSize, ctime: ctime, mtime: mtime, sp: sp}
	if _, err := job.run(ctx, sectionOpener(tmp)); err != nil {
		return nil, err
	}
	return f.finishUpload(ctx, remote)
}

// Put uploads the object: when the source can provide an md5 the server is
// first asked whether the content already exists (秒传) and a hit creates the
// entry instantly without reading the stream. Otherwise the source is hashed
// in one pass and the slices are uploaded: from a temp spool, or - when the
// source is a local file - by re-opening ranges of the source itself, which
// needs no temp file at all.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	size := src.Size()
	if size < 1 {
		return nil, fmt.Errorf("baidunetdisk: upload %q: %w", remote, api.ErrBaiduEmptyFilesNotAllowed)
	}
	mtime := src.ModTime(ctx).Unix()
	ctime := mtime
	absPath := f.absPath(remote)
	if md5Sum, ok := f.rapidMD5(ctx, src); ok {
		if _, hit := f.rapidCreate(ctx, absPath, size, md5Sum, ctime, mtime); hit {
			fs.Infof(src, "baidunetdisk: content already present, instant-deduped (source hash)")
			return f.finishUpload(ctx, remote)
		}
	}
	sliceSize, err := f.sliceSize(ctx, size)
	if err != nil {
		return nil, err
	}
	if obj, ok := localSource(src); ok {
		// Local source: hash the stream while discarding it, then serve the
		// slices by re-opening ranges of the local file. No temp spool.
		sp, err := f.spoolAndHash(ctx, remote, in, size, sliceSize, io.Discard)
		if err != nil {
			return nil, err
		}
		job := &uploadJob{f: f, remote: remote, absPath: absPath, size: size, sliceSize: sliceSize, ctime: ctime, mtime: mtime, sp: sp}
		if _, err := job.run(ctx, rangeOpener(ctx, obj)); err != nil {
			return nil, err
		}
		return f.finishUpload(ctx, remote)
	}
	return f.uploadTemp(ctx, remote, in, size, sliceSize, ctime, mtime)
}

// PutStream uploads the object with unknown size, spooling to a temp file
// first so the slice hashes are known before the upload starts.
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	tmp, err := os.CreateTemp("", "rclone-baidunetdisk-*")
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: temp file: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	size, err := io.Copy(tmp, in)
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: spool stream: %w", err)
	}
	if size < 1 {
		return nil, fmt.Errorf("baidunetdisk: upload %q: %w", src.Remote(), api.ErrBaiduEmptyFilesNotAllowed)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("baidunetdisk: rewind stream: %w", err)
	}
	sliceSize, err := f.sliceSize(ctx, size)
	if err != nil {
		return nil, err
	}
	mtime := src.ModTime(ctx).Unix()
	ctime := mtime
	sp, err := f.spoolAndHash(ctx, src.Remote(), tmp, size, sliceSize, io.Discard)
	if err != nil {
		return nil, err
	}
	job := &uploadJob{f: f, remote: src.Remote(), absPath: f.absPath(src.Remote()), size: size, sliceSize: sliceSize, ctime: ctime, mtime: mtime, sp: sp}
	if _, err := job.run(ctx, sectionOpener(tmp)); err != nil {
		return nil, err
	}
	return f.finishUpload(ctx, src.Remote())
}

// chunkWriter implements fs.ChunkWriter for the slice upload. In dedupe mode
// the entry already exists server-side, so chunks are consumed and discarded;
// otherwise each chunk is a whole slice, hashed as it streams. Spool mode
// stores each slice at its offset in a temp file for the upload; local mode
// (local source) only hashes and discards, and Close serves the slices by
// re-opening ranges of the source. Close runs the precreate → slices →
// create sequence either way.
type chunkWriter struct {
	f         *Fs
	remote    string
	absPath   string
	size      int64
	sliceSize int64
	ctime     int64
	mtime     int64
	deduped   bool

	tmp   *os.File  // nil in local mode
	local fs.Object // non-nil in local mode
	sp    *spooled
}

// AlreadyExists implements fs.ChunkWriterAlreadyExistser: when the dedupe
// pre-check hit, the object already exists on the server, so multiThreadCopy
// skips reading the source and writing chunks entirely.
func (w *chunkWriter) AlreadyExists() bool {
	return w.deduped
}

// OpenChunkWriter returns a chunk writer for the slice upload. When the
// source can provide an md5 the server is asked before any chunk is read: a
// hit creates the entry instantly and the writer runs in discard mode (秒传).
// The chunk size equals the slice size so every chunk is exactly one slice.
// With a local source the writer only hashes (no temp spool); Close serves
// the slices by re-opening ranges of the source.
func (f *Fs) OpenChunkWriter(ctx context.Context, remote string, src fs.ObjectInfo, options ...fs.OpenOption) (fs.ChunkWriterInfo, fs.ChunkWriter, error) {
	size := src.Size()
	if size < 1 {
		return fs.ChunkWriterInfo{}, nil, fmt.Errorf("baidunetdisk: upload %q: %w", remote, api.ErrBaiduEmptyFilesNotAllowed)
	}
	mtime := src.ModTime(ctx).Unix()
	ctime := mtime
	absPath := f.absPath(remote)
	deduped := false
	if md5Sum, ok := f.rapidMD5(ctx, src); ok {
		if _, hit := f.rapidCreate(ctx, absPath, size, md5Sum, ctime, mtime); hit {
			fs.Infof(src, "baidunetdisk: content already present, instant-deduped (source hash)")
			deduped = true
		}
	}
	sliceSize, err := f.sliceSize(ctx, size)
	if err != nil {
		return fs.ChunkWriterInfo{}, nil, err
	}
	localObj, _ := localSource(src)
	var tmp *os.File
	if localObj == nil {
		tmp, err = os.CreateTemp("", "rclone-baidunetdisk-*")
		if err != nil {
			return fs.ChunkWriterInfo{}, nil, fmt.Errorf("baidunetdisk: temp file: %w", err)
		}
	}
	_, leaf := splitRemote(remote)
	count := 1
	if size > sliceSize {
		count = int((size + sliceSize - 1) / sliceSize)
	}
	w := &chunkWriter{
		f:         f,
		remote:    remote,
		absPath:   absPath,
		size:      size,
		sliceSize: sliceSize,
		ctime:     ctime,
		mtime:     mtime,
		deduped:   deduped,
		tmp:       tmp,
		local:     localObj,
		sp:        newSpooled(f.opt.Enc.FromStandardName(leaf), count),
	}
	return fs.ChunkWriterInfo{
		ChunkSize:   sliceSize,
		Concurrency: f.uploadThread(),
	}, w, nil
}

// WriteChunk writes a chunk of the object. chunkNumber starts at 0 and maps
// one-to-one onto the upload slices. In local mode the chunk is only hashed
// (the slices are re-opened from the source on Close); otherwise it is
// hashed and spooled at its offset.
func (w *chunkWriter) WriteChunk(ctx context.Context, chunkNumber int, reader io.ReadSeeker) (int64, error) {
	if w.deduped {
		// The entry already exists; consume the chunk so the core's accounting
		// and read loop keep working, but never upload a slice.
		return io.Copy(io.Discard, reader)
	}
	n, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return 0, fmt.Errorf("baidunetdisk: read chunk: %w", err)
	}
	if chunkNumber < 0 || chunkNumber >= len(w.sp.blockList) {
		return 0, fmt.Errorf("baidunetdisk: chunk %d out of range", chunkNumber)
	}
	if w.local != nil {
		w.sp.hashSlice(chunkNumber, buf)
		return n, nil
	}
	if err := w.sp.writeSlice(w.tmp, w.sliceSize, chunkNumber, buf); err != nil {
		return 0, fmt.Errorf("baidunetdisk: spool slice: %w", err)
	}
	return n, nil
}

// Close runs the precreate → slices → create sequence, reading the slices
// back from the spool or - in local mode - by re-opening ranges of the
// source. In dedupe mode the entry was already created by the instant-dedupe
// response, so this only waits the visibility throttle before the core looks
// it up.
func (w *chunkWriter) Close(ctx context.Context) error {
	if w.tmp != nil {
		defer func() {
			_ = w.tmp.Close()
			_ = os.Remove(w.tmp.Name())
		}()
	}
	if w.deduped {
		time.Sleep(time.Second)
		return nil
	}
	complete := false
	for _, h := range w.sp.blockList {
		if h != "" {
			complete = true
			break
		}
	}
	if !complete {
		return fmt.Errorf("baidunetdisk: no data written for %q", w.remote)
	}
	if w.local != nil {
		job := &uploadJob{f: w.f, remote: w.remote, absPath: w.absPath, size: w.size, sliceSize: w.sliceSize, ctime: w.ctime, mtime: w.mtime, sp: w.sp}
		_, err := job.run(ctx, rangeOpener(ctx, w.local))
		return err
	}
	job := &uploadJob{f: w.f, remote: w.remote, absPath: w.absPath, size: w.size, sliceSize: w.sliceSize, ctime: w.ctime, mtime: w.mtime, sp: w.sp}
	_, err := job.run(ctx, sectionOpener(w.tmp))
	return err
}

// Abort discards the upload, removing the spool when there is one. The
// server never saw the slices (precreate gates every upload), so there is
// nothing to cancel remotely.
func (w *chunkWriter) Abort(ctx context.Context) error {
	if w.tmp != nil {
		_ = w.tmp.Close()
		_ = os.Remove(w.tmp.Name())
	}
	fs.Debugf(w.remote, "baidunetdisk: aborted upload of %s", w.remote)
	return nil
}

// check interfaces
var (
	_ fs.OpenChunkWriter            = (*Fs)(nil)
	_ fs.PutStreamer                = (*Fs)(nil)
	_ fs.ChunkWriter                = (*chunkWriter)(nil)
	_ fs.ChunkWriterAlreadyExistser = (*chunkWriter)(nil)
)

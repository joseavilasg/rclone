package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/quark/api"
	"github.com/rclone/rclone/fs"
	fshash "github.com/rclone/rclone/fs/hash"
)

// uploadPartRetry uploads a single part, retrying re-readable bodies up to 3
// attempts like the reference driver.
func (f *Fs) uploadPartRetry(ctx context.Context, pre api.UpPreResp, mimeType string, partNumber int, size int64, body *bytes.Reader) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			if _, err := body.Seek(0, io.SeekStart); err != nil {
				return "", err
			}
		}
		etag, err := f.client.UploadPart(ctx, pre, mimeType, partNumber, size, body)
		if err == nil {
			return etag, nil
		}
		lastErr = err
		fs.Debugf(pre.Data.TaskId, "quark: part %d attempt %d failed: %v", partNumber, attempt, err)
		time.Sleep(time.Second)
	}
	return "", lastErr
}

// uploadCommit finishes a task: registers the content hashes then commits the
// parts. When the server already has the content (finish=true) the entry is
// created instantly and the commit becomes a no-op, just like the reference
// driver's instant-dedupe path. Both paths wait the ported visibility throttle
// before returning: the entry is created asynchronously server-side and rclone
// looks the object up immediately after the upload.
func (f *Fs) uploadCommit(ctx context.Context, pre api.UpPreResp, md5Sum, sha1Sum string, etags []string) error {
	finished, err := f.client.UploadHash(ctx, pre, md5Sum, sha1Sum)
	if err != nil {
		return err
	}
	if finished {
		fs.Debugf(pre.Data.TaskId, "quark: content already present, instant-deduped")
	} else {
		if err := f.client.UploadCommit(ctx, pre, etags); err != nil {
			return fmt.Errorf("quark: upload commit: %w", err)
		}
		if err := f.client.UploadFinish(ctx, pre); err != nil {
			return fmt.Errorf("quark: upload finish: %w", err)
		}
	}
	// ported throttle: the server needs a moment before the entry is visible
	time.Sleep(time.Second)
	return nil
}

// sourceHashes returns the md5 and sha1 of the source object when it can
// provide both without us having to read the stream, and ok=false otherwise.
// quark looks the content up by md5+sha1 together, so the instant-dedupe
// pre-check only makes sense when both are present.
func sourceHashes(ctx context.Context, src fs.ObjectInfo) (md5Sum, sha1Sum string, ok bool) {
	if src == nil {
		return "", "", false
	}
	fs.Infof(src, "quark: computing source hashes to check if the upload already exists")
	md5Sum, err := src.Hash(ctx, fshash.MD5)
	if err != nil || md5Sum == "" {
		return "", "", false
	}
	sha1Sum, err = src.Hash(ctx, fshash.SHA1)
	if err != nil || sha1Sum == "" {
		return "", "", false
	}
	return md5Sum, sha1Sum, true
}

// dedupePrecheck asks the server whether the source content already exists,
// using the hashes the source provides. finished=false means either the source
// could not provide both hashes (no pre-check possible) or it is a miss and the
// upload must proceed. finished=true means quark has the content already: the
// entry was created instantly by that response (秒传) and the upload must be
// skipped entirely.
func (f *Fs) dedupePrecheck(ctx context.Context, src fs.ObjectInfo, pre api.UpPreResp) (finished bool, err error) {
	md5Sum, sha1Sum, ok := sourceHashes(ctx, src)
	if !ok {
		return false, nil
	}
	finished, err = f.client.UploadHash(ctx, pre, md5Sum, sha1Sum)
	if err != nil {
		return false, err
	}
	if finished {
		fs.Infof(pre.Data.TaskId, "quark: content already present, instant-deduped (source hash)")
	}
	return finished, nil
}

// newUploadTask creates the destination directory if needed and asks the server
// for an upload task for remote.
func (f *Fs) newUploadTask(ctx context.Context, remote string, size int64, formatType string) (api.UpPreResp, error) {
	dir, leaf := splitRemote(remote)
	parentID, err := f.resolveDirCreate(ctx, dir)
	if err != nil {
		return api.UpPreResp{}, err
	}
	pre, err := f.client.UploadPre(ctx, f.opt.Enc.FromStandardName(leaf), parentID, size, formatType)
	if err != nil {
		return api.UpPreResp{}, fmt.Errorf("quark: upload pre %q: %w", remote, err)
	}
	return pre, nil
}

// uploadStream uploads size bytes from in as a new file at remote, hashing the
// parts as they stream so the upload stays single-pass.
func (f *Fs) uploadStream(ctx context.Context, remote string, size int64, formatType string, in io.Reader) error {
	pre, err := f.newUploadTask(ctx, remote, size, formatType)
	if err != nil {
		return err
	}
	return f.uploadPartsAndCommit(ctx, remote, pre, formatType, size, in)
}

// uploadPartsAndCommit streams size bytes from in as parts of the pre upload
// task, hashing inline as they stream, and commits the completed upload.
func (f *Fs) uploadPartsAndCommit(ctx context.Context, remote string, pre api.UpPreResp, formatType string, size int64, in io.Reader) error {
	partSize := int64(pre.Metadata.PartSize)
	if partSize <= 0 {
		return fmt.Errorf("quark: pre response has bad part size %d", pre.Metadata.PartSize)
	}
	w := &chunkWriter{
		f:        f,
		pre:      pre,
		mimeType: formatType,
		md5Sum:   md5.New(),
		sha1Sum:  sha1.New(),
	}
	if size > 0 {
		etags, err := w.uploadParts(ctx, size, partSize, in)
		if err != nil {
			return err
		}
		fs.Debugf(remote, "quark: uploaded %d parts (%d bytes)", len(etags), size)
	}
	return f.uploadCommit(ctx, pre, w.sumMD5(), w.sumSHA1(), w.etags)
}

// Put uploads the object to the remote. When the source can provide both hashes
// the server is asked up front: a hit creates the entry instantly and skips the
// transfer entirely (秒传) instead of uploading first and deduping afterwards.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	size := src.Size()
	if size < 0 {
		return nil, fmt.Errorf("quark: cannot upload a stream of unknown size")
	}
	remote := src.Remote()
	formatType := fs.MimeType(ctx, src)
	pre, err := f.newUploadTask(ctx, remote, size, formatType)
	if err != nil {
		return nil, err
	}
	finished, err := f.dedupePrecheck(ctx, src, pre)
	if err != nil {
		return nil, err
	}
	if finished {
		// the entry was created by the instant-dedupe response; the visibility
		// throttle makes it findable right away on the lookup below.
		time.Sleep(time.Second)
		return f.NewObject(ctx, remote)
	}
	if err := f.uploadPartsAndCommit(ctx, remote, pre, formatType, size, in); err != nil {
		return nil, fmt.Errorf("quark: upload commit %q: %w", remote, err)
	}
	return f.NewObject(ctx, remote)
}

// PutStream uploads the object with unknown size, spooling to a temp file
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	tmp, err := os.CreateTemp("", "rclone-quark-*")
	if err != nil {
		return nil, fmt.Errorf("quark: temp file: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	size, err := io.Copy(tmp, in)
	if err != nil {
		return nil, fmt.Errorf("quark: spool stream: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("quark: rewind stream: %w", err)
	}
	if err := f.uploadStream(ctx, src.Remote(), size, fs.MimeType(ctx, src), tmp); err != nil {
		return nil, err
	}
	return f.NewObject(ctx, src.Remote())
}

// chunkWriter implements fs.ChunkWriter for the multipart OSS upload
type chunkWriter struct {
	f        *Fs
	pre      api.UpPreResp
	mimeType string
	deduped  bool // content already exists: chunks are consumed and discarded

	mu      sync.Mutex
	etags   []string
	md5Sum  hash.Hash
	sha1Sum hash.Hash
}

// AlreadyExists implements fs.ChunkWriterAlreadyExistser: when the dedupe
// pre-check hit, the object already exists on quark, so multiThreadCopy skips
// reading the source and writing chunks entirely.
func (w *chunkWriter) AlreadyExists() bool {
	return w.deduped
}

// OpenChunkWriter returns a chunk writer for the OSS multipart upload. When the
// source can provide both hashes the server is asked before any chunk is read:
// a hit creates the entry instantly and the writer runs in discard mode so no
// part is ever uploaded (秒传), the core's chunk reads still count as progress.
func (f *Fs) OpenChunkWriter(ctx context.Context, remote string, src fs.ObjectInfo, options ...fs.OpenOption) (fs.ChunkWriterInfo, fs.ChunkWriter, error) {
	if src.Size() <= 0 {
		return fs.ChunkWriterInfo{}, nil, fmt.Errorf("quark: OpenChunkWriter requires a positive size")
	}
	formatType := fs.MimeType(ctx, src)
	pre, err := f.newUploadTask(ctx, remote, src.Size(), formatType)
	if err != nil {
		return fs.ChunkWriterInfo{}, nil, err
	}
	partSize := int64(pre.Metadata.PartSize)
	if partSize <= 0 {
		return fs.ChunkWriterInfo{}, nil, fmt.Errorf("quark: pre response has bad part size %d", pre.Metadata.PartSize)
	}
	deduped, err := f.dedupePrecheck(ctx, src, pre)
	if err != nil {
		return fs.ChunkWriterInfo{}, nil, err
	}
	w := &chunkWriter{
		f:        f,
		pre:      pre,
		mimeType: formatType,
		deduped:  deduped,
		md5Sum:   md5.New(),
		sha1Sum:  sha1.New(),
	}
	return fs.ChunkWriterInfo{
		ChunkSize:   partSize,
		Concurrency: 1,
	}, w, nil
}

// WriteChunk writes a chunk of the object. chunkNumber starts at 0.
func (w *chunkWriter) WriteChunk(ctx context.Context, chunkNumber int, reader io.ReadSeeker) (int64, error) {
	if w.deduped {
		// the entry already exists; consume the chunk so the core's accounting
		// and read loop keep working, but never upload a part.
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
		return 0, fmt.Errorf("quark: read chunk: %w", err)
	}
	_, _ = w.md5Sum.Write(buf)
	_, _ = w.sha1Sum.Write(buf)
	etag, err := w.f.uploadPartRetry(ctx, w.pre, w.mimeType, chunkNumber+1, n, bytes.NewReader(buf))
	if err != nil {
		return 0, err
	}
	w.mu.Lock()
	w.etags = append(w.etags, etag)
	w.mu.Unlock()
	return n, nil
}

// uploadParts streams size bytes from in in partSize chunks, hashing each part
// inline as it streams and uploading it. The etags accumulate on the writer so
// it stays commit-ready whether finished through Close or committed directly.
func (w *chunkWriter) uploadParts(ctx context.Context, size, partSize int64, in io.Reader) ([]string, error) {
	buf := make([]byte, partSize)
	remaining := size
	part := 1
	for remaining > 0 {
		read := partSize
		if remaining < read {
			read = remaining
		}
		n, err := io.ReadFull(in, buf[:read])
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return nil, fmt.Errorf("quark: read part: %w", err)
		}
		if n == 0 {
			return nil, fmt.Errorf("quark: source ended before declared size %d", size)
		}
		chunk := buf[:n]
		_, _ = w.md5Sum.Write(chunk)
		_, _ = w.sha1Sum.Write(chunk)
		etag, err := w.f.uploadPartRetry(ctx, w.pre, w.mimeType, part, int64(n), bytes.NewReader(chunk))
		if err != nil {
			return nil, err
		}
		w.mu.Lock()
		w.etags = append(w.etags, etag)
		w.mu.Unlock()
		remaining -= int64(n)
		part++
	}
	return w.etags, nil
}

// Close commits the completed upload. In dedupe mode the entry was already
// created by the instant-dedupe response, so this only waits the visibility
// throttle before the core looks the object up.
func (w *chunkWriter) Close(ctx context.Context) error {
	if w.deduped {
		time.Sleep(time.Second)
		return nil
	}
	return w.f.uploadCommit(ctx, w.pre, w.sumMD5(), w.sumSHA1(), w.etags)
}

// Abort discards the upload. The orphaned OSS parts are cleaned up by the
// server, so there is nothing to cancel remotely.
func (w *chunkWriter) Abort(ctx context.Context) error {
	fs.Debugf(w.pre.Data.TaskId, "quark: aborted upload of %s", w.pre.Data.ObjKey)
	return nil
}

// sumMD5 hex-encodes the running md5 of the uploaded content
func (w *chunkWriter) sumMD5() string {
	return hex.EncodeToString(w.md5Sum.Sum(nil))
}

// sumSHA1 hex-encodes the running sha1 of the uploaded content
func (w *chunkWriter) sumSHA1() string {
	return hex.EncodeToString(w.sha1Sum.Sum(nil))
}

// check interfaces
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ fs.OpenChunkWriter = (*Fs)(nil)
	_ fs.PutStreamer     = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.ChunkWriter     = (*chunkWriter)(nil)
)

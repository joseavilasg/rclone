// Package api provides the Baidu Netdisk API client
package api

import (
	"errors"
	"path"
	"strconv"
	"time"
)

// ErrBaiduEmptyFilesNotAllowed is reported when trying to upload a zero-byte
// file: Baidu Netdisk refuses empty files.
var ErrBaiduEmptyFilesNotAllowed = errors.New("empty files are not allowed by baidu netdisk")

// TokenErrResp is the error body of the OAuth token endpoint
type TokenErrResp struct {
	ErrorDescription string `json:"error_description"`
	Error            string `json:"error"`
}

// File describes a file or directory. The md5 field is deliberately ignored:
// Baidu's API cannot be trusted with it (the OpenList driver drops it too).
// ServerMtime/ServerCtime fall back to the create-time Ctime/Mtime fields,
// which only appear in create/precreate responses.
type File struct {
	Category int   `json:"category"`
	FsId     int64 `json:"fs_id"`
	Thumbs   struct {
		Url3 string `json:"url3"`
	} `json:"thumbs"`
	Size           int64  `json:"size"`
	Path           string `json:"path"`
	ServerFilename string `json:"server_filename"`
	Md5            string `json:"md5"`
	Isdir          int    `json:"isdir"`

	ServerCtime int64 `json:"server_ctime"`
	ServerMtime int64 `json:"server_mtime"`
	LocalMtime  int64 `json:"local_mtime"`
	LocalCtime  int64 `json:"local_ctime"`

	Ctime int64 `json:"ctime"`
	Mtime int64 `json:"mtime"`
}

// Name returns the file name, falling back to the base of the path when the
// server omitted server_filename.
func (f File) Name() string {
	if f.ServerFilename == "" {
		return path.Base(f.Path)
	}
	return f.ServerFilename
}

// ModTime returns the server modification time, falling back to the create
// time when only that is present.
func (f File) ModTime() time.Time {
	secs := f.ServerMtime
	if secs == 0 {
		secs = f.Mtime
	}
	return time.Unix(secs, 0)
}

// ID returns the fs_id as a string.
func (f File) ID() string {
	return strconv.FormatInt(f.FsId, 10)
}

// ListResp is the response of /xpan/file?method=list
type ListResp struct {
	Errno     int    `json:"errno"`
	List      []File `json:"list"`
	RequestId int64  `json:"request_id"`
}

// DownloadResp is the response of /xpan/multimedia (official dlink)
type DownloadResp struct {
	Errmsg string `json:"errmsg"`
	Errno  int    `json:"errno"`
	List   []struct {
		Dlink string `json:"dlink"`
	} `json:"list"`
	RequestId string `json:"request_id"`
}

// DownloadResp2 is the response of /api/filemetas and /api/mediainfo
type DownloadResp2 struct {
	Errno int `json:"errno"`
	Info  []struct {
		Dlink string `json:"dlink"`
	} `json:"info"`
	RequestID int64 `json:"request_id"`
}

// QuotaResp is the response of /api/quota
type QuotaResp struct {
	Errno     int   `json:"errno"`
	RequestId int64 `json:"request_id"`
	Total     int64 `json:"total"`
	Used      int64 `json:"used"`
}

// ErrUploadIDExpired marks a slice upload whose uploadid must be recreated:
// the server rejected it as invalid, expired or not found, so the whole upload
// has to start over with a fresh precreate.
var ErrUploadIDExpired = errors.New("uploadid expired")

// PrecreateResp is the response of /xpan/file?method=precreate. ReturnType 1
// means the slice upload must proceed (Uploadid + which parts are already on
// the server); ReturnType 2 means the server already has the file (秒传) and
// the reply also carries the created File.
type PrecreateResp struct {
	Errno      int   `json:"errno"`
	RequestId  int64 `json:"request_id"`
	ReturnType int   `json:"return_type"`

	// return_type=1: the upload to perform
	Path      string `json:"path"`
	Uploadid  string `json:"uploadid"`
	BlockList []int  `json:"block_list"` // partseq per slice; -1 = already uploaded

	// return_type=2: the file was created instantly
	File File `json:"info"`
}

// PreCreateArgs carries the pre-upload parameters: the destination path and
// size plus the hashes Baidu demands before the first byte (the md5 of every
// slice, the whole-file content-md5 and the first-256KB slice-md5). A
// re-precreate after an expired uploadid passes empty hashes.
type PreCreateArgs struct {
	Path       string
	BlockList  string
	ContentMd5 string
	SliceMd5   string
	Size       int64
	Ctime      int64
	Mtime      int64
}

// UploadServerResp is the response of the locateupload endpoint, used to pick
// the dynamic upload domain.
type UploadServerResp struct {
	Servers []struct {
		Server string `json:"server"`
	} `json:"servers"`
	BakServers []struct {
		Server string `json:"server"`
	} `json:"bak_servers"`
}

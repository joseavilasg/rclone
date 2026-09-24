package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/pacer"
)

const (
	defaultMinSleep = 10 * time.Millisecond
	defaultMaxSleep = 2 * time.Second
	defaultDecay    = 2
)

// bodyLimit is the amount to read from the API response body
const bodyLimit = 10 * 1024 * 1024

// downloadURLTTL is how long a signed download URL is reused before re-signing.
// quark's CDN throttles fresh URLs until they have been exercised a few times,
// so keeping one URL per file lets a download warm up and stay fast (see
// Object.Open in the quark backend).
const downloadURLTTL = 10 * time.Minute

// downloadURLEntry is a signed download URL with its expiry.
type downloadURLEntry struct {
	url     string
	expires time.Time
}

// userAgent and endpoint quirks ported verbatim from the OpenList quark_uc
// driver. Do not "modernise" them without re-validating a real upload.
const (
	UserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/2.5.20 Chrome/100.0.4896.160 Electron/18.3.5.4-b478491100 Safari/537.36 Channel/pckk_other_ch"
	OSSUA      = "aliyun-sdk-js/6.6.1 Chrome 98.0.4758.80 on Windows 10 64-bit"
	OSSReferer = "https://pan.quark.cn/"
)

// retryErrorCodes is a list of HTTP response status codes that we will retry
var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
	509, // Bandwidth Limit Exceeded
}

// Client is the Quark API client
type Client struct {
	http     *http.Client
	download *http.Client
	api      string // base API URL (drive.quark.cn/1/clouddrive)
	referer  string
	pr       string
	pacer    *fs.Pacer

	mu              sync.Mutex
	cookie          string // cookie header value, rotated from Set-Cookie responses
	persistCookieFn func()
	schemeWarnOnce  sync.Once           // warns once per client when transfers downgrade to http
	httpHosts       map[string]struct{} // PDS hosts flipped to http:// by doSchemeFlip

	dlMu   sync.Mutex
	dlURLs map[string]downloadURLEntry // fid -> signed download URL (reused by all chunk opens)
}

// NewClient creates a Quark API client. httpClient carries the API and OSS
// upload traffic and may negotiate HTTP/2. downloadClient is dedicated to the
// ranged CDN downloads and must run HTTP/1.1 (quark's edge resets long HTTP/2
// streams on the download hosts; OpenList's downloader is HTTP/1.1-only too).
func NewClient(ctx context.Context, httpClient, downloadClient *http.Client, apiBase, referer, pr, cookie string) *Client {
	return &Client{
		http:     httpClient,
		download: downloadClient,
		api:      apiBase,
		referer:  referer,
		pr:       pr,
		cookie:   cookie,
		pacer: fs.NewPacer(ctx, pacer.NewDefault(
			pacer.MinSleep(defaultMinSleep),
			pacer.MaxSleep(defaultMaxSleep),
			pacer.DecayConstant(defaultDecay),
		)),
	}
}

// Cookie returns the current cookie value (for persisting rotations)
func (c *Client) Cookie() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cookie
}

// SetCookiePersister registers a callback invoked whenever the stored
// cookie changes, so the Fs can persist the rotated value.
func (c *Client) SetCookiePersister(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.persistCookieFn = fn
}

// SetCookie sets (or replaces) a name=value pair inside a cookie header string.
func SetCookie(cookie, name, value string) string {
	if cookie == "" {
		return name + "=" + value
	}
	parts := strings.Split(cookie, ";")
	found := false
	for i, p := range parts {
		if strings.HasPrefix(strings.TrimSpace(p), name+"=") {
			parts[i] = name + "=" + value
			found = true
			break
		}
	}
	if !found {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, ";")
}

// RotateCookie applies each name=value pair to a cookie header string,
// replacing existing pairs with the same name.
func RotateCookie(cookie string, values map[string]string) string {
	for name, value := range values {
		cookie = SetCookie(cookie, name, value)
	}
	return cookie
}

// rotateCookie updates __puus from a response if present
func (c *Client) rotateCookie(res *http.Response) {
	values := map[string]string{}
	for _, ck := range res.Cookies() {
		if ck.Name == "__puus" && strings.TrimSpace(ck.Value) != "" {
			values["__puus"] = ck.Value
		}
	}
	if len(values) == 0 {
		return
	}
	c.mu.Lock()
	c.cookie = RotateCookie(c.cookie, values)
	fn := c.persistCookieFn
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Do makes a request to the API, decodes the JSON envelope and returns the
// body bytes. out may be nil. A non-zero envelope code is returned as an error.
func (c *Client) Do(ctx context.Context, method, pathname string, params url.Values, body interface{}, out interface{}) (err error) {
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("quark: json marshal: %w", err)
		}
	}

	var res *http.Response
	var resBytes []byte
	err = c.pacer.Call(func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, method, c.api+pathname, bytes.NewReader(payload))
		if err != nil {
			return false, fmt.Errorf("quark: new request: %w", err)
		}
		req.Header.Set("Cookie", c.Cookie())
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Referer", c.referer)
		// quark enforces a per-file download size cap (code 23018) unless the
		// request is signed with the official PC client UA.
		req.Header.Set("User-Agent", UserAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		q := req.URL.Query()
		q.Set("pr", c.pr)
		q.Set("fr", "pc")
		for k, vs := range params {
			for _, v := range vs {
				q.Set(k, v)
			}
		}
		req.URL.RawQuery = q.Encode()

		res, err = c.http.Do(req)
		if err != nil {
			return fserrors.ShouldRetry(err), fmt.Errorf("quark: do request: %w", err)
		}
		defer func() {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
		}()

		c.rotateCookie(res)

		if fserrors.ShouldRetryHTTP(res, retryErrorCodes) {
			return true, fmt.Errorf("quark: got HTTP %d", res.StatusCode)
		}
		resBytes, err = io.ReadAll(io.LimitReader(res.Body, bodyLimit))
		if err != nil {
			return false, fmt.Errorf("quark: read response: %w", err)
		}
		return false, nil
	})
	if err != nil {
		return err
	}

	// resp is the generic envelope: error if the code is non-zero
	var resp Resp
	if err := json.Unmarshal(resBytes, &resp); err != nil {
		return fmt.Errorf("quark: decode response: %w", err)
	}
	if resp.Status >= 400 || resp.Code != 0 {
		return fmt.Errorf("quark: %s (status %d, code %d)", resp.Message, resp.Status, resp.Code)
	}
	if out != nil {
		if err := json.Unmarshal(resBytes, out); err != nil {
			return fmt.Errorf("quark: decode %q: %w", pathname, err)
		}
	}
	return nil
}

// pdsHostSuffix is the quark object-storage transfer subnet (upload ul-* and
// download dl-* subdomains). The PC download CDN signs URLs on dl-* under
// drive.quark.cn itself. Only these transfer endpoints may answer plain HTTP
// on an HTTPS dial (a broken TLS-proxying middlebox); the API host
// drive.quark.cn always stays HTTPS.
const pdsHostSuffix = ".pds.quark.cn"

// isPDSHost reports whether host is a quark transfer endpoint: the OSS
// subnets (ul-*.pds.quark.cn, dl-*.pds.quark.cn) or the PC download CDN
// (dl-*.drive.quark.cn). The bare API host and the never-sent drive-pc
// sibling are left out so the API traffic is never downgraded.
func isPDSHost(host string) bool {
	return strings.HasSuffix(host, pdsHostSuffix) ||
		(strings.HasPrefix(host, "dl-") && strings.HasSuffix(host, ".drive.quark.cn"))
}

// isPlainHTTPToHTTPS reports whether the transport dialed TLS and got a plain
// HTTP response. net/http exposes no sentinel for it, so the stable error text
// is matched.
func isPlainHTTPToHTTPS(err error) bool {
	return err != nil && strings.Contains(err.Error(), "server gave HTTP response to HTTPS client")
}

// doSchemeFlip runs req on do and, when the TLS dial answered with a plain
// HTTP response on a PDS transfer host, retries the same request once over
// http://. reset repositions a consumed re-readable body for the retry; it is
// only used when the request carries no GetBody snapshot. A host flipped once
// is dialed over http:// directly from then on, so segmented downloads do not
// waste a failed TLS dial on every range. Downloads pass c.download and OSS
// uploads pass c.http so each path uses its own HTTP version.
func (c *Client) doSchemeFlip(do *http.Client, req *http.Request, reset func()) (*http.Response, error) {
	if isPDSHost(req.URL.Hostname()) && c.httpMemo(req.URL.Hostname()) {
		return c.runOverHTTP(do, req, nil)
	}
	res, err := do.Do(req)
	if err == nil || !isPlainHTTPToHTTPS(err) || !isPDSHost(req.URL.Hostname()) {
		return res, err
	}
	c.schemeWarnOnce.Do(func() {
		fs.LogLevelPrintf(fs.LogLevelWarning, "quark",
			"HTTPS to %s answered plain HTTP; retrying transfer over http:// for the rest of this run (a proxy/VPN is terminating TLS to quark's storage hosts; the signed OSS auth binds the path, not the scheme)", req.URL.Host)
	})
	c.httpMemoSet(req.URL.Hostname())
	return c.runOverHTTP(do, req, reset)
}

// runOverHTTP retries the same request over http://.
func (c *Client) runOverHTTP(do *http.Client, req *http.Request, reset func()) (*http.Response, error) {
	if reset != nil {
		reset()
	}
	u := *req.URL
	u.Scheme = "http"
	retry := req.Clone(req.Context())
	retry.URL = &u
	if retry.GetBody != nil {
		body, err := retry.GetBody()
		if err != nil {
			return nil, err
		}
		retry.Body = body
	}
	return do.Do(retry)
}

// httpMemo reports whether host was previously flipped to plain http.
func (c *Client) httpMemo(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.httpHosts[host]
	return ok
}

// httpMemoSet records that host must be dialed over http:// from now on.
func (c *Client) httpMemoSet(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.httpHosts == nil {
		c.httpHosts = map[string]struct{}{}
	}
	c.httpHosts[host] = struct{}{}
}

// plainGet performs a GET outside the API envelope (used for downloads).
// The response body is left open for the caller to consume.
func (c *Client) plainGet(ctx context.Context, downloadURL string, extraHeaders http.Header) (*http.Response, error) {
	var res *http.Response
	err := c.pacer.Call(func() (bool, error) {
		var err error
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		if err != nil {
			return false, fmt.Errorf("quark: new download request: %w", err)
		}
		req.Header.Set("Cookie", c.Cookie())
		req.Header.Set("Referer", c.referer)
		req.Header.Set("User-Agent", UserAgent)
		for k, vs := range extraHeaders {
			for _, v := range vs {
				req.Header.Set(k, v)
			}
		}
		res, err = c.doSchemeFlip(c.download, req, nil)
		if err != nil {
			return fserrors.ShouldRetry(err), fmt.Errorf("quark: do download: %w", err)
		}
		c.rotateCookie(res)
		if fserrors.ShouldRetryHTTP(res, retryErrorCodes) {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			return true, fmt.Errorf("quark: download got HTTP %d", res.StatusCode)
		}
		return false, nil
	})
	return res, err
}

// Download fetches a download URL with the given extra headers, leaving the
// response body open.
func (c *Client) Download(ctx context.Context, downloadURL string, extraHeaders http.Header) (*http.Response, error) {
	return c.plainGet(ctx, downloadURL, extraHeaders)
}

// GetFiles lists the children of a directory fid, following pagination
func (c *Client) GetFiles(ctx context.Context, parent string) ([]File, error) {
	files := make([]File, 0)
	page := 1
	size := 100
	for {
		params := url.Values{}
		params.Set("pdir_fid", parent)
		params.Set("_size", fmt.Sprintf("%d", size))
		params.Set("_page", fmt.Sprintf("%d", page))
		params.Set("_fetch_total", "1")
		params.Set("fetch_all_file", "1")
		params.Set("fetch_risk_file_name", "1")
		var resp SortResp
		if err := c.Do(ctx, http.MethodGet, "/file/sort", params, nil, &resp); err != nil {
			return nil, err
		}
		for i := range resp.Data.List {
			resp.Data.List[i].FileName = html.UnescapeString(resp.Data.List[i].FileName)
		}
		files = append(files, resp.Data.List...)
		if page*size >= resp.Metadata.Total {
			break
		}
		page++
	}
	return files, nil
}

// DownloadURL returns a signed download URL for a fid, reusing any still-valid
// cached URL so the many ranged GETs rclone issues for one object share a
// single signature. A shared URL matters for throughput: quark's CDN throttles
// fresh URLs until they have been used a few times.
func (c *Client) DownloadURL(ctx context.Context, fid string) (string, error) {
	c.dlMu.Lock()
	if e, ok := c.dlURLs[fid]; ok && time.Now().Before(e.expires) {
		url := e.url
		c.dlMu.Unlock()
		return url, nil
	}
	c.dlMu.Unlock()
	url, err := c.signDownloadURL(ctx, fid)
	if err != nil {
		return "", err
	}
	c.dlMu.Lock()
	if c.dlURLs == nil {
		c.dlURLs = map[string]downloadURLEntry{}
	}
	c.dlURLs[fid] = downloadURLEntry{url: url, expires: time.Now().Add(downloadURLTTL)}
	c.dlMu.Unlock()
	return url, nil
}

// DropDownloadURL invalidates a cached signed URL so the next request signs a
// fresh one. Callers drop the URL when the CDN rejects it (4xx on expired or
// cold signatures) instead of failing the transfer.
func (c *Client) DropDownloadURL(fid string) {
	c.dlMu.Lock()
	delete(c.dlURLs, fid)
	c.dlMu.Unlock()
}

// signDownloadURL asks quark for a fresh signed download URL for a fid.
func (c *Client) signDownloadURL(ctx context.Context, fid string) (string, error) {
	body := map[string]interface{}{"fids": []string{fid}}
	var resp DownResp
	if err := c.Do(ctx, http.MethodPost, "/file/download", nil, body, &resp); err != nil {
		return "", err
	}
	if len(resp.Data) == 0 || resp.Data[0].DownloadUrl == "" {
		return "", fmt.Errorf("quark: no download url for fid %q", fid)
	}
	return resp.Data[0].DownloadUrl, nil
}

// MakeDir creates a directory and returns its fid
func (c *Client) MakeDir(ctx context.Context, name, parent string) (string, error) {
	body := map[string]interface{}{
		"dir_init_lock": false,
		"dir_name":      "",
		"file_name":     name,
		"pdir_fid":      parent,
	}
	var resp CreateResp
	if err := c.Do(ctx, http.MethodPost, "/file", nil, body, &resp); err != nil {
		return "", err
	}
	if resp.Data.Fid == "" {
		return "", fmt.Errorf("quark: create dir %q returned empty fid", name)
	}
	return resp.Data.Fid, nil
}

// Delete removes a fid (moves it to trash)
func (c *Client) Delete(ctx context.Context, fid string) error {
	body := map[string]interface{}{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{fid},
	}
	return c.Do(ctx, http.MethodPost, "/file/delete", nil, body, nil)
}

// Move moves a fid into another directory
func (c *Client) Move(ctx context.Context, fid, toDirFid string) error {
	body := map[string]interface{}{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{fid},
		"to_pdir_fid":  toDirFid,
	}
	return c.Do(ctx, http.MethodPost, "/file/move", nil, body, nil)
}

// Rename renames a fid
func (c *Client) Rename(ctx context.Context, fid, name string) error {
	body := map[string]interface{}{
		"fid":       fid,
		"file_name": name,
	}
	return c.Do(ctx, http.MethodPost, "/file/rename", nil, body, nil)
}

// Member returns account quota information
func (c *Client) Member(ctx context.Context) (*MemberResp, error) {
	params := url.Values{}
	params.Set("fetch_subscribe", "false")
	params.Set("_ch", "home")
	params.Set("fetch_identity", "false")
	var resp MemberResp
	if err := c.Do(ctx, http.MethodGet, "/member", params, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UploadPre starts an upload task
func (c *Client) UploadPre(ctx context.Context, name string, parentFid string, size int64, formatType string) (UpPreResp, error) {
	now := time.Now()
	body := map[string]interface{}{
		"ccp_hash_update": true,
		"dir_name":        "",
		"file_name":       name,
		"format_type":     formatType,
		"l_created_at":    now.UnixMilli(),
		"l_updated_at":    now.UnixMilli(),
		"pdir_fid":        parentFid,
		"size":            size,
	}
	var resp UpPreResp
	err := c.Do(ctx, http.MethodPost, "/file/upload/pre", nil, body, &resp)
	return resp, err
}

// UploadAuth exchanges an auth_meta string for an OSS signature
func (c *Client) UploadAuth(ctx context.Context, pre UpPreResp, authMeta string) (string, error) {
	body := map[string]interface{}{
		"auth_info": pre.Data.AuthInfo,
		"auth_meta": authMeta,
		"task_id":   pre.Data.TaskId,
	}
	var resp UpAuthResp
	if err := c.Do(ctx, http.MethodPost, "/file/upload/auth", nil, body, &resp); err != nil {
		return "", err
	}
	return resp.Data.AuthKey, nil
}

// uploadURL builds the OSS endpoint for the given bucket/object. The API
// advertises the transfer host as http:// but the object-storage edge serves
// proper TLS, so the scheme is pinned to https; doSchemeFlip downgrades to
// http only when a broken TLS path answers plain HTTP on the https dial.
func (c *Client) uploadURL(pre UpPreResp) string {
	const sep = "://"
	host := pre.Data.UploadUrl
	if i := strings.Index(host, sep); i >= 0 {
		host = host[i+len(sep):]
	}
	return fmt.Sprintf("https://%s.%s/%s", pre.Data.Bucket, host, pre.Data.ObjKey)
}

// UploadPart uploads a single part to the OSS endpoint and returns its etag.
// size must be the exact number of bytes in body so OSS never receives a
// chunked request.
func (c *Client) UploadPart(ctx context.Context, pre UpPreResp, mimeType string, partNumber int, size int64, body io.Reader) (string, error) {
	timeStr := time.Now().UTC().Format(http.TimeFormat)
	authMeta := fmt.Sprintf(`PUT

%s
%s
x-oss-date:%s
x-oss-user-agent:aliyun-sdk-js/6.6.1 Chrome 98.0.4758.80 on Windows 10 64-bit
/%s/%s?partNumber=%d&uploadId=%s`,
		mimeType, timeStr, timeStr, pre.Data.Bucket, pre.Data.ObjKey, partNumber, pre.Data.UploadId)
	authKey, err := c.UploadAuth(ctx, pre, authMeta)
	if err != nil {
		return "", fmt.Errorf("quark: part %d auth: %w", partNumber, err)
	}
	u := c.uploadURL(pre)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, body)
	if err != nil {
		return "", fmt.Errorf("quark: part %d new request: %w", partNumber, err)
	}
	req.ContentLength = size
	req.Header.Set("Authorization", authKey)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("Referer", OSSReferer)
	req.Header.Set("x-oss-date", timeStr)
	req.Header.Set("x-oss-user-agent", OSSUA)
	q := req.URL.Query()
	q.Add("partNumber", strconv.Itoa(partNumber))
	q.Add("uploadId", pre.Data.UploadId)
	req.URL.RawQuery = q.Encode()
	res, err := c.doSchemeFlip(c.http, req, func() {
		if s, ok := body.(io.Seeker); ok {
			_, _ = s.Seek(0, io.SeekStart)
		}
	})
	if err != nil {
		return "", fmt.Errorf("quark: part %d upload: %w", partNumber, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	if res.StatusCode != 200 {
		respBody, _ := io.ReadAll(io.LimitReader(res.Body, bodyLimit))
		return "", fmt.Errorf("quark: part %d upload status: %d, error: %s", partNumber, res.StatusCode, string(respBody))
	}
	return res.Header.Get("Etag"), nil
}

// UploadCommit completes the multipart upload against the OSS endpoint
func (c *Client) UploadCommit(ctx context.Context, pre UpPreResp, etags []string) error {
	timeStr := time.Now().UTC().Format(http.TimeFormat)
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUpload>
`)
	for i, etag := range etags {
		sb.WriteString(fmt.Sprintf(`<Part>
<PartNumber>%d</PartNumber>
<ETag>%s</ETag>
</Part>
`, i+1, etag))
	}
	sb.WriteString("</CompleteMultipartUpload>")
	body := sb.String()
	h := md5.New()
	_, _ = h.Write([]byte(body))
	contentMd5 := base64.StdEncoding.EncodeToString(h.Sum(nil))
	callbackBytes, err := json.Marshal(pre.Data.Callback)
	if err != nil {
		return fmt.Errorf("quark: marshal callback: %w", err)
	}
	callbackBase64 := base64.StdEncoding.EncodeToString(callbackBytes)
	authMeta := fmt.Sprintf(`POST
%s
application/xml
%s
x-oss-callback:%s
x-oss-date:%s
x-oss-user-agent:aliyun-sdk-js/6.6.1 Chrome 98.0.4758.80 on Windows 10 64-bit
/%s/%s?uploadId=%s`,
		contentMd5, timeStr, callbackBase64, timeStr,
		pre.Data.Bucket, pre.Data.ObjKey, pre.Data.UploadId)
	authKey, err := c.UploadAuth(ctx, pre, authMeta)
	if err != nil {
		return fmt.Errorf("quark: commit auth: %w", err)
	}
	u := c.uploadURL(pre)
	commitBody := strings.NewReader(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, commitBody)
	if err != nil {
		return fmt.Errorf("quark: commit new request: %w", err)
	}
	req.Header.Set("Authorization", authKey)
	req.Header.Set("Content-MD5", contentMd5)
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Referer", OSSReferer)
	req.Header.Set("x-oss-callback", callbackBase64)
	req.Header.Set("x-oss-date", timeStr)
	req.Header.Set("x-oss-user-agent", OSSUA)
	q := req.URL.Query()
	q.Add("uploadId", pre.Data.UploadId)
	req.URL.RawQuery = q.Encode()
	res, err := c.doSchemeFlip(c.http, req, func() {
		_, _ = commitBody.Seek(0, io.SeekStart)
	})
	if err != nil {
		return fmt.Errorf("quark: commit upload: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	if res.StatusCode != 200 {
		respBody, _ := io.ReadAll(io.LimitReader(res.Body, bodyLimit))
		return fmt.Errorf("quark: commit status: %d, error: %s", res.StatusCode, string(respBody))
	}
	return nil
}

// UploadFinish completes an upload task
func (c *Client) UploadFinish(ctx context.Context, pre UpPreResp) error {
	body := map[string]interface{}{
		"obj_key": pre.Data.ObjKey,
		"task_id": pre.Data.TaskId,
	}
	return c.Do(ctx, http.MethodPost, "/file/upload/finish", nil, body, nil)
}

// UploadHash registers the content hashes for an upload task with the server.
// It returns finish=true when the file already exists server-side, in which
// case the entry is created instantly and no further upload is needed.
func (c *Client) UploadHash(ctx context.Context, pre UpPreResp, md5, sha1 string) (bool, error) {
	body := map[string]any{
		"md5":     md5,
		"sha1":    sha1,
		"task_id": pre.Data.TaskId,
	}
	var resp HashResp
	if err := c.Do(ctx, http.MethodPost, "/file/update/hash", nil, body, &resp); err != nil {
		return false, fmt.Errorf("quark: upload hash: %w", err)
	}
	return resp.Data.Finish, nil
}

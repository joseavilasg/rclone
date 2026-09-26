package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/pacer"
	"golang.org/x/sync/singleflight"
)

const (
	defaultMinSleep = 10 * time.Millisecond
	defaultMaxSleep = 2 * time.Second
	defaultDecay    = 2
)

// bodyLimit is the amount to read from the API response body
const bodyLimit = 10 * 1024 * 1024

// defaultUploadSliceTimeout is the per-slice upload timeout in seconds when
// the option is not set.
const defaultUploadSliceTimeout = 60

// uploadLocateAPI is the default locateupload endpoint that resolves the
// dynamic upload domain for a path+uploadid pair.
const uploadLocateAPI = "https://d.pcs.baidu.com"

// downloadURLTTL is how long a resolved download URL is cached. The official
// dlink expires after about one hour; crack dlinks have no documented expiry
// but are cached conservatively. Keeping one URL per file lets the many ranged
// GETs of a multi-threaded copy share a single signature.
const downloadURLTTL = 50 * time.Minute

// maxSpanTruncations is how many consecutive truncated spans a cached
// download URL survives before it is dropped as burned and resolved fresh.
// A single short body can be network weather; three in a row on the same URL
// means the signature is being killed.
const maxSpanTruncations = 3

// downloadURLEntry is a resolved download URL with its expiry.
type downloadURLEntry struct {
	url     string
	expires time.Time
}

// errno values returned by the Baidu Netdisk API
const (
	tokenInvalidErrno = 111   // access token invalid
	tokenExpiredErrno = -6    // access token expired
	mediaInfoErrno    = 31023 // mediainfo wraps its data in this errno for crack_video
)

// InvalidTokenErrno reports whether an errno is one of the access-token
// invalid markers that force a token refresh and a retry.
func InvalidTokenErrno(errno int) bool {
	return errno == tokenInvalidErrno || errno == tokenExpiredErrno
}

// errorInfo is the token error body from Baidu's OAuth endpoint.
type errorInfo struct {
	ErrorDescription string `json:"error_description"`
	Error            string `json:"error"`
}

// Config carries the options needed by the client: the refresh token and how
// to mint fresh access tokens from it.
type Config struct {
	RefreshToken       string // required
	UseOnlineAPI       bool
	APIAddress         string // oplist refresh proxy used when UseOnlineAPI is true
	ClientID           string // required when UseOnlineAPI is false
	ClientSecret       string // required when UseOnlineAPI is false
	DownloadAPI        string // "official", "crack" or "crack_video"
	CustomCrackUA      string // UA stamped on crack/crack_video downloads
	UploadSliceTimeout int    // per-slice upload timeout in seconds
	LocateBase         string // locateupload base URL (tests override the default)
}

// Client is the Baidu Netdisk API client. httpClient carries the API traffic;
// downloadClient carries the CDN downloads and is stamped with the driver's
// User-Agent by the Fs through a transport request filter. headClient splits
// the official download chase: the Location of the *unfollowed* redirect is
// the actual CDN URL, so that path must never let the transport follow it.
type Client struct {
	http           *http.Client
	download       *http.Client
	headNoRedirect *http.Client
	api            string // base API URL (https://pan.baidu.com)
	locateBase     string // locateupload base URL (https://d.pcs.baidu.com)
	config         Config
	pacer          *fs.Pacer

	mu             sync.Mutex
	accessToken    string
	refreshToken   string
	persistTokenFn func(refreshToken string)

	dlMu   sync.Mutex
	dlURLs map[string]downloadURLEntry // fs_id -> resolved download URL
	// dlFails counts consecutive truncated spans per fs_id. At
	// maxSpanTruncations the URL is dropped as burned and resolved fresh.
	dlFails map[string]int
	// dlFlight collapses concurrent resolutions for one file into a single
	// signature: the chunks of a multi-thread copy start together and must
	// not pay one filemetas call each.
	dlFlight singleflight.Group

	vipOnce sync.Once
	vipType int
	vipErr  error
}

// NewClient creates a Baidu Netdisk API client. apiBase overrides the default
// https://pan.baidu.com endpoint (used by tests). cfg.DownloadAPI selects the
// download link resolution; the other fields drive the token minting.
func NewClient(ctx context.Context, httpClient, downloadClient *http.Client, apiBase string, cfg Config) *Client {
	if apiBase == "" {
		apiBase = "https://pan.baidu.com"
	}
	locateBase := cfg.LocateBase
	if locateBase == "" {
		locateBase = uploadLocateAPI
	}
	return &Client{
		http:     httpClient,
		download: downloadClient,
		// The redirect chase shares the download transport (same UA stamping)
		// but never follows redirects.
		headNoRedirect: &http.Client{
			Transport:     downloadClient.Transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
		},
		api:          apiBase,
		locateBase:   locateBase,
		config:       cfg,
		refreshToken: cfg.RefreshToken,
		pacer: fs.NewPacer(ctx, pacer.NewDefault(
			pacer.MinSleep(defaultMinSleep),
			pacer.MaxSleep(defaultMaxSleep),
			pacer.DecayConstant(defaultDecay),
		)),
	}
}

// SetTokenPersister registers a callback invoked whenever the stored
// refresh_token changes, so the Fs can persist the rotated value.
func (c *Client) SetTokenPersister(fn func(refreshToken string)) {
	c.mu.Lock()
	c.persistTokenFn = fn
	c.mu.Unlock()
}

// AccessToken returns the current access token.
func (c *Client) AccessToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accessToken
}

// RefreshToken returns the current refresh token (for persisting rotations).
func (c *Client) RefreshToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshToken
}

// persistToken notifies the registered callback (outside the lock) of a new
// refresh token so the Fs can write it back into its config.
func (c *Client) persistToken() {
	c.mu.Lock()
	fn := c.persistTokenFn
	token := c.refreshToken
	c.mu.Unlock()
	if fn != nil {
		fn(token)
	}
}

// RefreshToken mints a fresh access token from the refresh token. When
// UseOnlineAPI is set the oplist proxy is asked (no client credentials
// needed); otherwise the direct OAuth endpoint is used. Both paths persist the
// rotated refresh_token through the registered callback.
func (c *Client) refreshAccessToken(ctx context.Context) error {
	var err error
	if c.config.UseOnlineAPI {
		if strings.TrimSpace(c.config.APIAddress) == "" {
			return fmt.Errorf("baidunetdisk: empty api_url_address for online token refresh")
		}
		err = c.refreshTokenOnline(ctx)
	} else if c.config.ClientID == "" || c.config.ClientSecret == "" {
		return fmt.Errorf("baidunetdisk: empty client_id or client_secret for direct token refresh")
	} else {
		err = c.refreshTokenOAuth(ctx)
	}
	if err != nil {
		return err
	}
	c.persistToken()
	return nil
}

// refreshTokenOnline mints a fresh token pair through the oplist refresh proxy.
func (c *Client) refreshTokenOnline(ctx context.Context) error {
	params := url.Values{}
	params.Set("refresh_ui", c.refreshToken)
	params.Set("server_use", "true")
	params.Set("driver_txt", "baiduyun_go")
	var resp struct {
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
		ErrorText    string `json:"text"`
	}
	if err := c.fetchJSON(ctx, http.MethodGet, c.config.APIAddress+"?"+params.Encode(), nil, &resp); err != nil {
		return fmt.Errorf("baidunetdisk: refresh token (online API): %w", err)
	}
	if resp.RefreshToken == "" || resp.AccessToken == "" {
		if resp.ErrorText != "" {
			return fmt.Errorf("baidunetdisk: refresh token: %s", resp.ErrorText)
		}
		return fmt.Errorf("baidunetdisk: refresh token: empty token returned from online API, a wrong refresh token may have been used")
	}
	c.mu.Lock()
	c.accessToken = resp.AccessToken
	c.refreshToken = resp.RefreshToken
	c.mu.Unlock()
	return nil
}

// refreshTokenOAuth mints a fresh token pair from Baidu's OAuth endpoint.
func (c *Client) refreshTokenOAuth(ctx context.Context) error {
	params := url.Values{}
	params.Set("grant_type", "refresh_token")
	params.Set("refresh_token", c.refreshToken)
	params.Set("client_id", c.config.ClientID)
	params.Set("client_secret", c.config.ClientSecret)
	u := "https://openapi.baidu.com/oauth/2.0/token?" + params.Encode()
	var resp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	var e errorInfo
	if err := c.fetchJSON(ctx, http.MethodGet, u, &e, &resp); err != nil {
		return fmt.Errorf("baidunetdisk: refresh token (oauth): %w", err)
	}
	if e.Error != "" {
		return fmt.Errorf("baidunetdisk: refresh token: %s: %s", e.Error, e.ErrorDescription)
	}
	if resp.RefreshToken == "" {
		return fmt.Errorf("baidunetdisk: refresh token: empty token returned from OAuth, a wrong refresh token may have been used")
	}
	c.mu.Lock()
	c.accessToken = resp.AccessToken
	c.refreshToken = resp.RefreshToken
	c.mu.Unlock()
	return nil
}

// fetchJSON performs a raw GET that does not carry an access token nor errno
// handling (used by the token refresh paths). An OAuth-style error body is
// decoded into errOut when it can be.
func (c *Client) fetchJSON(ctx context.Context, method, furl string, errOut, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, furl, nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("http status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, bodyLimit))
	if err != nil {
		return err
	}
	if errOut != nil {
		_ = json.Unmarshal(body, errOut) // error body is optional
	}
	return json.Unmarshal(body, out)
}

// request performs an API request against furl (already the full URL), retrying
// up to three times with exponential backoff. When errno is an access-token
// marker the token is refreshed and the request retried. errno 31023 with
// download_api=crack_video is not an error: the mediainfo endpoint wraps its
// payload in that errno. out, when non-nil, receives the decoded JSON body.
func (c *Client) request(ctx context.Context, method, furl string, params, form url.Values, out any) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		body, err := c.roundTrip(ctx, method, furl, params, form)
		if err != nil {
			lastErr = err
			continue
		}
		var envelope struct {
			Errno int `json:"errno"`
		}
		// Not every body is JSON; a non-JSON body decodes to errno 0, which
		// matches the OpenList driver reading errno with a default of 0.
		_ = json.Unmarshal(body, &envelope)
		if InvalidTokenErrno(envelope.Errno) {
			if err := c.refreshAccessToken(ctx); err != nil {
				return nil, err
			}
			lastErr = fmt.Errorf("baidunetdisk: errno %d: token refreshed, retrying", envelope.Errno)
			continue
		}
		if envelope.Errno != 0 {
			if envelope.Errno == mediaInfoErrno && c.config.DownloadAPI == "crack_video" {
				return body, nil // passthrough: mediainfo wraps its data in 31023
			}
			return nil, fmt.Errorf("baidunetdisk: %s: errno %d (see https://pan.baidu.com/union/doc/)", furl, envelope.Errno)
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return nil, fmt.Errorf("baidunetdisk: decode %s: %w", furl, err)
			}
		}
		return body, nil
	}
	return nil, lastErr
}

// roundTrip builds and executes one HTTP request, attaching the access token
// and returning the response body.
func (c *Client) roundTrip(ctx context.Context, method, furl string, params, form url.Values) ([]byte, error) {
	u, err := url.Parse(furl)
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: parse url: %w", err)
	}
	q := u.Query()
	q.Add("access_token", c.AccessToken())
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: new request: %w", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: do request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	respBody, err := io.ReadAll(io.LimitReader(res.Body, bodyLimit))
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: read response: %w", err)
	}
	return respBody, nil
}

// get performs a GET on a /rest/2.0 API path.
func (c *Client) get(ctx context.Context, pathname string, params url.Values, out any) ([]byte, error) {
	return c.request(ctx, http.MethodGet, c.api+"/rest/2.0"+pathname, params, nil, out)
}

// postForm performs a POST on a /rest/2.0 API path with a urlencoded body.
func (c *Client) postForm(ctx context.Context, pathname string, params, form url.Values, out any) ([]byte, error) {
	return c.request(ctx, http.MethodPost, c.api+"/rest/2.0"+pathname, params, form, out)
}

// GetFiles lists the children of an absolute directory path (the root is "/"),
// following pagination with limit 1000 until a short page comes back.
func (c *Client) GetFiles(ctx context.Context, dir string) ([]File, error) {
	params := url.Values{}
	params.Set("method", "list")
	params.Set("dir", dir)
	params.Set("web", "web")
	start := 0
	const limit = 1000
	files := make([]File, 0)
	for {
		params.Set("start", strconv.Itoa(start))
		params.Set("limit", strconv.Itoa(limit))
		var resp ListResp
		if _, err := c.get(ctx, "/xpan/file", params, &resp); err != nil {
			return nil, err
		}
		if len(resp.List) == 0 {
			break
		}
		files = append(files, resp.List...)
		if len(resp.List) < limit {
			break
		}
		start += limit
	}
	return files, nil
}

// DownloadURL returns a download URL for a file, reusing any still-valid
// cached URL so the many ranged GETs rclone issues for one object share a
// single signature. Concurrent resolutions for one file collapse into a
// single flight. The URL is resolved through the configured download API.
func (c *Client) DownloadURL(ctx context.Context, f File) (string, error) {
	key := f.ID()
	c.dlMu.Lock()
	if e, ok := c.dlURLs[key]; ok && time.Now().Before(e.expires) {
		url := e.url
		c.dlMu.Unlock()
		fs.Debugf(nil, "baidunetdisk: reusing cached download URL for %q", f.Path)
		return url, nil
	}
	c.dlMu.Unlock()
	v, err, _ := c.dlFlight.Do(key, func() (any, error) {
		// Re-check under the shared flight: a sibling may have resolved
		// while this call queued.
		c.dlMu.Lock()
		if e, ok := c.dlURLs[key]; ok && time.Now().Before(e.expires) {
			url := e.url
			c.dlMu.Unlock()
			return url, nil
		}
		c.dlMu.Unlock()
		// Logged inside the flight so the line counts real signatures, not
		// callers queueing on the same file.
		fs.Debugf(nil, "baidunetdisk: resolving fresh download URL for %q via %s", f.Path, c.config.DownloadAPI)
		url, err := c.resolveDownloadURL(ctx, f)
		if err != nil {
			return "", err
		}
		c.dlMu.Lock()
		if c.dlURLs == nil {
			c.dlURLs = map[string]downloadURLEntry{}
		}
		c.dlURLs[key] = downloadURLEntry{url: url, expires: time.Now().Add(downloadURLTTL)}
		c.dlMu.Unlock()
		return url, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// DropDownloadURL invalidates a cached download URL so the next request
// resolves a fresh one. Callers drop the URL when the CDN rejects it (4xx on
// expired or cold signatures) instead of failing the transfer.
func (c *Client) DropDownloadURL(f File) {
	c.dlMu.Lock()
	delete(c.dlURLs, f.ID())
	delete(c.dlFails, f.ID())
	c.dlMu.Unlock()
	fs.Debugf(nil, "baidunetdisk: dropped download URL for %q, next open re-signs", f.Path)
}

// NoteSpanTruncation records a span that ended short of its requested bytes.
// Consecutive truncations on the same cached URL mean the signature is being
// killed: at maxSpanTruncations the URL is dropped so the next open resolves
// a fresh one. It reports whether the URL was dropped.
func (c *Client) NoteSpanTruncation(f File) bool {
	key := f.ID()
	c.dlMu.Lock()
	if c.dlFails == nil {
		c.dlFails = map[string]int{}
	}
	c.dlFails[key]++
	n := c.dlFails[key]
	c.dlMu.Unlock()
	if n >= maxSpanTruncations {
		fs.Debugf(nil, "baidunetdisk: %d consecutive truncated spans for %q, dropping burned download URL", n, f.Path)
		c.DropDownloadURL(f)
		return true
	}
	fs.Debugf(nil, "baidunetdisk: truncated span for %q (%d/%d)", f.Path, n, maxSpanTruncations)
	return false
}

// NoteSpanComplete records a fully delivered span, clearing any truncation
// streak on its URL.
func (c *Client) NoteSpanComplete(f File) {
	key := f.ID()
	c.dlMu.Lock()
	n := c.dlFails[key]
	delete(c.dlFails, key)
	c.dlMu.Unlock()
	if n > 0 {
		fs.Debugf(nil, "baidunetdisk: full span for %q after %d truncations, streak cleared", f.Path, n)
	}
}

// resolveDownloadURL resolves a fresh download URL following the configured
// download API.
func (c *Client) resolveDownloadURL(ctx context.Context, f File) (string, error) {
	switch c.config.DownloadAPI {
	case "crack":
		return c.linkCrack(ctx, f)
	case "crack_video":
		return c.linkCrackVideo(ctx, f)
	default: // official
		return c.linkOfficial(ctx, f)
	}
}

// linkOfficial resolves the download URL through the official multimedia
// endpoint: dlink + access_token, then a no-redirect HEAD to chase the CDN
// redirect to the final location. The official link expires after about one
// hour.
func (c *Client) linkOfficial(ctx context.Context, f File) (string, error) {
	params := url.Values{}
	params.Set("method", "filemetas")
	params.Set("fsids", fmt.Sprintf("[%s]", f.ID()))
	params.Set("dlink", "1")
	var resp DownloadResp
	if _, err := c.get(ctx, "/xpan/multimedia", params, &resp); err != nil {
		return "", err
	}
	if len(resp.List) == 0 || resp.List[0].Dlink == "" {
		return "", fmt.Errorf("baidunetdisk: no download url for fs_id %q", f.ID())
	}
	u := fmt.Sprintf("%s&access_token=%s", resp.List[0].Dlink, c.AccessToken())
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return "", fmt.Errorf("baidunetdisk: new head request: %w", err)
	}
	res, err := c.headNoRedirect.Do(req)
	if err != nil {
		return "", fmt.Errorf("baidunetdisk: head download url: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	location := res.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("baidunetdisk: no location from download head for fs_id %q", f.ID())
	}
	return location, nil
}

// linkCrack resolves the download URL through the webdav-style /api/filemetas
// endpoint with the dlna origin, which yields a direct dlink.
func (c *Client) linkCrack(ctx context.Context, f File) (string, error) {
	params := url.Values{}
	params.Set("target", fmt.Sprintf("[\"%s\"]", f.Path))
	params.Set("dlink", "1")
	params.Set("web", "5")
	params.Set("origin", "dlna")
	var resp DownloadResp2
	if _, err := c.request(ctx, http.MethodGet, c.api+"/api/filemetas", params, nil, &resp); err != nil {
		return "", err
	}
	if len(resp.Info) == 0 || resp.Info[0].Dlink == "" {
		return "", fmt.Errorf("baidunetdisk: no download url for path %q", f.Path)
	}
	return resp.Info[0].Dlink, nil
}

// linkCrackVideo resolves the download URL through the /api/mediainfo endpoint
// used by the official players. The payload is wrapped in errno 31023, which
// request() passes through unmodified for this mode.
func (c *Client) linkCrackVideo(ctx context.Context, f File) (string, error) {
	params := url.Values{}
	params.Set("type", "VideoURL")
	params.Set("path", f.Path)
	params.Set("fs_id", f.ID())
	params.Set("devuid", "0%1")
	params.Set("clienttype", "1")
	params.Set("channel", "android_15_25010PN30C_bd-netdisk_1523a")
	params.Set("nom3u8", "1")
	params.Set("dlink", "1")
	params.Set("media", "1")
	params.Set("origin", "dlna")
	body, err := c.request(ctx, http.MethodGet, c.api+"/api/mediainfo", params, nil, nil)
	if err != nil {
		return "", err
	}
	var resp DownloadResp2
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("baidunetdisk: decode mediainfo: %w", err)
	}
	if len(resp.Info) == 0 || resp.Info[0].Dlink == "" {
		return "", fmt.Errorf("baidunetdisk: no download url for path %q", f.Path)
	}
	return resp.Info[0].Dlink, nil
}

// Manage applies a filemanager operation (copy, move, rename, delete) to a
// list of operation records, mirroring the OpenList driver.
func (c *Client) Manage(ctx context.Context, opera string, filelist any) error {
	params := url.Values{}
	params.Set("method", "filemanager")
	params.Set("opera", opera)
	m, err := json.Marshal(filelist)
	if err != nil {
		return fmt.Errorf("baidunetdisk: marshal filelist: %w", err)
	}
	form := url.Values{}
	form.Set("async", "0")
	form.Set("filelist", string(m))
	form.Set("ondup", "fail")
	_, err = c.postForm(ctx, "/xpan/file", params, form, nil)
	return err
}

// CreateDir creates a directory at the given absolute path.
func (c *Client) CreateDir(ctx context.Context, dirPath string) error {
	params := url.Values{}
	params.Set("method", "create")
	form := url.Values{}
	form.Set("path", dirPath)
	form.Set("size", "0")
	form.Set("isdir", "1")
	form.Set("rtype", "3")
	_, err := c.postForm(ctx, "/xpan/file", params, form, nil)
	return err
}

// Quota returns the account's total and used space.
func (c *Client) Quota(ctx context.Context) (total, used int64, err error) {
	var resp QuotaResp
	if _, err := c.request(ctx, http.MethodGet, c.api+"/api/quota", nil, nil, &resp); err != nil {
		return 0, 0, err
	}
	return resp.Total, resp.Used, nil
}

// Download performs a GET on a resolved download URL, leaving the response
// body open. The caller is responsible for closing it.
func (c *Client) Download(ctx context.Context, downloadURL string, extraHeaders http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: new download request: %w", err)
	}
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	res, err := c.download.Do(req)
	if err != nil {
		return nil, fmt.Errorf("baidunetdisk: download: %w", err)
	}
	return res, nil
}

// VipType returns the account tier (0=regular, 1=vip, 2=super vip). The uinfo
// answer is fetched lazily on the first upload and cached, so listings and
// downloads never pay the extra call.
func (c *Client) VipType(ctx context.Context) (int, error) {
	c.vipOnce.Do(func() {
		params := url.Values{}
		params.Set("method", "uinfo")
		var resp struct {
			VipType int `json:"vip_type"`
		}
		if _, err := c.get(ctx, "/xpan/nas", params, &resp); err != nil {
			c.vipErr = err
			return
		}
		c.vipType = resp.VipType
	})
	return c.vipType, c.vipErr
}

// joinTime stamps the local ctime/mtime onto a create-style form. Baidu
// always answers the current time; the payload carries the real file times.
func joinTime(form url.Values, ctime, mtime int64) {
	form.Set("local_mtime", strconv.FormatInt(mtime, 10))
	form.Set("local_ctime", strconv.FormatInt(ctime, 10))
}

// PreCreate runs the pre-upload step: it sends the md5 of every slice plus
// the whole-file content-md5 and the first-256KB slice-md5. Only the first
// precreate of an upload carries the hashes; a re-precreate after an expired
// uploadid passes empty hashes. ReturnType 2 means the server already has the
// content (秒传) and the reply carries the created File directly.
func (c *Client) PreCreate(ctx context.Context, args PreCreateArgs) (*PrecreateResp, error) {
	params := url.Values{}
	params.Set("method", "precreate")
	form := url.Values{}
	form.Set("path", args.Path)
	form.Set("size", strconv.FormatInt(args.Size, 10))
	form.Set("isdir", "0")
	form.Set("autoinit", "1")
	form.Set("rtype", "3")
	form.Set("block_list", args.BlockList)
	// Only the first upload carries the hashes.
	if args.ContentMd5 != "" && args.SliceMd5 != "" {
		form.Set("content-md5", args.ContentMd5)
		form.Set("slice-md5", args.SliceMd5)
	}
	joinTime(form, args.Ctime, args.Mtime)
	var resp PrecreateResp
	if _, err := c.postForm(ctx, "/xpan/file", params, form, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Create finalizes an upload, or performs a rapid create (秒传) when uploadID
// is empty and blockList carries the whole-file md5 as its single entry. The
// reply decodes straight into a File. Both paths stamp the file times because
// Baidu answers the current time instead of the file time.
func (c *Client) Create(ctx context.Context, path string, size int64, uploadID, blockList string, ctime, mtime int64) (*File, error) {
	params := url.Values{}
	params.Set("method", "create")
	form := url.Values{}
	form.Set("path", path)
	form.Set("size", strconv.FormatInt(size, 10))
	form.Set("isdir", "0")
	form.Set("rtype", "3")
	if mtime != 0 && ctime != 0 {
		joinTime(form, ctime, mtime)
	}
	if uploadID != "" {
		form.Set("uploadid", uploadID)
	}
	if blockList != "" {
		form.Set("block_list", blockList)
	}
	var file File
	if _, err := c.postForm(ctx, "/xpan/file", params, form, &file); err != nil {
		return nil, err
	}
	file.Ctime = ctime
	file.Mtime = mtime
	return &file, nil
}

// LocateUpload asks the pcs endpoint for the best upload domain for a
// path+uploadid pair. The first server (or backup server) wins.
func (c *Client) LocateUpload(ctx context.Context, path, uploadID string) (string, error) {
	return c.locateUpload(ctx, path, uploadID)
}

// locateUpload asks the pcs endpoint for the best upload domain for a
// path+uploadid pair. The first server (or backup server) wins.
func (c *Client) locateUpload(ctx context.Context, path, uploadID string) (string, error) {
	params := url.Values{}
	params.Set("method", "locateupload")
	params.Set("appid", "250528")
	params.Set("path", path)
	params.Set("uploadid", uploadID)
	params.Set("upload_version", "2.0")
	var resp UploadServerResp
	if _, err := c.request(ctx, http.MethodGet, c.locateBase+"/rest/2.0/pcs/file", params, nil, &resp); err != nil {
		return "", err
	}
	if len(resp.Servers) > 0 {
		return resp.Servers[0].Server, nil
	}
	if len(resp.BakServers) > 0 {
		return resp.BakServers[0].Server, nil
	}
	return "", fmt.Errorf("baidunetdisk: upload URL is empty")
}

// uploadSliceTimeout returns the configured per-slice upload timeout.
func (c *Client) uploadSliceTimeout() time.Duration {
	if c.config.UploadSliceTimeout > 0 {
		return time.Duration(c.config.UploadSliceTimeout) * time.Second
	}
	return defaultUploadSliceTimeout * time.Second
}

// UploadSlice uploads one slice to {uploadURL}/rest/2.0/pcs/superfile2 as a
// multipart file, mirroring the reference driver: header and footer are
// pre-rendered so the request carries an exact Content-Length. params holds
// method/type/path/uploadid/partseq; the access token is attached here. The
// body is scanned for the uploadid expired/invalid/not-found marker, which
// surfaces as ErrUploadIDExpired so the caller recreates the upload from
// scratch. A single call makes one attempt; the caller retries.
func (c *Client) UploadSlice(ctx context.Context, uploadURL string, params url.Values, fileName string, section io.Reader, size int64) error {
	b := bytes.NewBuffer(make([]byte, 0, bytes.MinRead))
	mw := multipart.NewWriter(b)
	if _, err := mw.CreateFormFile("file", fileName); err != nil {
		return fmt.Errorf("baidunetdisk: multipart form: %w", err)
	}
	headSize := b.Len()
	if err := mw.Close(); err != nil {
		return fmt.Errorf("baidunetdisk: multipart close: %w", err)
	}
	head := bytes.NewReader(b.Bytes()[:headSize])
	tail := bytes.NewReader(b.Bytes()[headSize:])
	body := io.MultiReader(head, section, tail)

	u, err := url.Parse(uploadURL + "/rest/2.0/pcs/superfile2")
	if err != nil {
		return fmt.Errorf("baidunetdisk: parse upload url: %w", err)
	}
	q := u.Query()
	q.Set("access_token", c.AccessToken())
	for k, vs := range params {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()

	sliceCtx, cancel := context.WithTimeout(ctx, c.uploadSliceTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(sliceCtx, http.MethodPost, u.String(), body)
	if err != nil {
		return fmt.Errorf("baidunetdisk: new upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(b.Len()) + size

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("baidunetdisk: upload slice: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	respBody, err := io.ReadAll(io.LimitReader(res.Body, bodyLimit))
	if err != nil {
		return fmt.Errorf("baidunetdisk: read upload response: %w", err)
	}
	lower := strings.ToLower(string(respBody))
	if strings.Contains(lower, "uploadid") &&
		(strings.Contains(lower, "invalid") || strings.Contains(lower, "expired") || strings.Contains(lower, "not found")) {
		return ErrUploadIDExpired
	}
	var envelope struct {
		Errno     int `json:"errno"`
		ErrorCode int `json:"error_code"`
	}
	_ = json.Unmarshal(respBody, &envelope)
	if envelope.Errno != 0 || envelope.ErrorCode != 0 {
		return fmt.Errorf("baidunetdisk: upload slice: %s", string(respBody))
	}
	return nil
}

package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plainHTTPResponder accepts every dial and answers a plain HTTP 200, forcing
// the TLS->plain-HTTP sentinel on any https request (a TLS dial to it sees an
// HTTP response instead of a ServerHello).
type plainHTTPResponder struct {
	mu    sync.Mutex
	conns int
	ln    net.Listener
}

func newPlainHTTPResponder(t *testing.T) *plainHTTPResponder {
	r := &plainHTTPResponder{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := r.ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				buf := make([]byte, 4096)
				_, _ = c.Read(buf) // TLS ClientHello or a plain HTTP request
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			}(c)
		}
	}()
	return r
}

func (r *plainHTTPResponder) dial(context.Context, string, string) (net.Conn, error) {
	r.mu.Lock()
	r.conns++
	r.mu.Unlock()
	return net.Dial("tcp", r.ln.Addr().String())
}

func (r *plainHTTPResponder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns
}

// TestDoSchemeFlipForceDowngrade forces the TLS->plain-HTTP sentinel by wiring
// a synthetic responder that answers every dial with a plain HTTP response.
// It proves doSchemeFlip retries over http:// and that the PDS-only WARN is
// emitted exactly once per client, regardless of how many transfers flip.
func TestDoSchemeFlipForceDowngrade(t *testing.T) {
	r := newPlainHTTPResponder(t)
	c := &Client{download: &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: r.dial},
	}}

	var logBuf bytes.Buffer
	fs.SetLogger(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	oldLevel := fs.GetConfig(context.Background()).LogLevel
	fs.GetConfig(context.Background()).LogLevel = fs.LogLevelDebug
	t.Cleanup(func() {
		fs.SetLogger(slog.NewTextHandler(io.Discard, nil))
		fs.GetConfig(context.Background()).LogLevel = oldLevel
	})

	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodGet, "https://fake.pds.quark.cn/blob", nil)
		require.NoError(t, err)
		res, err := c.doSchemeFlip(c.download, req, nil)
		require.NoError(t, err, "flip #%d", i)
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		res.Body.Close()
		assert.Equal(t, "ok", string(body), "flip #%d body", i)
	}

	// First request: one failed https dial + one http retry. The second
	// request goes straight to http:// (the host is memoized after the first
	// flip), so it costs a single additional dial.
	assert.Equal(t, 3, r.count(), "https dial fails once, http retries succeed; the host is memoized afterwards")

	assert.Equal(t, 1, strings.Count(logBuf.String(), "plain HTTP"),
		"the downgrade WARN must fire exactly once per client")
	assert.True(t, strings.Contains(logBuf.String(), "http://"), logBuf.String())
}

// TestDoSchemeFlipForceDowngradePCDownloadHost is the same force-downgrade
// scenario over the PC download CDN host (dl-pc-*.drive.quark.cn) that the PC
// User-Agent elicits from /file/download. quark signs those URLs under
// drive.quark.cn itself, outside the *.pds.quark.cn subnet, and a non-flipped
// download dies with the TLS sentinel just like the web-issued links did.
func TestDoSchemeFlipForceDowngradePCDownloadHost(t *testing.T) {
	r := newPlainHTTPResponder(t)
	c := &Client{download: &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: r.dial},
	}}

	req, err := http.NewRequest(http.MethodGet, "https://dl-pc-zb.drive.quark.cn/mTX/obj?signature", nil)
	require.NoError(t, err)
	res, err := c.doSchemeFlip(c.download, req, nil)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	res.Body.Close()
	assert.Equal(t, "ok", string(body))
	assert.Equal(t, 2, r.count(), "https dial fails once, http retry succeeds")
}

// TestDoSchemeFlipLeavesAPITrafficAlone proves non-PDS hosts never downgrade:
// the request error must pass through untouched and no WARN may be emitted.
func TestDoSchemeFlipLeavesAPITrafficAlone(t *testing.T) {
	var (
		mu    sync.Mutex
		conns int
	)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close() // no TLS: the handshake dies fast, before any HTTP response
		}
	}()

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		conns++
		mu.Unlock()
		return net.Dial("tcp", ln.Addr().String())
	}
	c := &Client{http: &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DialContext: dial},
	}}

	var logBuf bytes.Buffer
	fs.SetLogger(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { fs.SetLogger(slog.NewTextHandler(io.Discard, nil)) })

	req, err := http.NewRequest(http.MethodGet, "https://drive.quark.cn/1/clouddrive", nil)
	require.NoError(t, err)
	_, err = c.doSchemeFlip(c.http, req, nil)
	require.Error(t, err, "the API host is not a PDS transfer host and must fail, not downgrade")

	mu.Lock()
	got := conns
	mu.Unlock()
	assert.Equal(t, 1, got, "no retry for non-PDS hosts")
	assert.Equal(t, 0, strings.Count(logBuf.String(), "plain HTTP"), "no WARN for non-PDS hosts")
}

// TestIsPDSHost pins the flip-eligibility gate: the OSS subnets and the PC
// download CDN (dl-pc-*.drive.quark.cn) may downgrade, the API hosts never.
func TestIsPDSHost(t *testing.T) {
	for _, host := range []string{
		"ul-jsz-acc.pds.quark.cn",
		"dl-pc-jsz.pds.quark.cn",
		"dl-pc-zb.drive.quark.cn",
		"dl-somewhere.drive.quark.cn",
	} {
		assert.True(t, isPDSHost(host), "transfer host %s must be flip-eligible", host)
	}
	for _, host := range []string{
		"drive.quark.cn",
		"drive-pc.quark.cn",
		"pan.quark.cn",
		"drive.quark.cn.attacker",
		"notquark.cn",
	} {
		assert.False(t, isPDSHost(host), "host %s must never be flip-eligible", host)
	}
}

// TestDoSendsPCCLientHeaders proves every API request carries the official
// quark PC client User-Agent plus the pr/fr query pair. quark enforces a
// per-file download size cap (code 23018) unless the request is signed with
// that UA, so dropping it here regresses large downloads.
func TestDoSendsPCCLientHeaders(t *testing.T) {
	var (
		mu        sync.Mutex
		gotUA     string
		gotQuery  string
		gotBody   string
		gotCookie string
		gotRef    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUA = r.Header.Get("User-Agent")
		gotQuery = r.URL.RawQuery
		gotCookie = r.Header.Get("Cookie")
		gotRef = r.Header.Get("Referer")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":200,"code":0,"data":[{"download_url":"https://cdn.example/obj"}]}`)
	}))
	defer srv.Close()

	c := NewClient(context.Background(), srv.Client(), srv.Client(), srv.URL+"/1/clouddrive", "https://pan.quark.cn", "ucpro", "k1=v1")

	var resp DownResp
	err := c.Do(context.Background(), http.MethodPost, "/file/download", nil, map[string]interface{}{"fids": []string{"abc"}}, &resp)
	require.NoError(t, err)
	require.Len(t, resp.Data, 1)
	assert.Equal(t, "https://cdn.example/obj", resp.Data[0].DownloadUrl)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, UserAgent, gotUA, "every API call must carry the PC client UA (23018 bypass)")
	assert.Contains(t, gotQuery, "pr=ucpro")
	assert.Contains(t, gotQuery, "fr=pc")
	assert.Equal(t, "k1=v1", gotCookie)
	assert.Equal(t, "https://pan.quark.cn", gotRef)
	assert.Equal(t, `{"fids":["abc"]}`, gotBody)
}

// TestSplitAPIHTTP2DownloadHTTP11 pins the protocol split between the
// API/upload client (HTTP/2, like OpenList's resty default transport,
// ForceAttemptHTTP2 on) and the download client (HTTP/1.1, like OpenList's
// bare &http.Transport{}). quark's download edge resets long HTTP/2 streams
// and Go would multiplex concurrent chunk streams onto one H2 connection,
// collapsing them into a single throttle bucket (~1.4 MiB/s flat).
func TestSplitAPIHTTP2DownloadHTTP11(t *testing.T) {
	var (
		mu     sync.Mutex
		protos []int
	)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.ProtoMajor)
		mu.Unlock()
		switch r.URL.Path {
		case "/1/clouddrive/file/download":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":200,"code":0}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	ctx, _ := fs.AddConfig(context.Background())

	// Production-wire download client (fshttp, HTTP/1.1 forced). It is
	// deterministically HTTP/1.1: with ForceAttemptHTTP2 off the ALPN offer
	// never includes h2, no matter the state of http.DefaultTransport.
	downloadClient := fshttp.NewClientCustom(ctx, func(t *http.Transport) {
		t.ForceAttemptHTTP2 = false
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	})
	dlTr, ok := downloadClient.Transport.(*fshttp.Transport)
	require.True(t, ok)
	assert.False(t, dlTr.ForceAttemptHTTP2, "download client must stay on HTTP/1.1")

	// Production-wire API/upload client (fshttp). Its default carries
	// ForceAttemptHTTP2 true (copied from http.DefaultTransport), matching
	// OpenList's resty client.
	apiFshttp := fshttp.NewClientCustom(ctx, func(t *http.Transport) {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	})
	apiTr, ok := apiFshttp.Transport.(*fshttp.Transport)
	require.True(t, ok)
	assert.True(t, apiTr.ForceAttemptHTTP2, "API/upload client must keep HTTP/2 like OpenList's resty client")

	// http.DefaultTransport is process-global: once anything uses it,
	// fshttp's shallow clone (lib/structs.SetDefaults) inherits its
	// populated TLSNextProto and silently lands on HTTP/1.1 on the wire. The
	// flag asserts above pin the fshttp contract; for the wire proof this
	// pristine HTTP/2 transport negotiates for the API/upload side.
	apiWire := &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	}}
	c := NewClient(ctx, apiWire, downloadClient, srv.URL, "https://pan.quark.cn", "ucpro", "k1=v1")

	// API call on c.http:
	err := c.Do(ctx, http.MethodPost, "/1/clouddrive/file/download", nil, map[string]interface{}{"fids": []string{"abc"}}, nil)
	require.NoError(t, err)

	// OSS upload on c.http: same client and scheme-flip mechanism as
	// UploadPart/UploadCommit use.
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPut, srv.URL+"/obj?partNumber=1&uploadId=upid", bytes.NewReader([]byte("x")))
	require.NoError(t, err)
	upRes, err := c.doSchemeFlip(c.http, upReq, nil)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, upRes.Body)
	_ = upRes.Body.Close()

	// CDN download on c.download:
	res, err := c.Download(ctx, srv.URL+"/obj?signature", nil)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, protos, 3, "API POST + OSS PUT + download GET must each hit the server once")
	assert.Equal(t, 2, protos[0], "API call must negotiate HTTP/2")
	assert.Equal(t, 2, protos[1], "OSS upload must negotiate HTTP/2")
	assert.Equal(t, 1, protos[2], "download must negotiate HTTP/1.1")
}

// TestFshttpTransportFilterOverridesForcedUA pins the production mechanism
// behind the 23018 fix. The quark Fs runs over an fshttp.NewClient, and
// fshttp's transport force-stamps "rclone/" on every request (fs/fshttp
// RoundTrip, "Force user agent") - a per-request header set would be wiped
// before it reaches the wire. The Fs therefore installs a SetRequestFilter
// on the transport, which runs AFTER the stamping and is the only hook that
// guarantees the PC client UA reaches quark's /file/download endpoint.
func TestFshttpTransportFilterOverridesForcedUA(t *testing.T) {
	var (
		mu    sync.Mutex
		gotUA string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUA = r.Header.Get("User-Agent")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, _ := fs.AddConfig(context.Background())
	hc := fshttp.NewClient(ctx)
	tr, ok := hc.Transport.(*fshttp.Transport)
	require.True(t, ok, "fshttp.NewClient must return an fshttp transport")
	tr.SetRequestFilter(func(req *http.Request) {
		req.Header.Set("User-Agent", UserAgent)
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("User-Agent", "rclone/")
	_, err = hc.Do(req)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, UserAgent, gotUA, "the transport filter must override the forced rclone/ UA")
}

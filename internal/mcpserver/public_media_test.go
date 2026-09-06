package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type mediaTransport func(*http.Request) (*http.Response, error)

func (f mediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testCache(t *testing.T) *MediaCache {
	t.Helper()
	c := &MediaCache{Dir: filepath.Join(t.TempDir(), "cache"), TTL: 6 * time.Hour, MaxBytes: 8 << 20, MaxFileBytes: 1 << 20, MaxFiles: 4}
	if e := c.Prepare(); e != nil {
		t.Fatal(e)
	}
	return c
}
func TestPublicFakeIPExceptionIsBoundToHostnameAndTLS(t *testing.T) {
	d := newMediaDownloader(1024, false)
	d.fakeIP = true
	for _, tc := range []struct {
		host, port, ip string
		want           bool
	}{{"files.oaiusercontent.com", "443", "198.18.1.9", true}, {"evil.test", "443", "198.18.1.9", false}, {"oaiusercontent.com.evil.test", "443", "198.18.1.9", false}, {"files.oaiusercontent.com", "80", "198.18.1.9", false}, {"198.18.1.9", "443", "198.18.1.9", false}, {"files.oaiusercontent.com", "443", "127.0.0.1", false}, {"files.oaiusercontent.com", "443", "169.254.169.254", false}, {"files.oaiusercontent.com", "443", "10.0.0.1", false}, {"files.oaiusercontent.com", "443", "::ffff:127.0.0.1", false}, {"files.oaiusercontent.com", "443", "192.0.2.1", false}} {
		if got := d.allowedAddress(tc.host, tc.port, net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("%+v got %v", tc, got)
		}
	}
	d.fakeIP = false
	if d.allowedAddress("files.oaiusercontent.com", "443", net.ParseIP("198.18.1.9")) {
		t.Fatal("fake IP allowed by default")
	}
	transport := d.client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("unsafe proxy/TLS")
	}
}
func TestPublicInputSSRFAndRebinding(t *testing.T) {
	d := newMediaDownloader(1024, false)
	d.publicInputs = true
	for _, raw := range []string{"https://127.0.0.1/x", "https://files.oaiusercontent.com.evil.test/x", "https://files.oaiusercontent.com:8443/x", "https://user:pass@files.oaiusercontent.com/x", "http://files.oaiusercontent.com/x", "file:///etc/passwd", "https://[::1]/x"} {
		if _, e := d.validateRemoteURL(raw); e == nil {
			t.Errorf("allowed %s", raw)
		}
	}
	if _, e := d.validateRemoteURL("https://files.oaiusercontent.com/input.png?sig=secret"); e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse("https://169.254.169.254/latest/meta-data")
	if e := d.client.CheckRedirect(&http.Request{URL: u}, []*http.Request{{}}); e == nil {
		t.Fatal("redirect to metadata accepted")
	}
	d.lookupIP = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if conn, e := d.dialContext(context.Background(), "tcp", "files.oaiusercontent.com:443"); e == nil {
		conn.Close()
		t.Fatal("mixed DNS answer accepted")
	}
}
func TestPublicInputCleanupAndLimits(t *testing.T) {
	c := testCache(t)
	d := newMediaDownloader(c.MaxFileBytes, false)
	d.cache = c
	d.publicInputs = true
	var encoded bytes.Buffer
	_ = png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	d.client.Transport = mediaTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("credential sent to file host")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encoded.Bytes())), Header: http.Header{"Content-Type": []string{"image/png"}}}, nil
	})
	files := []FileInput{{DownloadURL: "https://files.oaiusercontent.com/coffee.png", FileID: "chatgpt-image", FileName: "../../coffee.png"}}
	m, e := d.materialize(context.Background(), files, fileKindImage)
	if e != nil {
		t.Fatal(e)
	}
	if len(m.Paths) != 1 || filepath.Dir(m.Paths[0]) != m.Dir {
		t.Fatal("unsafe path")
	}
	m.Cleanup()
	m.Cleanup()
	if _, e = os.Stat(m.Dir); !os.IsNotExist(e) {
		t.Fatal("normal cleanup failed")
	}
	d.client.Transport = mediaTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("download failed") })
	if _, e = d.materialize(context.Background(), files, fileKindImage); e == nil {
		t.Fatal("failure expected")
	}
	entries, _ := os.ReadDir(c.Dir)
	if len(entries) != 0 || c.reserved != 0 {
		t.Fatal("failure leaked cache")
	}
	if _, _, e = c.allocate(5); e == nil {
		t.Fatal("count limit bypass")
	}
	c.MaxBytes = c.MaxFileBytes - 1
	if _, _, e = c.allocate(1); e == nil {
		t.Fatal("byte limit bypass")
	}
}
func TestPublicJanitorKeepsActiveAndRejectsContentSpoof(t *testing.T) {
	c := testCache(t)
	stale := filepath.Join(c.Dir, mediaCachePrefix+"stale")
	_ = os.Mkdir(stale, 0700)
	old := time.Now().Add(-7 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	dir, release, e := c.allocate(1)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	_ = os.Chtimes(dir, old, old)
	if e = c.Cleanup(); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(stale); !os.IsNotExist(e) {
		t.Fatal("stale cache retained")
	}
	if _, e = os.Stat(dir); e != nil {
		t.Fatal("active cache removed")
	}
	for _, tc := range []struct{ mime, ext string }{{"text/html", ".png"}, {"application/octet-stream", ".mp4"}, {"image/png", ".exe"}, {"image/svg+xml", ".svg"}} {
		if validatePublicMedia(fileKindUpload, tc.mime, tc.ext) == nil {
			t.Fatal("MIME spoof allowed", tc)
		}
	}
}

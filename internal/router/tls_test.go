package router

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRateLimitAnswers429(t *testing.T) {
	r, srv, _ := newRouter(t)
	r.SetRateLimit(2) // burst 4
	a := newBackend(t, "a")
	r.Set("hello.localhost", "hello", addrOf(a))
	codes := make([]int, 0, 6)
	for range 6 {
		resp := get(t, srv, "hello.localhost", nil)
		_ = resp.Body.Close()
		codes = append(codes, resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "1" {
			t.Errorf("429 without Retry-After")
		}
	}
	// Four get through at once; the rest are refused until the bucket refills.
	want := []int{200, 200, 200, 200, 429, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("codes %v, want %v", codes, want)
		}
	}
	r.SetRateLimit(0)
	if resp := get(t, srv, "hello.localhost", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("limit off: %d", resp.StatusCode)
	}
}

func TestRateLimitCoversHandlerRoute(t *testing.T) {
	r, srv, _ := newRouterWith(t, Options{RateRPS: 1}) // burst 2
	r.SetHandler("dash.example.com", "_dashboard", dashboard())
	codes := make([]int, 0, 3)
	for range 3 {
		resp := get(t, srv, "dash.example.com", nil)
		_ = resp.Body.Close()
		codes = append(codes, resp.StatusCode)
	}
	if codes[0] != http.StatusTeapot || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("codes %v", codes)
	}
}

// The session cookie's Secure flag reads r.TLS and the login limit reads
// r.RemoteAddr, so both have to arrive as the client's connection set them.
func TestSetHandlerSeesTheConnection(t *testing.T) {
	r := New(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	type seen struct {
		tls    bool
		remote string
	}
	got := make(chan seen, 1)
	r.SetHandler("dash.example.com", "_dashboard", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got <- seen{req.TLS != nil, req.RemoteAddr}
	}))
	srv := httptest.NewTLSServer(r)
	t.Cleanup(srv.Close)

	local := make(chan string, 1)
	client := srv.Client()
	tr := client.Transport.(*http.Transport)
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := new(net.Dialer).DialContext(ctx, network, addr)
		if err == nil {
			local <- c.LocalAddr().String()
		}
		return c, err
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = "dash.example.com"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	s := <-got
	if !s.tls {
		t.Error("handler saw no TLS state")
	}
	if want := <-local; s.remote != want {
		t.Errorf("RemoteAddr = %q, want the client's %q", s.remote, want)
	}
}

func TestHandlerRouteRedirectsToHTTPS(t *testing.T) {
	r := New(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r.EnableTLS(TLSConfig{Email: "ops@example.com", CacheDir: t.TempDir()})
	r.SetHandler("dash.example.com", "_dashboard", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran on the plain listener")
	}))
	srv := httptest.NewServer(r.httpHandler())
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/login", strings.NewReader("token=secret"))
	req.Host = "dash.example.com"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://dash.example.com/login" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestHostPolicyAllowsHandlerRoute(t *testing.T) {
	r := New(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r.SetHandler("dash.example.com", "_dashboard", dashboard())
	if err := r.hostPolicy(context.Background(), "dash.example.com"); err != nil {
		t.Errorf("handler route refused: %v", err)
	}
	if err := r.hostPolicy(context.Background(), "other.example.com"); err == nil {
		t.Error("unrouted host allowed")
	}
}

func TestHTTPRedirectsToHTTPSWhenTLSIsOn(t *testing.T) {
	r := New(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r.EnableTLS(TLSConfig{Email: "ops@example.com", CacheDir: t.TempDir()})
	srv := httptest.NewServer(r.httpHandler())
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/path?q=1", strings.NewReader("x"))
	req.Host = "hello.example.com:80"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://hello.example.com/path?q=1" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// The redirect is rate limited like everything else.
	r.SetRateLimit(1) // burst 2; one request is spent above
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = "hello.example.com"
	var last int
	for range 3 {
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("third redirect request: %d, want 429", last)
	}
	r.SetRateLimit(0)

	// The ACME challenge path reaches the certificate manager instead.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/.well-known/acme-challenge/token", nil)
	req.Host = "hello.example.com"
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 == 3 {
		t.Fatalf("challenge path was redirected: %d", resp.StatusCode)
	}

	// Only routed hosts get certificates.
	r.Set("hello.example.com", "hello", "127.0.0.1:1")
	if err := r.hostPolicy(context.Background(), "hello.example.com"); err != nil {
		t.Errorf("routed host refused: %v", err)
	}
	if err := r.hostPolicy(context.Background(), "evil.example.com"); err == nil {
		t.Error("unrouted host allowed a certificate")
	}
	if r.tlsConf.MinVersion != tls.VersionTLS12 {
		t.Error("TLS 1.0 and 1.1 should be off")
	}
}

func TestServeTLS(t *testing.T) {
	r := New(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := r.ServeTLS(context.Background(), nil); err == nil {
		t.Fatal("ServeTLS without EnableTLS should fail")
	}
	cert := selfSigned(t)
	r.tlsConf = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	a := newBackend(t, "secure")
	r.Set("hello.localhost", "hello", addrOf(a))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.ServeTLS(ctx, ln) }()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // a test against a self-signed certificate
	req, _ := http.NewRequest(http.MethodGet, "https://"+ln.Addr().String()+"/", nil)
	req.Host = "hello.localhost"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, resp); b != "secure" || resp.Header.Get("Echo-Proto") != "https" {
		t.Fatalf("over TLS: %q proto %q", b, resp.Header.Get("Echo-Proto"))
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// selfSigned makes a certificate for hello.localhost, valid for an hour.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hello.localhost"},
		DNSNames:     []string{"hello.localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

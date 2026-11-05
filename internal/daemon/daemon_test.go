package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maddoxrjohnson/wherry/internal/config"
	"github.com/maddoxrjohnson/wherry/internal/metrics"
	"github.com/maddoxrjohnson/wherry/internal/router"
	"github.com/maddoxrjohnson/wherry/internal/web"
)

// logBuffer collects log lines from the daemon's goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestRunStartsAndStops wires the whole daemon on random ports with a
// temp data directory and checks it comes up, answers on its listeners,
// and shuts down cleanly. Docker may or may not be reachable; either way
// the daemon starts.
func TestRunStartsAndStops(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "d") // short: the hook socket path has a limit
	cfg.SSHAddr, cfg.HTTPAddr, cfg.InternalAddr = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	cfg.SSHHostKey = filepath.Join(cfg.DataDir, "host_ed25519")
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, "test", log) }()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(logs.String(), "msg=ready") {
		select {
		case err := <-done:
			t.Fatalf("Run returned early: %v\n%s", err, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never became ready:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{"hook socket", "ssh listening", "http listening", "api listening", "admin token created", "version=test runtime=docker"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
	apiAddr := addrAfter(t, logs.String(), `msg="api listening" addr=`)
	resp, err := http.Get("http://" + apiAddr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Errorf("healthz: %d %q", resp.StatusCode, body)
	}
	if resp, err := http.Get("http://" + apiAddr + "/api/v1/apps"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("api without a token: %v %v", resp, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "admin.token")); err != nil {
		t.Errorf("token file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "hooks", "pre-receive")); err != nil {
		t.Errorf("hook script: %v", err)
	}
	httpAddr := addrAfter(t, logs.String(), `msg="http listening" addr=`)
	if c, err := net.DialTimeout("tcp", httpAddr, time.Second); err == nil {
		_ = c.Close()
	} else {
		t.Errorf("http listener: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not stop")
	}
	if !strings.Contains(logs.String(), "shutting down") {
		t.Errorf("no shutdown line:\n%s", logs.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "wherry.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("hook socket left behind: %v", err)
	}
}

// addrAfter finds the host:port that follows prefix in the log.
func addrAfter(t *testing.T, log, prefix string) string {
	t.Helper()
	i := strings.Index(log, prefix)
	if i < 0 {
		t.Fatalf("log lacks %q:\n%s", prefix, log)
	}
	rest := log[i+len(prefix):]
	if j := strings.IndexAny(rest, " \n"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestListenersCloseOnFailure(t *testing.T) {
	var lns listeners
	a, err := lns.tcp("a", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lns.tcp("b", "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "b listen") {
		t.Fatalf("privileged port: %v", err)
	}
	lns.closeAll()
	if _, err := net.DialTimeout("tcp", a.Addr().String(), time.Second); err == nil {
		t.Fatal("first listener still open after closeAll")
	}
}

// The public host must get the dashboard and nothing the internal mux adds.
// The dashboard here is the real handler; none of these paths reach its store.
func TestPublicHostGetsOnlyTheDashboard(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := router.New(router.Options{Log: log})
	mets := metrics.New("test", "docker")
	mets.SetReady(true)
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	dash := web.New(nil, nil, nil, "token", web.Config{}, log)
	const host = "dash.example.com"
	internal := mountHTTP(rt, host, api, mets, dash, log)

	status := func(h http.Handler, path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	for _, path := range []string{"/api/apps", "/metrics", "/healthz"} {
		if got := status(rt, path); got != http.StatusNotFound {
			t.Errorf("public %s: %d, want 404", path, got)
		}
		if got := status(internal, path); got != http.StatusOK {
			t.Errorf("internal %s: %d, want 200", path, got)
		}
	}
	for name, h := range map[string]http.Handler{"public": rt, "internal": internal} {
		if got := status(h, "/login"); got != http.StatusOK {
			t.Errorf("%s /login: %d", name, got)
		}
	}
}

func TestNoPublicRouteWithoutAHost(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := router.New(router.Options{Log: log})
	mountHTTP(rt, "", http.NotFoundHandler(), metrics.New("test", "docker"), http.NotFoundHandler(), log)
	if _, ok := rt.Get(""); ok {
		t.Error("an empty dashboard host made a route")
	}
}

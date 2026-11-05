package web

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maddoxrjohnson/wherry/internal/logs"
	"github.com/maddoxrjohnson/wherry/internal/site"
	"github.com/maddoxrjohnson/wherry/internal/store"
	"github.com/maddoxrjohnson/wherry/internal/supervisor"
)

const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeSup struct{ status map[string]supervisor.Status }

func (f *fakeSup) Status(app string) (supervisor.Status, bool) {
	st, ok := f.status[app]
	return st, ok
}

type fixture struct {
	store   *store.Store
	hub     *logs.Hub
	sup     *fakeSup
	web     *Server
	server  *httptest.Server
	client  *http.Client // with a cookie jar, not following redirects
	dataDir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hub := logs.New(dataDir, nil, logs.Options{})
	t.Cleanup(func() { _ = hub.Close() })
	f := &fixture{store: st, hub: hub, sup: &fakeSup{status: map[string]supervisor.Status{}}, dataDir: dataDir}
	cfg := Config{DataDir: dataDir, Site: site.Site{Domain: "localhost", SSHAddr: "127.0.0.1:2222", HTTPPort: 8080}, Version: "test"}
	f.web = New(st, f.sup, hub, token, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.web.sessionCheck = 10 * time.Millisecond
	f.server = httptest.NewServer(f.web)
	t.Cleanup(f.server.Close)
	jar, _ := cookiejar.New(nil)
	f.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f
}

func (f *fixture) get(t *testing.T, path string) (int, string, *http.Response) {
	t.Helper()
	resp, err := f.client.Get(f.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp
}

func (f *fixture) login(t *testing.T, tok string) *http.Response {
	t.Helper()
	resp, err := f.client.PostForm(f.server.URL+"/login", map[string][]string{"token": {tok}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func TestLoginFlow(t *testing.T) {
	f := newFixture(t)
	if code, _, resp := f.get(t, "/"); code != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous /: %d %q", code, resp.Header.Get("Location"))
	}
	if code, body, _ := f.get(t, "/login"); code != http.StatusOK || !strings.Contains(body, `name="token"`) {
		t.Fatalf("login page: %d\n%s", code, body)
	}
	if resp := f.login(t, "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	if code, _, _ := f.get(t, "/"); code != http.StatusSeeOther {
		t.Fatal("a refused login must not start a session")
	}
	resp := f.login(t, token)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Value == "" {
		t.Fatalf("session cookie: %+v", cookie)
	}
	if code, body, _ := f.get(t, "/"); code != http.StatusOK || !strings.Contains(body, "No apps yet") || !strings.Contains(body, "wherryd test") {
		t.Fatalf("apps page: %d\n%s", code, body)
	}
	if code, _, resp := f.get(t, "/login"); code != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login page while logged in: %d", code)
	}
	// Static files need no session; everything else does after logout.
	resp, err := f.client.Post(f.server.URL+"/logout", "", nil)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %v %v", resp, err)
	}
	if code, _, _ := f.get(t, "/"); code != http.StatusSeeOther {
		t.Fatal("still logged in after logout")
	}
	if code, body, _ := f.get(t, "/static/app.js"); code != http.StatusOK || !strings.Contains(body, "EventSource") {
		t.Fatalf("static: %d", code)
	}
}

// postLogin sends a login straight to the handler, so a test can choose the
// client's address.
func (f *fixture) postLogin(body, remote string) *httptest.ResponseRecorder {
	return f.postLoginFrom(body, remote, nil)
}

// postLoginFrom is postLogin with the headers a browser would add.
func (f *fixture) postLoginFrom(body, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = remote
	w := httptest.NewRecorder()
	f.web.ServeHTTP(w, req)
	return w
}

func TestCrossOriginPosts(t *testing.T) {
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusSeeOther},
		{"typed in", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusSeeOther},
		{"curl", nil, http.StatusSeeOther},
		{"origin is the host", map[string]string{"Origin": "http://127.0.0.1:9000"}, http.StatusSeeOther},
		{"same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"other origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token="+token))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Host = "127.0.0.1:9000" // the internal listener, as a tunnel shows it
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			f.web.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("status %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestCrossOriginRefusalSpendsNoAttempt(t *testing.T) {
	f := newFixture(t)
	const client = "192.0.2.1:1234"
	for range 2 * loginBurst {
		w := f.postLoginFrom("token="+token, client, map[string]string{"Sec-Fetch-Site": "cross-site"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("cross-site login: %d", w.Code)
		}
	}
	if w := f.postLogin("token="+token, client); w.Code != http.StatusSeeOther {
		t.Fatalf("the operator's own login: %d", w.Code)
	}
}

func TestCrossOriginLogoutKeepsTheSession(t *testing.T) {
	f := newFixture(t)
	if resp := f.login(t, token); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, f.server.URL+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-site")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("same-site logout: %d", resp.StatusCode)
	}
	if code, _, _ := f.get(t, "/"); code != http.StatusOK {
		t.Fatal("a refused logout ended the session")
	}
}

func TestLoginLimit(t *testing.T) {
	f := newFixture(t)
	const client = "192.0.2.1:1234"
	for i := range loginBurst {
		if w := f.postLogin("token=wrong", client); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i+1, w.Code)
		}
	}
	// The right token is refused too: the limit comes before the check.
	w := f.postLogin("token="+token, "192.0.2.1:5678")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q", got)
	}
	if !strings.Contains(w.Body.String(), "too many attempts") || !strings.Contains(w.Body.String(), `name="token"`) {
		t.Errorf("no login page with the error:\n%s", w.Body)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Error("a refused login must not start a session")
	}
	if w := f.postLogin("token="+token, "192.0.2.2:1234"); w.Code != http.StatusSeeOther {
		t.Fatalf("another client: %d", w.Code)
	}
}

func TestLoginLimitCountsSuccessToo(t *testing.T) {
	f := newFixture(t)
	const client = "[2001:db8::1]:1234"
	for i := range loginBurst {
		if w := f.postLogin("token="+token, client); w.Code != http.StatusSeeOther {
			t.Fatalf("attempt %d: %d", i+1, w.Code)
		}
	}
	// Another address in the same /64 is the same client.
	if w := f.postLogin("token="+token, "[2001:db8::2]:1234"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", w.Code)
	}
}

func TestLoginBodyCap(t *testing.T) {
	f := newFixture(t)
	// The right token first, so only the cap can refuse it.
	body := "token=" + token + "&pad=" + strings.Repeat("x", maxLoginBody)
	if w := f.postLogin(body, "192.0.2.1:1234"); w.Code != http.StatusUnauthorized {
		t.Fatalf("oversize body: %d", w.Code)
	}
	if w := f.postLogin("token="+token, "192.0.2.1:1234"); w.Code != http.StatusSeeOther {
		t.Fatalf("right size: %d", w.Code)
	}
}

func TestLogoutCookieMatchesSession(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.TLS = &tls.ConnectionState{}
	w := httptest.NewRecorder()
	f.web.ServeHTTP(w, req)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: %+v", cookies)
	}
	c := cookies[0]
	if c.Name != cookieName || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge >= 0 {
		t.Fatalf("logout cookie: %+v", c)
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	if resp := f.login(t, token); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	want := map[string]string{
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
		"Content-Security-Policy": "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
	}
	for _, path := range []string{"/", "/login", "/static/style.css", "/nowhere"} {
		_, _, resp := f.get(t, path)
		for k, v := range want {
			if got := resp.Header.Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", path, k, got, v)
			}
		}
		if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("%s: HSTS over plain HTTP: %q", path, got)
		}
	}
	for path, want := range map[string]string{"/": "no-store", "/static/style.css": ""} {
		_, _, resp := f.get(t, path)
		if got := resp.Header.Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control = %q, want %q", path, got, want)
		}
	}
}

func TestHSTSOnlyOverTLS(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.TLS = &tls.ConnectionState{}
	w := httptest.NewRecorder()
	f.web.ServeHTTP(w, req)
	if got := w.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("Strict-Transport-Security = %q", got)
	}
}

func TestLoginPage(t *testing.T) {
	f := newFixture(t)
	code, body, resp := f.get(t, "/login")
	if code != http.StatusOK {
		t.Fatalf("login page: %d", code)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if strings.Contains(body, "wherryd") {
		t.Errorf("the login page gives the version away:\n%s", body)
	}
}

// The handler is all a public dashboard host ever gets, so what it does not
// route is what the public cannot reach.
func TestNoInternalRoutes(t *testing.T) {
	f := newFixture(t)
	if resp := f.login(t, token); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	for _, path := range []string{"/api/apps", "/api/v1/apps", "/metrics", "/healthz"} {
		if code, _, _ := f.get(t, path); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, code)
		}
	}
}

func TestAppPages(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	app, err := f.store.CreateApp(ctx, "hello", "hello.localhost")
	if err != nil {
		t.Fatal(err)
	}
	const sha = "abc1234567890abc1234567890abc1234567890a"
	v1, _ := f.store.CreateRelease(ctx, app.ID, store.NewRelease{Image: "wherry/hello:abc1234", GitSHA: sha, Note: "push abc1234"})
	if err := f.store.SetLive(ctx, app.ID, v1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateRelease(ctx, app.ID, store.NewRelease{Note: "push def5678"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateConfig(ctx, app.ID, map[string]string{"SECRET": "s3cr3t", "A": "1"}, nil); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(f.dataDir, "builds", "hello")
	_ = os.MkdirAll(logDir, 0o700)
	_ = os.WriteFile(filepath.Join(logDir, sha+".log"), []byte("Step 1/2 : FROM scratch\n"), 0o600)
	f.sup.status["hello"] = supervisor.Status{App: "hello", Release: "v1", Instance: "0123456789abcdef", Addr: "127.0.0.1:40000",
		State: supervisor.StateRunning, Restarts: 2, StartedAt: time.Now().Add(-90 * time.Second)}
	f.login(t, token)

	code, body, _ := f.get(t, "/")
	for _, want := range []string{`<a href="/apps/hello">hello</a>`, `href="http://hello.localhost:8080"`, "<td>v1</td>"} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("apps page lacks %q (status %d):\n%s", want, code, body)
		}
	}

	code, body, _ = f.get(t, "/apps/hello")
	for _, want := range []string{
		"<h1>hello</h1>", "git remote add wherry ssh://wherry@127.0.0.1:2222/hello",
		"<td>0123456789ab</td>", `class="state-running"`, "<td>2</td>", "<td>1m30s</td>",
		`class="status-live"`, `class="status-building"`, "push def5678",
		`<a href="/apps/hello/builds/` + sha + `">abc1234</a>`, "wherry/hello:abc1234",
		`<code>A</code>`, `data-value="s3cr3t"`, `class="reveal"`, `<pre id="logs" data-app="hello">`,
	} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("app page lacks %q (status %d):\n%s", want, code, body)
		}
	}
	if strings.Contains(body, ">s3cr3t<") {
		t.Error("config value shown unmasked")
	}
	if code, _, _ := f.get(t, "/apps/nope"); code != http.StatusNotFound {
		t.Errorf("unknown app: %d", code)
	}

	// Build logs: served as text, refused for anything that is not a commit.
	if code, body, resp := f.get(t, "/apps/hello/builds/"+sha); code != http.StatusOK || body != "Step 1/2 : FROM scratch\n" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("build log: %d %q", code, body)
	}
	if code, _, _ := f.get(t, "/apps/hello/builds/abc1234567890abc1234567890abc1234567890b"); code != http.StatusNotFound {
		t.Errorf("missing build log: %d", code)
	}
	for _, bad := range []string{"ABC", "abc1234", "zz", "abc1234567890abc1234567890abc1234567890g"} {
		if code, _, _ := f.get(t, "/apps/hello/builds/"+bad); code != http.StatusBadRequest {
			t.Errorf("bad sha %q: %d", bad, code)
		}
	}
}

func TestLogStream(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.CreateApp(context.Background(), "hello", "hello.localhost"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.get(t, "/apps/hello/logs"); code != http.StatusSeeOther {
		t.Fatalf("anonymous log stream: %d", code)
	}
	f.login(t, token)
	f.hub.Sink("hello").Append(logs.Line{App: "hello", Release: "v1", Stream: logs.StreamStdout, Text: "early"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/apps/hello/logs", nil)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	rd := bufio.NewReader(resp.Body)
	frame := func() string {
		var b strings.Builder
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended: %v (got %q)", err, b.String())
			}
			if line == "\n" {
				return b.String()
			}
			b.WriteString(line)
		}
	}
	if fr := frame(); !strings.HasPrefix(fr, "event: line\n") || !strings.Contains(fr, `"text":"early"`) {
		t.Fatalf("first frame %q", fr)
	}
	f.hub.Sink("hello").Append(logs.Line{App: "hello", Stream: logs.StreamStderr, Text: "live"})
	if fr := frame(); !strings.Contains(fr, `"text":"live"`) {
		t.Fatalf("live frame %q", fr)
	}
	if err := f.hub.Forget("hello"); err != nil {
		t.Fatal(err)
	}
	if fr := frame(); fr != "event: done\ndata: \n" {
		t.Fatalf("after forget %q", fr)
	}
}

func TestLogStreamEndsWithTheSession(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.CreateApp(context.Background(), "hello", "hello.localhost"); err != nil {
		t.Fatal(err)
	}
	if resp := f.login(t, token); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+"/apps/hello/logs", nil)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	out, err := f.client.Post(f.server.URL+"/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Body.Close()
	// Only a stream the server closed ends without the deadline's error.
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("stream did not end: %v", err)
	}
	if code, _, _ := f.get(t, "/apps/hello/logs"); code != http.StatusSeeOther {
		t.Fatalf("reconnect: %d, want the login redirect", code)
	}
}

func TestUntilLoggedOutStopsItsWatcher(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "live"})
	f.web.mu.Lock()
	f.web.sessions["live"] = time.Now().Add(time.Hour)
	f.web.mu.Unlock()
	f.web.sessionCheck = time.Hour // the ticker never fires; stop alone must end it
	ctx, stop := f.web.untilLoggedOut(req)
	if ctx.Err() != nil {
		t.Fatal("a live session's context ended early")
	}
	stop() // returns only once the watcher has
	if ctx.Err() == nil {
		t.Fatal("stop left the context running")
	}
}

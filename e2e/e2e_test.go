//go:build e2e

// Package e2e runs the built binaries the way a user does: a real wherryd on
// Docker, the wherry CLI, git over SSH, and HTTP through the router. It
// imports nothing from the module, so it can only see what a user can see.
// Run it with make e2e, which builds bin/ first.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	helloV1  = "hello from wherry, v1"
	helloV2  = "hello from wherry, v2"
	unset    = "(GREETING is not set)"
	greeting = "ahoy from the e2e run"

	// How long anything that is not a build gets to happen.
	settle = time.Minute

	// Relative to this package, which is where go test runs it.
	wherryd = "../bin/wherryd"
	wherry  = "../bin/wherry"
)

// TestAppLifecycle is one story, because each step needs the state the one
// before it left: a deploy, a redeploy, a config change, a crash, and a
// daemon restart.
func TestAppLifecycle(t *testing.T) {
	h := newHarness(t)
	h.startDaemon()
	h.login()

	t.Log("push hello-go, then a change to it")
	hello := h.createApp("hello")
	h.checkout(hello, "hello-go")
	h.commitAndPush(hello, "first")
	h.waitBody(hello, helloV1)

	mainGo := filepath.Join(hello.dir, "main.go")
	src, err := os.ReadFile(mainGo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), helloV1) {
		t.Fatalf("examples/hello-go/main.go no longer says %q", helloV1)
	}
	if err := os.WriteFile(mainGo, []byte(strings.ReplaceAll(string(src), helloV1, helloV2)), 0o644); err != nil {
		t.Fatal(err)
	}
	h.commitAndPush(hello, "second")
	h.waitBody(hello, helloV2)

	t.Log("set a config var on config-reader")
	reader := h.createApp("reader")
	h.checkout(reader, "config-reader")
	h.commitAndPush(reader, "first")
	h.waitBody(reader, unset)
	h.wherryCLI("config:set", reader.name, "GREETING="+greeting)
	h.waitBody(reader, greeting)

	t.Log("kill hello's container")
	killed := h.onlyContainer(hello)
	if _, err := docker("kill", killed); err != nil {
		t.Fatal(err)
	}
	h.eventually("hello to be restarted after docker kill", func() (bool, string) {
		st := h.ps(hello)
		return st.State == "running" && st.Instance != killed && st.Restarts == 1, fmt.Sprintf("ps: %+v", st)
	})
	restarted := h.onlyContainer(hello)
	h.waitBody(hello, helloV2)

	t.Log("stop the daemon")
	if err := h.stopDaemon(); err != nil {
		t.Fatalf("wherryd did not exit cleanly on SIGTERM: %v", err)
	}
	if status, body, err := h.get(h.httpAddr, hello.host); err == nil {
		t.Fatalf("the router answered with the daemon stopped: %d %q", status, body)
	}

	t.Log("start the daemon again")
	h.refuseForeignContainers()
	h.startDaemon()
	// Reconcile runs before the listeners open and /healthz answers after
	// they have, so the first request to each app has to work.
	for _, tc := range []struct {
		app  app
		want string
	}{{hello, helloV2}, {reader, greeting}} {
		status, body, err := h.get(h.httpAddr, tc.app.host)
		if err != nil || status != http.StatusOK || body != tc.want {
			t.Fatalf("%s right after the restart: %d %q, %v; want 200 %q", tc.app.host, status, body, err, tc.want)
		}
	}
	if got := h.onlyContainer(hello); got != restarted {
		t.Fatalf("container changed across the restart: %s, want %s adopted", got, restarted)
	}
	st := h.ps(hello)
	if st.State != "running" || st.Instance != restarted || st.Restarts != 0 {
		t.Fatalf("ps after the restart: %+v, want running %s with the count reset", st, restarted)
	}
}

// lockDocker keeps two runs apart while both are still building, before either
// has a container for the other to see. The kernel drops the lock however the
// process ends, and locking the temp directory itself leaves no file behind.
func lockDocker(t *testing.T) {
	t.Helper()
	lock, err := os.Open(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("another e2e run holds the lock on %s: %v", os.TempDir(), err)
	}
}

// refuseForeignContainers stops the test before it can do damage. Every start
// of the daemon reconciles, and reconcile treats each wherry.app container on
// the host as its own, so it would stop another daemon's apps however unique
// our app names are.
func (h *harness) refuseForeignContainers() {
	t := h.t
	t.Helper()
	all, err := docker("ps", "-aq", "--filter", "label=wherry.app")
	if err != nil {
		t.Fatal(err)
	}
	ours, err := docker("ps", "-aq", "--filter", "label=wherry.test="+h.run)
	if err != nil {
		t.Fatal(err)
	}
	foreign := slices.DeleteFunc(strings.Fields(all), func(id string) bool {
		return slices.Contains(strings.Fields(ours), id)
	})
	if len(foreign) > 0 {
		t.Fatalf("refusing to run: the daemon would stop these containers (docker ps -a --filter label=wherry.app): %v", foreign)
	}
}

// harness is one run's daemon and everything needed to talk to it.
type harness struct {
	t   *testing.T
	ctx context.Context

	run        string // unique per run; names the apps and labels what they build
	dir        string
	key        string   // private half of the push key
	env        []string // the environment of a user with nothing configured
	dockerHost string   // the engine the docker CLI talks to

	httpAddr, sshAddr, internalAddr string

	daemon     *exec.Cmd
	daemonDone chan struct{}
	daemonErr  error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	lockDocker(t)
	id := make([]byte, 4)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	// Not t.TempDir: its long path pushes the daemon's socket past the
	// 108 bytes a Unix socket path can hold.
	dir, err := os.MkdirTemp("", "wherry-e2e")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, run: hex.EncodeToString(id), dir: dir}
	t.Cleanup(h.cleanup)

	// The daemon's client knows only DOCKER_HOST, not the CLI's contexts, and
	// the guard and cleanup below have to look at the engine it uses.
	if h.dockerHost, err = docker("context", "inspect", "--format", "{{.Endpoints.docker.Host}}"); err != nil {
		t.Fatal(err)
	}
	h.refuseForeignContainers()

	// go test's own timeout panics, which skips cleanup and leaves everything
	// running, so stop short of it. An interrupt gets the same treatment.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	t.Cleanup(stop)
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-3*time.Minute))
		t.Cleanup(cancel)
	}
	h.ctx = ctx

	addrs := freeAddrs(t, 3)
	h.httpAddr, h.sshAddr, h.internalAddr = addrs[0], addrs[1], addrs[2]

	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	h.key = filepath.Join(home, "id_ed25519")
	sshCmd := strings.Join([]string{
		"ssh", "-F", "/dev/null", "-i", h.key,
		"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + filepath.Join(home, "known_hosts"),
		"-o", "StrictHostKeyChecking=accept-new",
	}, " ")
	// A CI runner has no git identity and a laptop has settings that are not
	// ours to depend on, so neither one's are used.
	h.env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "WHERRY_") || strings.HasPrefix(kv, "GIT_")
	}),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_SSH_COMMAND="+sshCmd,
		"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.com",
		"WHERRY_CONFIG="+filepath.Join(dir, "cli.config"),
	)
	h.runIn("", "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "e2e", "-f", h.key)
	return h
}

func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	addrs := make([]string, n)
	for i := range addrs {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		// Held open until the loop ends so no two come back the same.
		defer func() { _ = l.Close() }()
		addrs[i] = l.Addr().String()
	}
	return addrs
}

// cleanup uses its own timeouts because h.ctx may be why the test is ending.
// Everything is found by this run's labels, so nothing else can be touched.
func (h *harness) cleanup() {
	t := h.t
	t.Helper()
	if h.daemon != nil {
		if err := h.stopDaemon(); err != nil {
			t.Logf("stopping wherryd: %v", err)
		}
	}
	if t.Failed() {
		h.logDaemonTail()
	}
	label := "label=wherry.test=" + h.run
	ids := func(args ...string) []string {
		out, err := docker(args...)
		if err != nil {
			t.Error(err)
		}
		return strings.Fields(out)
	}
	if found := ids("ps", "-aq", "--filter", label); len(found) > 0 {
		_, _ = docker(append([]string{"rm", "-f"}, found...)...)
	}
	// Removing an image can take its parents with it, so a later id may
	// already be gone and an error here proves nothing. The check below does.
	if found := ids("images", "-aq", "--filter", label); len(found) > 0 {
		_, _ = docker(append([]string{"rmi", "-f"}, found...)...)
	}
	if left := ids("ps", "-aq", "--filter", label); len(left) > 0 {
		t.Errorf("containers left behind: %v", left)
	}
	if left := ids("images", "-aq", "--filter", label); len(left) > 0 {
		t.Errorf("images left behind: %v", left)
	}
	if err := os.RemoveAll(h.dir); err != nil {
		t.Errorf("remove %s: %v", h.dir, err)
	}
}

// docker has a timeout of its own, not h.ctx, because cleanup needs it after
// h.ctx has ended. It runs with the caller's environment, not h.env: its
// context and config live under the real HOME.
func docker(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		err = fmt.Errorf("%w\n%s", err, exit.Stderr)
	}
	if err != nil {
		return "", fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (h *harness) logDaemonTail() {
	tail, err := os.ReadFile(filepath.Join(h.dir, "wherryd.log"))
	if err != nil {
		h.t.Logf("no wherryd log: %v", err)
		return
	}
	if len(tail) > 1<<16 {
		tail = tail[len(tail)-1<<16:]
	}
	h.t.Logf("wherryd log:\n%s", tail)
}

// startDaemon starts wherryd and waits for /healthz. The context is left off
// on purpose: it would SIGKILL the daemon, which should get a SIGTERM.
func (h *harness) startDaemon() {
	t := h.t
	t.Helper()
	log, err := os.OpenFile(filepath.Join(h.dir, "wherryd.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	cmd := exec.Command(wherryd)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = append(slices.Clone(h.env),
		"DOCKER_HOST="+h.dockerHost,
		"WHERRY_DATA_DIR="+filepath.Join(h.dir, "data"),
		"WHERRY_DOMAIN=localhost",
		"WHERRY_HTTP_ADDR="+h.httpAddr,
		"WHERRY_SSH_ADDR="+h.sshAddr,
		"WHERRY_INTERNAL_ADDR="+h.internalAddr,
		"WHERRY_RUNTIME=docker",
		"WHERRY_LOG_FORMAT=text",
		"WHERRY_DRAIN_GRACE=1s",
		"WHERRY_STOP_GRACE=2s",
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h.daemon, h.daemonDone = cmd, make(chan struct{})
	done := h.daemonDone
	go func() {
		h.daemonErr = cmd.Wait()
		close(done)
	}()
	h.eventually("wherryd to answer /healthz", func() (bool, string) {
		select {
		case <-done:
			t.Fatalf("wherryd exited during startup: %v", h.daemonErr)
		default:
		}
		status, body, err := h.get(h.internalAddr+"/healthz", "")
		if err != nil {
			return false, err.Error()
		}
		return status == http.StatusOK, fmt.Sprintf("%d %q", status, body)
	})
}

// stopDaemon sends SIGTERM and returns how the daemon exited, killing it if
// it will not go.
func (h *harness) stopDaemon() error {
	cmd, done := h.daemon, h.daemonDone
	h.daemon = nil
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		return h.daemonErr
	case <-time.After(settle):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("still running %s after SIGTERM", settle)
	}
}

// get makes one request with the given Host header, on a new connection each
// time, so a router that has gone away cannot look alive through an old one.
func (h *harness) get(target, host string) (int, string, error) {
	req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, "http://"+target, nil)
	if err != nil {
		return 0, "", err
	}
	req.Host = host
	client := http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	return res.StatusCode, strings.TrimSpace(string(body)), err
}

// eventually polls check until it passes, and fails with what it last saw.
func (h *harness) eventually(what string, check func() (ok bool, last string)) {
	h.t.Helper()
	deadline := time.After(settle)
	for {
		ok, last := check()
		if ok {
			return
		}
		select {
		case <-deadline:
			h.t.Fatalf("waited %s for %s; last saw: %s", settle, what, last)
		case <-h.ctx.Done():
			h.t.Fatalf("gave up waiting for %s: %v; last saw: %s", what, context.Cause(h.ctx), last)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (h *harness) waitBody(a app, want string) {
	h.t.Helper()
	h.eventually(fmt.Sprintf("%s to answer %q", a.host, want), func() (bool, string) {
		status, body, err := h.get(h.httpAddr, a.host)
		if err != nil {
			return false, err.Error()
		}
		return status == http.StatusOK && body == want, fmt.Sprintf("%d %q", status, body)
	})
}

// runIn runs a command as the user and returns its output. Only h.ctx limits
// it, since a first git push may be pulling the base image as well as building.
func (h *harness) runIn(dir, name string, args ...string) string {
	h.t.Helper()
	cmd := exec.CommandContext(h.ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = h.env
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (h *harness) wherryCLI(args ...string) string {
	h.t.Helper()
	return h.runIn("", wherry, args...)
}

func (h *harness) login() {
	h.t.Helper()
	token, err := os.ReadFile(filepath.Join(h.dir, "data", "admin.token"))
	if err != nil {
		h.t.Fatal(err)
	}
	h.wherryCLI("login", "http://"+h.internalAddr, strings.TrimSpace(string(token)))
	h.wherryCLI("keys:add", "e2e", h.key+".pub")
}

// app is an app on the daemon and its working copy.
type app struct {
	name, host, remote string
	dir                string
}

func (h *harness) createApp(base string) app {
	h.t.Helper()
	name := "e2e-" + base + "-" + h.run
	var a struct{ Host, Remote string }
	out := h.wherryCLI("--json", "apps:create", name)
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		h.t.Fatalf("apps:create: %v\n%s", err, out)
	}
	return app{name: name, host: a.Host, remote: a.Remote, dir: filepath.Join(h.dir, "repos", name)}
}

// checkout copies an example into a's working copy as a new repository.
func (h *harness) checkout(a app, example string) {
	t := h.t
	t.Helper()
	if err := os.CopyFS(a.dir, os.DirFS(filepath.Join("..", "examples", example))); err != nil {
		t.Fatal(err)
	}
	h.labelStages(filepath.Join(a.dir, "Dockerfile"))
	h.runIn(a.dir, "git", "init", "-q", "-b", "main")
	h.runIn(a.dir, "git", "remote", "add", "wherry", a.remote)
}

// labelStages adds this run's label to every stage of a Dockerfile. The daemon
// only labels the final image, and a multi-stage build also leaves the image of
// each earlier stage behind, untagged. A label is the only thing that says
// those were ours, and containers inherit it too. The cost is the build cache:
// each run compiles from scratch.
func (h *harness) labelStages(path string) {
	t := h.t
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for line := range strings.Lines(string(src)) {
		out.WriteString(line)
		if strings.HasPrefix(line, "FROM ") {
			out.WriteString("LABEL wherry.test=" + h.run + "\n")
		}
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitAndPush returns once the deploy has finished: git push stays open
// for it, and a failed deploy rejects the push.
func (h *harness) commitAndPush(a app, message string) {
	h.t.Helper()
	h.runIn(a.dir, "git", "add", "-A")
	h.runIn(a.dir, "git", "commit", "-q", "-m", message)
	h.runIn(a.dir, "git", "push", "-q", "wherry", "main")
}

// onlyContainer returns the app's one container, counting exited ones. After
// a deploy the old instance can still be draining, so more than one settles.
func (h *harness) onlyContainer(a app) string {
	h.t.Helper()
	var id string
	h.eventually("exactly one container for "+a.name, func() (bool, string) {
		out, err := docker("ps", "-a", "-q", "--no-trunc", "--filter", "label=wherry.app="+a.name)
		if err != nil {
			h.t.Fatal(err)
		}
		ids := strings.Fields(out)
		if len(ids) != 1 {
			return false, fmt.Sprint(ids)
		}
		id = ids[0]
		return true, ""
	})
	return id
}

type instance struct {
	Instance string
	State    string
	Restarts int
}

// ps is what wherry ps says about the app's one instance.
func (h *harness) ps(a app) instance {
	h.t.Helper()
	var ps struct{ Instances []instance }
	out := h.wherryCLI("ps", "--json", a.name)
	if err := json.Unmarshal([]byte(out), &ps); err != nil {
		h.t.Fatalf("ps: %v\n%s", err, out)
	}
	if len(ps.Instances) != 1 {
		h.t.Fatalf("ps %s: want one instance\n%s", a.name, out)
	}
	return ps.Instances[0]
}

package hook

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/maddoxrjohnson/wherry/internal/gitserver"
	"github.com/maddoxrjohnson/wherry/internal/store"
)

// sshRig is what the daemon wires together for a push: the SSH server in
// front of git, the hook server behind it, and one app with one work tree
// holding a commit to push.
type sshRig struct {
	hooks   *Server
	git     *gitserver.Server
	addr    string
	port    string
	signer  ssh.Signer
	keyFile string
	repo    string
	work    string
	head    string
}

func startRig(t *testing.T, d *fakeDeployer) *sshRig {
	t.Helper()
	for _, bin := range []string{"git", "ssh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	s := startServer(t, d)
	if err := InstallScript(s.dataDir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(s.dataDir, "wherry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateApp(ctx, "hello", "hello.localhost"); err != nil {
		t.Fatal(err)
	}
	repo, err := gitserver.InitRepo(ctx, s.dataDir, "hello")
	if err != nil {
		t.Fatal(err)
	}
	r := &sshRig{hooks: s, repo: repo, work: t.TempDir()}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if r.signer, err = ssh.NewSignerFromKey(priv); err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddKey(ctx, "laptop", ssh.FingerprintSHA256(sshPub), string(ssh.MarshalAuthorizedKey(sshPub))); err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	r.keyFile = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(r.keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	if r.git, err = gitserver.New(st, s, s.dataDir, filepath.Join(s.dataDir, "host_ed25519"), nil); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.addr = ln.Addr().String()
	_, r.port, _ = net.SplitHostPort(r.addr)
	done := make(chan error, 1)
	go func() { done <- r.git.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("SSH server did not stop after cancel")
		}
	})

	r.runGit(t, "init", "--quiet", "--initial-branch=main")
	r.runGit(t, "commit", "--quiet", "--allow-empty", "-m", "one")
	r.head = r.runGit(t, "rev-parse", "HEAD")
	return r
}

func (r *sshRig) env() []string {
	sshCmd := strings.Join([]string{
		"ssh", "-F", "/dev/null", "-i", r.keyFile, "-p", r.port, "-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=no", "-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
	}, " ")
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + r.work, "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=" + sshCmd,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}
}

func (r *sshRig) runGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.work
	cmd.Env = r.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// pushWithGit starts a real git push and returns how to pull the plug on it:
// git and its ssh are killed outright, as if the laptop had gone away.
func pushWithGit(t *testing.T, r *sshRig) (kill func()) {
	t.Helper()
	cmd := exec.Command("git", "push", fmt.Sprintf("ssh://wherry@127.0.0.1:%s/hello", r.port), "main")
	cmd.Dir = r.work
	cmd.Env = r.env()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	kill = func() {
		if killed {
			return
		}
		killed = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}
	t.Cleanup(kill)
	return kill
}

// pushWithoutSideBand speaks receive-pack by hand, leaving side-band-64k out
// of its capabilities, which no git client does. Then git gives the hook its
// own stderr instead of a pipe to its sideband thread, so a hook that
// outlives git holds that stderr open, and cmd.Wait has to give up on it.
func pushWithoutSideBand(t *testing.T, r *sshRig) (kill func()) {
	t.Helper()
	pack := exec.Command("git", "pack-objects", "--stdout", "--revs", "--quiet")
	pack.Dir = r.work
	pack.Stdin = strings.NewReader(r.head + "\n")
	data, err := pack.Output()
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", r.addr, &ssh.ClientConfig{
		User:            "wherry",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(r.signer)},
		HostKeyCallback: ssh.FixedHostKey(r.git.HostKey()),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("git-receive-pack 'hello'"); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("0", len(r.head)) + " " + r.head + " refs/heads/main\x00report-status\n"
	var msg bytes.Buffer
	fmt.Fprintf(&msg, "%04x%s0000", len(line)+4, line)
	msg.Write(data)
	if _, err := stdin.Write(msg.Bytes()); err != nil {
		t.Fatal(err)
	}
	return func() { _ = client.Close() }
}

// hookPIDs lists the live processes running this test binary as a hook.
func hookPIDs(t *testing.T) []string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("listing processes needs /proc")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(exe + "\x00hook\x00pre-receive\x00")
	paths, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	var pids []string
	for _, p := range paths {
		if cmdline, err := os.ReadFile(p); err == nil && bytes.Equal(cmdline, want) {
			pids = append(pids, filepath.Base(filepath.Dir(p)))
		}
	}
	return pids
}

// TestPusherGoneCancelsSilentDeploy joins the two halves of a dropped push.
// The SSH server's SIGTERM reaches git receive-pack but not the hook, which
// sits in a read on the socket and only finds out about git's death when it
// next writes. A deploy that is writing nothing, in a health wait or a quiet
// build step, gives it no such chance, so the socket alone would never say
// that the push is dead. The deploy has to hear it from git's exit.
func TestPusherGoneCancelsSilentDeploy(t *testing.T) {
	const bound = 10 * time.Second // stopGrace, for the case Wait must give up on, and some air
	for name, push := range map[string]func(*testing.T, *sshRig) func(){
		"git push":     pushWithGit,
		"no side-band": pushWithoutSideBand,
	} {
		t.Run(name, func(t *testing.T) {
			d := &fakeDeployer{block: true, done: make(chan error, 1)}
			r := startRig(t, d)
			kill := push(t, r)
			waitFor(t, "Deploy to start", func() bool { return len(d.callList()) == 1 })
			if len(hookPIDs(t)) != 1 {
				t.Fatalf("hook processes = %v, want one", hookPIDs(t))
			}

			kill()
			start := time.Now()
			select {
			case err := <-d.done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("deploy ended with %v, want context.Canceled", err)
				}
				t.Logf("deploy cancelled %v after the client died", time.Since(start).Round(time.Millisecond))
			case <-time.After(bound):
				t.Fatalf("silent deploy still running %v after the client died", bound)
			}
			// The hook hung on that socket with no git above it. Nothing is
			// left to reap it, so it has to leave by itself.
			waitFor(t, "the orphaned hook to exit", func() bool { return len(hookPIDs(t)) == 0 })

			out, err := exec.Command("git", "--git-dir="+r.repo, "for-each-ref").CombinedOutput()
			if err != nil || len(out) != 0 {
				t.Errorf("refs after the dead push: %q, err %v; want none", out, err)
			}
		})
	}
}

// TestSilentDeployPushSucceeds is the other side of that cancellation. A
// deploy this quiet looks just like a dead push from the hook's end, so only
// the order of things on the SSH server tells them apart: git is still waiting
// for its hook, and revoke must not fire until it stops.
func TestSilentDeployPushSucceeds(t *testing.T) {
	d := &fakeDeployer{delay: 200 * time.Millisecond}
	r := startRig(t, d)
	url := fmt.Sprintf("ssh://wherry@127.0.0.1:%s/hello", r.port)
	for i := range 5 {
		r.runGit(t, "commit", "--quiet", "--allow-empty", "-m", fmt.Sprintf("push %d", i))
		want := r.runGit(t, "rev-parse", "HEAD")
		r.runGit(t, "push", url, "main") // fails the test on a non-zero exit

		out, err := exec.Command("git", "--git-dir="+r.repo, "rev-parse", "refs/heads/main").CombinedOutput()
		if got := strings.TrimSpace(string(out)); err != nil || got != want {
			t.Fatalf("push %d: main = %q, err %v; want %s", i, got, err, want)
		}
		if got := len(d.callList()); got != i+1 {
			t.Fatalf("push %d: %d deploys, want %d", i, got, i+1)
		}
	}
}

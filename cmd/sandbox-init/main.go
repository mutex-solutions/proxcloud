// Command sandbox-init runs as PID 1 inside a customer image: it prepares the default user,
// starts E2B's envd, pushes the user env to it via POST /init, optionally launches
// PROXCLOUD_START_CMD, then marks the sandbox ready and supervises envd.
//
//	sandbox-init          run envd as the container command
//	sandbox-init -native <argv...> mark boot then replace PID 1 with native command
//	sandbox-init -ready     startup probe: exit 0 iff the ready marker exists
//	sandbox-init -snapshot  gzip tar of everything changed since boot, to stdout (pause/snapshot)
package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	envdBin  = "/.proxcloud/envd"
	envdAddr = "http://127.0.0.1:49983"
)

func main() {
	m, err := runMode(os.Args[1:], os.Getpid())
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "sandbox-init:", err)
		os.Exit(2)
	case m == "-prepull":
		// Pull-only worker: no envd, image ENV interpretation or customer command.
		return
	case m == "-snapshot":
		os.Exit(runSnapshot())
	case m == "-ready":
		os.Exit(readyCheck(readyPath))
	case m == "-native":
		if err := nativeExec(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox-init:", err)
			os.Exit(1)
		}
		return
	}
	log.SetPrefix("sandbox-init: ")
	log.SetFlags(0)
	if err := validateBootEnv(os.Environ()); err != nil {
		log.Fatalf("environment: %v", err)
	}
	markBoot() // everything after this point is "changed since boot" for -snapshot

	userEnv, childEnv := splitEnv(os.Environ())
	token := os.Getenv("PROXCLOUD_ENVD_ACCESS_TOKEN")
	user := os.Getenv("PROXCLOUD_DEFAULT_USER")
	if user == "" {
		user = defaultUsr
	}
	startCmd := os.Getenv("PROXCLOUD_START_CMD")
	readyCmd := os.Getenv("PROXCLOUD_READY_CMD")
	workdir := os.Getenv("PROXCLOUD_DEFAULT_WORKDIR")
	startAsRoot := os.Getenv("PROXCLOUD_START_AS_ROOT") == "1"
	_ = os.Remove(readyPath)
	go forwardLocalhost(time.Second) // services bound to localhost become reachable on the pod IP

	uid, gid, home := 0, 0, "/root"
	if user != "root" {
		var err error
		if uid, gid, home, err = ensureUser("/etc/passwd", "/etc/group", "/home", user); err != nil {
			log.Printf("ensure user %q failed, falling back to root: %v", user, err)
			user, uid, gid, home = "root", 0, 0, "/root"
		}
	}

	// Subscribe before forking so a fast-exiting child is never missed.
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)

	startEnvd := func() *exec.Cmd {
		c := exec.Command(envdBin, "-isnotfc", "-no-cgroups", "-port", "49983")
		c.Env = childEnv
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Start(); err != nil {
			log.Fatalf("start envd: %v", err)
		}
		return c
	}
	envd := startEnvd()
	if workdir == "" {
		workdir = home
	}

	// readyPID is the ready-check loop (PROXCLOUD_READY_CMD), reaped below like every child: its
	// exit 0 is what marks the sandbox ready. The lock spans Start+store so a loop that exits at
	// once is still recognised by the reaper.
	var readyMu sync.Mutex
	readyPID := 0

	go func() {
		if err := initEnvd(userEnv, token, user, workdir, now()); err != nil {
			log.Printf("init failed: %v", err)
			_ = envd.Process.Signal(syscall.SIGTERM)
			time.Sleep(2 * time.Second)
			os.Exit(1)
		}
		if startCmd != "" {
			su, shome, suid, sgid := user, home, uid, gid
			if startAsRoot { // the template's service needs root (e.g. a server under /root)
				su, shome, suid, sgid = "root", "/root", 0, 0
			}
			if err := userCmd("sh", []string{"-c", startCmd}, userEnv, su, shome, workdir, suid, sgid).Start(); err != nil {
				log.Printf("start cmd: %v", err)
			}
		}
		if readyCmd == "" {
			markReady()
			return
		}
		// Retry the template's ready command every second until it succeeds.
		loop := userCmd("sh", []string{"-c", `until sh -c "$PROXCLOUD_READY_CMD"; do sleep 1; done`},
			mergeEnv(userEnv, "PROXCLOUD_READY_CMD", readyCmd), user, home, workdir, uid, gid)
		readyMu.Lock()
		if err := loop.Start(); err != nil {
			log.Printf("start ready check: %v", err)
			markReady()
		} else {
			readyPID = loop.Process.Pid
		}
		readyMu.Unlock()
	}()

	// PID 1 loop. Children are never Wait()ed through os/exec, so this is the only reaper.
	// envd exiting on its own (crash, or customer code killing it) restarts envd in place
	// instead of exiting: a container restart would flap the pod's readiness. Only a stop from
	// the kubelet (SIGTERM/SIGINT) ends the sandbox.
	stopping := false
	for s := range sig {
		switch s {
		case syscall.SIGTERM, syscall.SIGINT:
			stopping = true
			_ = envd.Process.Signal(s)
		case syscall.SIGCHLD:
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
				readyMu.Lock()
				if pid == readyPID {
					if ws.ExitStatus() == 0 {
						markReady()
					}
					readyPID = 0
				}
				readyMu.Unlock()
				if pid == envd.Process.Pid {
					code := ws.ExitStatus()
					if ws.Signaled() {
						code = 128 + int(ws.Signal())
					}
					if stopping {
						os.Exit(code)
					}
					log.Printf("envd exited (%d); restarting", code)
					envd = restartEnvd(startEnvd, func() error { return initEnvd(userEnv, token, user, workdir, now()) }, restartHold)
				}
			}
		}
	}
}

func now() time.Time { return time.Now() }

// initEnvd waits (up to 30s) for envd's /health, then POSTs /init.
func initEnvd(env map[string]string, token, user, home string, t time.Time) error {
	c := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := c.Get(envdAddr + "/health")
		if err == nil {
			res.Body.Close()
			if res.StatusCode < 300 {
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("envd not healthy after 30s: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	body, err := initPayload(env, token, user, home, t)
	if err != nil {
		return err
	}
	res, err := c.Post(envdAddr+"/init", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("POST /init: status %d", res.StatusCode)
	}
	return nil
}

// userCmd builds a background command run as the default user in workdir (HOME = its home);
// the PID 1 loop reaps it.
func userCmd(name string, args []string, env map[string]string, user, home, workdir string, uid, gid int) *exec.Cmd {
	c := exec.Command(name, args...)
	c.Dir = workdir
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Env = append(c.Env, "HOME="+home, "USER="+user, "LOGNAME="+user)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if uid != 0 {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	}
	return c
}

func mergeEnv(env map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(env)+1)
	for a, b := range env {
		out[a] = b
	}
	out[k] = v
	return out
}

// restartHold is how long a restarted envd waits after the ready marker is gone: the readiness probe (every second,
// one failure) turns the pod NotReady and sandbox-proxy's 2s lookup cache expires, so no request reaches the new
// envd before /init has set its access token (envd with no token lets everything through).
// ponytail: timing, not a handshake; if the kubelet's status update ever lags more than ~2s, have sandbox-proxy check
// X-Access-Token on port 49983 itself.
const restartHold = 5 * time.Second

// restartEnvd brings envd back after it exited on its own: unready first, then hold, start, /init, and ready again
// only when /init succeeded (a failed re-init leaves the sandbox NotReady, so closed).
func restartEnvd(start func() *exec.Cmd, reinit func() error, hold time.Duration) *exec.Cmd {
	if err := os.Remove(readyPath); err != nil && !os.IsNotExist(err) {
		log.Printf("remove ready marker: %v", err)
	}
	time.Sleep(hold)
	c := start()
	go func() {
		if err := reinit(); err != nil {
			log.Printf("re-init after restart failed, sandbox stays not ready: %v", err)
			return
		}
		markReady()
	}()
	return c
}

// markReady writes the marker the pod's startup and readiness probes (sandbox-init -ready) check.
func markReady() {
	if err := os.MkdirAll(filepath.Dir(readyPath), 0o755); err != nil {
		log.Printf("mkdir %s: %v", filepath.Dir(readyPath), err)
		return
	}
	if err := os.WriteFile(readyPath, nil, 0o644); err != nil {
		log.Printf("write ready marker: %v", err)
	}
}

// runMode is what this run of sandbox-init does: "-snapshot", "-ready", or "" (the init). The portal execs it in a
// running sandbox, so anything else must stop before the init's side effects: a second init removes the ready marker
// (the pod drops out of the proxy for good) and fights envd for its port. That happens when a newer portal asks a
// node whose runtime is older for a mode it does not know, e.g. while node-setup rolls.
func runMode(args []string, pid int) (string, error) {
	if len(args) == 1 && args[0] == "--serve" {
		args = nil
	}
	if len(args) > 0 {
		if len(args) == 1 && args[0] == "-prepull" {
			if pid != 1 {
				return "", fmt.Errorf("-prepull requires PID 1")
			}
			return "-prepull", nil
		}
		if args[0] == "-native" {
			if pid != 1 || len(args) < 2 {
				return "", fmt.Errorf("-native requires PID 1 and a command")
			}
			return args[0], nil
		}
		if len(args) == 1 && (args[0] == "-snapshot" || args[0] == "-ready") {
			if pid == 1 {
				return "", fmt.Errorf("utility modes cannot run as PID 1")
			}
			return args[0], nil
		}
		return "", fmt.Errorf("unknown arguments %q", args)
	}
	if pid != 1 {
		return "", fmt.Errorf("the init runs only as PID 1 (this is pid %d)", pid)
	}
	return "", nil
}

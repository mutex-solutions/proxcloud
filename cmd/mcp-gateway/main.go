// Command mcp-gateway is the in-sandbox MCP gateway of the "mcp-gateway" template, compatible
// with the E2B SDKs' `mcp` option: the SDK runs `mcp-gateway --config '<json>'` as root with
// GATEWAY_ACCESS_TOKEN, then talks MCP (streamable HTTP) to :50005/mcp with that bearer. Each
// configured server's tools are offered as "<catalog name>-<tool>", as on E2B.
//
//	mcp-gateway --config '<json>'  write the config, start the gateway in the background, wait until every server is up
//	mcp-gateway serve             the gateway itself (foreground)
//	mcp-gateway boot              the template's start command: restart the gateway after a resume
//	mcp-gateway pull <names…>     build time: check that the named servers can run here
//	mcp-gateway list              the servers this gateway can run
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	addr       = ":50005"
	confDir    = "/etc/mcp-gateway"
	logFile    = "/var/log/mcp-gateway/gateway.log"
	githubRoot = "/opt/mcp/github"
	// startWait stays under the SDK's 60 s command timeout.
	startWait = 50 * time.Second
)

// binPath holds the servers baked into the template image (deploy/templates/mcp-gateway) and the Node they run on,
// ahead of the sandbox's own PATH (its node is the base image's, too old for some servers).
var binPath = "/opt/mcp/node/bin:/opt/mcp/bin:/opt/mcp/npm/bin"

//go:embed catalog.json
var catalogJSON []byte

// entry is one catalog server (gencatalog.py). Remote servers have URL; package servers Cmd.
type entry struct {
	Name      string            `json:"name"`
	Fields    []string          `json:"fields"`
	Required  []string          `json:"required"`
	URL       string            `json:"url"`
	Transport string            `json:"transport"`
	Headers   map[string]string `json:"headers"`
	Cmd       []string          `json:"cmd"`
	Env       map[string]string `json:"env"`
}

// githubServer is a "github/<owner>/<repo>" config value.
type githubServer struct {
	RunCmd     string            `json:"run_cmd"`
	InstallCmd string            `json:"install_cmd"`
	Envs       map[string]string `json:"envs"`
}

func catalog() map[string]entry {
	var c map[string]entry
	if err := json.Unmarshal(catalogJSON, &c); err != nil {
		panic(err)
	}
	return c
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("mcp-gateway: ")
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 2 && args[0] == "--config":
		err = start(args[1])
	case len(args) == 1 && strings.HasPrefix(args[0], "--config="):
		err = start(strings.TrimPrefix(args[0], "--config="))
	case len(args) == 1 && args[0] == "serve":
		err = serve()
	case len(args) == 1 && args[0] == "boot":
		err = boot()
	case len(args) >= 1 && args[0] == "pull":
		err = pull(args[1:])
	case len(args) == 1 && args[0] == "list":
		c := catalog()
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println(strings.Join(keys, "\n"))
	default:
		err = errors.New("usage: mcp-gateway --config '<json>' | serve | boot | pull <names…> | list")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ---- config -----------------------------------------------------------------------------------

// upstream is a resolved server to connect to.
type upstream struct {
	prefix string // tool name prefix (catalog name, or the repo for github servers)
	e      entry  // remote or command, placeholders filled
	github *githubServer
	repo   string // owner/repo
}

var placeholder = regexp.MustCompile(`\{([A-Za-z0-9_]+)(\.\.\.)?\}`)

// fill replaces {field} with the config value; a missing field is an error.
func fill(s string, conf map[string]any) (string, error) {
	var missing string
	out := placeholder.ReplaceAllStringFunc(s, func(m string) string {
		f := placeholder.FindStringSubmatch(m)[1]
		v, ok := conf[f]
		if !ok || v == nil {
			missing = f
			return ""
		}
		if str, ok := v.(string); ok {
			return str
		}
		b, _ := json.Marshal(v)
		return string(b)
	})
	if missing != "" {
		return "", fmt.Errorf("missing %q", missing)
	}
	return out, nil
}

// resolve checks the SDK config against the catalog and fills every server's placeholders.
func resolve(raw map[string]json.RawMessage) ([]upstream, error) {
	cat := catalog()
	var ups []upstream
	var unknown []string
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if repo, ok := strings.CutPrefix(key, "github/"); ok {
			var g githubServer
			if err := json.Unmarshal(raw[key], &g); err != nil || g.RunCmd == "" || strings.Count(repo, "/") != 1 {
				return nil, fmt.Errorf("%s: needs {\"run_cmd\": …} and a github/<owner>/<repo> key", key)
			}
			ups = append(ups, upstream{prefix: strings.ReplaceAll(repo, "/", "-"), github: &g, repo: repo})
			continue
		}
		e, ok := cat[key]
		if !ok {
			unknown = append(unknown, key)
			continue
		}
		conf := map[string]any{}
		if s := string(raw[key]); s != "null" && s != "true" {
			if err := json.Unmarshal(raw[key], &conf); err != nil {
				return nil, fmt.Errorf("%s: the config must be an object", key)
			}
		}
		var err error
		fe := e
		if fe.URL, err = fill(e.URL, conf); err == nil {
			fe.Headers, err = fillMap(e.Headers, conf)
		}
		if err == nil {
			fe.Env, err = fillMap(e.Env, conf)
		}
		if err == nil {
			fe.Cmd, err = fillCmd(e.Cmd, conf)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %v (fields: %s)", key, err, strings.Join(e.Fields, ", "))
		}
		ups = append(ups, upstream{prefix: e.Name, e: fe})
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("%s not available on ProxCloud (run `mcp-gateway list` in the sandbox for the servers it offers)", strings.Join(unknown, ", "))
	}
	return ups, nil
}

func fillMap(m map[string]string, conf map[string]any) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := map[string]string{}
	for k, v := range m {
		s, err := fill(v, conf)
		if err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, nil
}

func fillCmd(cmd []string, conf map[string]any) ([]string, error) {
	var out []string
	for _, a := range cmd {
		if m := placeholder.FindStringSubmatch(a); m != nil && m[2] == "..." && m[0] == a {
			list, _ := conf[m[1]].([]any)
			if s, ok := conf[m[1]].(string); ok {
				list = []any{s}
			}
			if len(list) == 0 {
				return nil, fmt.Errorf("missing %q", m[1])
			}
			for _, v := range list {
				out = append(out, fmt.Sprint(v))
			}
			continue
		}
		s, err := fill(a, conf)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ---- start / boot -----------------------------------------------------------------------------

// start validates and saves the config, starts `serve` in the background and waits for it.
func start(cfg string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cfg), &raw); err != nil {
		return fmt.Errorf("the config must be a JSON object: %v", err)
	}
	if _, err := resolve(raw); err != nil {
		return err
	}
	tok := os.Getenv("GATEWAY_ACCESS_TOKEN")
	if tok == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		tok = hex.EncodeToString(b)
	}
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(confDir, ".token"), []byte(tok), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(confDir, "config.json"), []byte(cfg), 0o600); err != nil {
		return err
	}
	if err := spawn(); err != nil {
		return err
	}
	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		code, body := health()
		switch code {
		case http.StatusOK:
			return nil
		case http.StatusInternalServerError:
			return fmt.Errorf("%s\n%s", body, tail(logFile, 4096))
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the MCP servers did not start within %s\n%s", startWait, tail(logFile, 4096))
}

// boot restarts the gateway when a config exists (after a resume or fork: processes are gone).
func boot() error {
	if _, err := os.Stat(filepath.Join(confDir, "config.json")); err != nil {
		return nil
	}
	return spawn()
}

// spawn starts `mcp-gateway serve` in its own session with output to the log file, so the
// caller's pipes close and envd reports the command as finished.
func spawn() error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "serve")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func health() (int, string) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1" + addr + "/health")
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, string(b)
}

func tail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > n {
		_, _ = f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

// ---- serve ------------------------------------------------------------------------------------

type state struct {
	mu    sync.Mutex
	ready bool
	err   error
}

func serve() error {
	cfg, err := os.ReadFile(filepath.Join(confDir, "config.json"))
	if err != nil {
		return err
	}
	tok, err := os.ReadFile(filepath.Join(confDir, ".token"))
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &raw); err != nil {
		return err
	}
	ups, err := resolve(raw)
	if err != nil {
		return err
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "proxcloud-mcp-gateway", Version: "1"}, nil)
	st := &state{}
	go func() {
		err := connectAll(context.Background(), srv, ups)
		st.mu.Lock()
		st.ready, st.err = err == nil, err
		st.mu.Unlock()
		if err != nil {
			log.Printf("start failed: %v", err)
		} else {
			log.Printf("ready: %d servers", len(ups))
		}
	}()
	return http.ListenAndServe(addr, handler(srv, strings.TrimSpace(string(tok)), st))
}

func handler(srv *mcp.Server, token string, st *state) http.Handler {
	mcpH := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case st.err != nil:
			http.Error(w, "Failed to start MCP gateway: "+st.err.Error(), http.StatusInternalServerError)
		case !st.ready:
			http.Error(w, "starting", http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	})
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpH.ServeHTTP(w, r)
	}))
	return mux
}

// connectAll connects every upstream and re-offers its tools under the prefix.
func connectAll(ctx context.Context, srv *mcp.Server, ups []upstream) error {
	errs := make([]error, len(ups))
	var wg sync.WaitGroup
	for i, u := range ups {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, startWait-5*time.Second)
			defer cancel()
			if err := connect(cctx, srv, u); err != nil {
				errs[i] = fmt.Errorf("%s: %w", u.prefix, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// headerRT adds a remote server's headers (the tenant's key for it) to requests for that server's own origin only. The
// HTTP client follows redirects through this transport, so without the check a redirect to another host, or to plain
// http, would get the key too.
type headerRT struct {
	origin string // scheme://host of the configured URL
	h      map[string]string
	rt     http.RoundTripper
}

func (t headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if len(t.h) == 0 || r.URL.Scheme+"://"+r.URL.Host != t.origin {
		return t.rt.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	for k, v := range t.h {
		r.Header.Set(k, v)
	}
	return t.rt.RoundTrip(r)
}

func transportFor(u upstream) (mcp.Transport, error) {
	if u.github != nil {
		dir := filepath.Join(githubRoot, u.repo)
		if _, err := os.Stat(dir); err != nil {
			gc := exec.Command("git", "clone", "--depth=1", "https://github.com/"+u.repo+".git", dir)
			gc.Env = childEnv(nil)
			out, err := gc.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("git clone: %v: %s", err, out)
			}
			if u.github.InstallCmd != "" {
				c := exec.Command("sh", "-c", u.github.InstallCmd)
				c.Dir, c.Env = dir, childEnv(u.github.Envs)
				if out, err := c.CombinedOutput(); err != nil {
					return nil, fmt.Errorf("install_cmd: %v: %s", err, out)
				}
			}
		}
		c := exec.Command("sh", "-c", u.github.RunCmd)
		c.Dir, c.Env, c.Stderr = dir, childEnv(u.github.Envs), os.Stderr
		return &mcp.CommandTransport{Command: c}, nil
	}
	if u.e.URL != "" {
		pu, err := url.Parse(u.e.URL)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("bad server URL %q", u.e.URL)
		}
		hc := &http.Client{Transport: headerRT{pu.Scheme + "://" + pu.Host, u.e.Headers, http.DefaultTransport}}
		if u.e.Transport == "sse" {
			return &mcp.SSEClientTransport{Endpoint: u.e.URL, HTTPClient: hc}, nil
		}
		return &mcp.StreamableClientTransport{Endpoint: u.e.URL, HTTPClient: hc}, nil
	}
	// Resolve on the servers' PATH: exec.Command alone would look only at the gateway's own.
	bin, err := lookPath(u.e.Cmd[0])
	if err != nil {
		return nil, fmt.Errorf("%s is not installed in this sandbox", u.e.Cmd[0])
	}
	c := exec.Command(bin, u.e.Cmd[1:]...)
	c.Env, c.Stderr = childEnv(u.e.Env), os.Stderr
	return &mcp.CommandTransport{Command: c}, nil
}

// passEnv is what a server may take from the gateway's environment: where it runs, locale, proxies and CA files. The
// rest stays with the gateway: GATEWAY_ACCESS_TOKEN (the bearer for every tool), the other servers' keys and the
// sandbox's envVars. A server gets these, telemetryOff and its own catalog env (or a github server's envs).
// Environment only: servers run as root like the gateway, so they can still read its files and /proc/<pid>/environ.
var passEnv = []string{"HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TZ", "TERM", "TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"}

// telemetryOff turns off the usage reporting some package servers do by default (a server's own env can set it again):
// desktopCommander's analytics, mongodb's telemetry, markitdown's onnxruntime, and DO_NOT_TRACK for the rest.
var telemetryOff = map[string]string{
	"DO_NOT_TRACK":                        "1",
	"DESKTOP_COMMANDER_DISABLE_TELEMETRY": "1",
	"MDB_MCP_TELEMETRY":                   "disabled",
	"ORT_DISABLE_TELEMETRY":               "1",
}

func childEnv(extra map[string]string) []string {
	m := map[string]string{"PATH": binPath + ":" + os.Getenv("PATH")}
	for _, k := range passEnv {
		if v, ok := os.LookupEnv(k); ok {
			m[k] = v
		}
	}
	maps.Copy(m, telemetryOff)
	maps.Copy(m, extra)
	env := make([]string, 0, len(m))
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

func lookPath(name string) (string, error) {
	for _, d := range strings.Split(binPath+":"+os.Getenv("PATH"), ":") {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

func connect(ctx context.Context, srv *mcp.Server, u upstream) error {
	t, err := transportFor(u)
	if err != nil {
		return err
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: "proxcloud-mcp-gateway", Version: "1"}, nil)
	// Not ctx: an SSE transport ties its stream to the connect context, and the session must
	// outlive the start. A hung initialize shows up as `--config` timing out.
	cs, err := cl.Connect(context.Background(), t, nil)
	if err != nil {
		return err
	}
	n := 0
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		up := tool.Name
		t := *tool
		t.Name = u.prefix + "-" + up
		if t.InputSchema == nil {
			t.InputSchema = map[string]any{"type": "object"}
		}
		srv.AddTool(&t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return cs.CallTool(ctx, &mcp.CallToolParams{Name: up, Arguments: req.Params.Arguments})
		})
		n++
	}
	log.Printf("%s: %d tools", u.prefix, n)
	return nil
}

// ---- pull -------------------------------------------------------------------------------------

// pull checks at template build time that each named server can run here: remote servers need nothing, package
// servers must be installed in the image and answer MCP initialize and tools/list (an installed server whose runtime
// is too old only fails when it starts).
func pull(names []string) error {
	cat := catalog()
	for _, n := range names {
		e, ok := cat[n]
		switch {
		case !ok:
			return fmt.Errorf("%s not available on ProxCloud", n)
		case e.URL != "":
		default:
			if _, err := lookPath(e.Cmd[0]); err != nil {
				return fmt.Errorf("%s: %s is not installed in this image", n, e.Cmd[0])
			}
			if err := probe(e); err != nil {
				return fmt.Errorf("%s: does not start: %w", n, err)
			}
		}
		fmt.Println("ok", n)
	}
	return nil
}

// probeValues are placeholders a server validates the shape of at start; every other field gets a directory.
var probeValues = map[string]any{"mdbMcpConnectionString": "mongodb://127.0.0.1:1/probe"}

// probe starts a package server with a placeholder for every config field (no real key: the servers only check
// their keys on a call) and lists its tools.
func probe(e entry) error {
	conf := map[string]any{}
	for _, f := range e.Fields {
		conf[f] = os.TempDir()
		if v, ok := probeValues[f]; ok {
			conf[f] = v
		}
	}
	cmd, err := fillCmd(e.Cmd, conf)
	if err != nil {
		return err
	}
	env, err := fillMap(e.Env, conf)
	if err != nil {
		return err
	}
	t, err := transportFor(upstream{prefix: e.Name, e: entry{Cmd: cmd, Env: env}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "proxcloud-mcp-gateway", Version: "1"}, nil).Connect(ctx, t, nil)
	if err != nil {
		return err
	}
	defer cs.Close()
	_, err = cs.ListTools(ctx, nil)
	return err
}

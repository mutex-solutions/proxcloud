package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	envPrefix  = "PROXCLOUD_"
	readyPath  = "/run/proxcloud/ready"
	defaultUsr = "user"
)

// perUserEnv are set by envd for each process from the user running it (or describe this init
// process only), so they must not be pinned as sandbox-wide defaults: HOME=/root from the
// container would otherwise leak into every `user` process.
var perUserEnv = map[string]bool{"HOME": true, "USER": true, "LOGNAME": true, "HOSTNAME": true, "PWD": true, "SHLVL": true, "_": true}

// splitEnv separates user env (returned as a map: the image ENV plus the caller's envVars, which
// become envd's defaults) from PROXCLOUD_* control vars (dropped). The returned slice is envd's own
// environment: everything except PROXCLOUD_* entries.
func splitEnv(environ []string) (map[string]string, []string) {
	user := map[string]string{}
	var list []string
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.HasPrefix(k, envPrefix) {
			continue
		}
		list = append(list, kv)
		if !perUserEnv[k] {
			user[k] = v
		}
	}
	return user, list
}

// initPayload is the JSON body for envd's POST /init (envd/spec/envd.yaml). Empty token omitted.
func initPayload(env map[string]string, token, user, workdir string, now time.Time) ([]byte, error) {
	m := map[string]any{
		"envVars":        env,
		"defaultUser":    user,
		"defaultWorkdir": workdir,
		"timestamp":      now.UTC().Format(time.RFC3339),
	}
	if token != "" {
		m["accessToken"] = token
	}
	return json.Marshal(m)
}

// idsUsed returns the set of numeric ids in field idx (2 for passwd uid, 2 for group gid) and
// whether name is already present in field 0, plus the matching line's fields.
func scanColon(path, name string) (used map[int]bool, found []string, err error) {
	used = map[int]bool{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return used, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(sc.Text(), ":")
		if len(p) < 3 {
			continue
		}
		if n, err := strconv.Atoi(p[2]); err == nil {
			used[n] = true
		}
		if p[0] == name && found == nil {
			found = p
		}
	}
	return used, found, sc.Err()
}

func appendLine(path, line string) error {
	b, _ := os.ReadFile(path)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		line = "\n" + line
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

var chown = os.Chown // overridden in tests (unprivileged)

// ensureUser makes sure name has a passwd entry, creating user:x:<uid>:<gid>::/home/user:/bin/sh
// (first uid>=1000 free in both files) plus a same-named group and the home dir. It returns
// uid, gid and home. Existing entries are returned untouched.
func ensureUser(passwd, group, homeRoot, name string) (uid, gid int, home string, err error) {
	usedU, found, err := scanColon(passwd, name)
	if err != nil {
		return 0, 0, "", err
	}
	if found != nil && len(found) >= 6 {
		uid, _ = strconv.Atoi(found[2])
		gid, _ = strconv.Atoi(found[3])
		return uid, gid, found[5], nil
	}
	usedG, grp, err := scanColon(group, name)
	if err != nil {
		return 0, 0, "", err
	}
	id := 1000
	for usedU[id] || (grp == nil && usedG[id]) {
		id++
	}
	uid, gid = id, id
	if grp != nil {
		if g, e := strconv.Atoi(grp[2]); e == nil {
			gid = g
		}
	}
	home = homeRoot + "/" + name
	if grp == nil {
		if err := appendLine(group, fmt.Sprintf("%s:x:%d:", name, gid)); err != nil {
			return 0, 0, "", err
		}
	}
	if err := appendLine(passwd, fmt.Sprintf("%s:x:%d:%d::%s:/bin/sh", name, uid, gid, home)); err != nil {
		return 0, 0, "", err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return 0, 0, "", err
	}
	if err := chown(home, uid, gid); err != nil {
		return 0, 0, "", err
	}
	return uid, gid, home, nil
}

// readyCheck implements `sandbox-init -ready`: exit code 0 iff the marker exists.
func readyCheck(path string) int {
	if _, err := os.Stat(path); err != nil {
		return 1
	}
	return 0
}

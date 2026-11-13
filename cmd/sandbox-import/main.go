// sandbox-import is a trusted worker. It never executes customer code or logs credentials.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

func credential(path, host string) (authn.AuthConfig, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return authn.AuthConfig{}, errors.New("required registry credential is unavailable")
	}
	return sandboximport.DockerCredential(b, host)
}
func sourceCredential(path, host string) (authn.AuthConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return authn.AuthConfig{}, errors.New("required source auth configuration is unavailable")
	}
	return sandboximport.SourceCredential(b, host)
}

// mirror is the copy mode for a built-in sandbox template image (docs/template-updates.md §5): argv is
// "mirror <ghcr source@digest> <registry>/library/sandbox-<name>:<tag> <policy>"; the source credential is the
// pod-owned Secret at /source (a short-lived GHCR registry token, never the PAT), the destination the Zot mirror
// account at /push.
func mirror() error {
	source, dest := os.Args[2], os.Args[3]
	policy, err := sandboximport.ParseMirrorPolicy(os.Args[4])
	if err != nil {
		return err
	}
	if _, _, err = policy.Check(source, dest); err != nil {
		return err
	}
	raw, err := os.ReadFile("/source/config.json")
	if err != nil {
		return errors.New("required source registry token is unavailable")
	}
	src, err := sandboximport.MirrorSourceCredential(raw)
	if err != nil {
		return err
	}
	push, err := credential("/push/config.json", policy.Registry)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 29*time.Minute)
	defer cancel()
	result, err := sandboximport.RunMirror(ctx, policy, source, dest, src, push)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func run() error {
	if len(os.Args) != 5 {
		return errors.New("expected project, immutable source, destination and operator policy")
	}
	if os.Args[1] == sandboximport.MirrorMode {
		return mirror()
	}
	project, source, dest := os.Args[1], os.Args[2], os.Args[3]
	policy, err := sandboximport.ParsePolicy(os.Args[4])
	if err != nil {
		return err
	}
	canonical, err := policy.Source(source, project)
	if err != nil {
		return err
	}
	registryHost, _, _ := strings.Cut(policy.TemplateRegistry, "/")
	pull, err := credential("/pull/config.json", registryHost)
	if err != nil {
		return err
	}
	var src, push authn.AuthConfig
	if !canonical {
		host, _, _ := strings.Cut(source, "/")
		src, err = sourceCredential("/source/config.json", host)
		if err != nil {
			return err
		}
		push, err = credential("/push/config.json", registryHost)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 29*time.Minute)
	defer cancel()
	result, err := sandboximport.Run(ctx, policy, project, source, dest, src, push, pull)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
func main() {
	if err := run(); err != nil {
		message := err.Error()
		if len(message) > 1000 {
			message = "image import failed"
		}
		_ = os.WriteFile("/dev/termination-log", []byte(message), 0600)
		fmt.Fprintln(os.Stderr, message)
		os.Exit(1)
	}
}

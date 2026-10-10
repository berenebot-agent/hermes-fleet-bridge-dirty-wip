package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Compose drives each fleet member through its own Docker Compose project. A
// member is identified by its compose project directory; the ephemeral container
// is never the source of truth.
type Compose struct {
	exec func(ctx context.Context, args ...string) ([]byte, error)
}

func newCompose() *Compose {
	return &Compose{exec: dockerComposeExec}
}

// dockerComposeExec runs `docker compose ...` via the mounted socket.
func dockerComposeExec(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.Bytes(), nil
}

func composeFile(dir string) (string, error) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no compose file in %s", dir)
}

func (c *Compose) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	file, err := composeFile(dir)
	if err != nil {
		return nil, err
	}
	return c.exec(ctx, append([]string{"-f", file, "--project-directory", dir}, args...)...)
}

// container is one entry of `compose ps --format json`.
type container struct {
	Name    string `json:"Name"`
	State   string `json:"State"`
	Service string `json:"Service"`
}

// status reports whether any container of the project is running, and the name
// of the primary container (first running one, else the first one known).
func (c *Compose) status(ctx context.Context, dir string) (running bool, primary string, err error) {
	out, err := c.run(ctx, dir, "ps", "-a", "--format", "json")
	if err != nil {
		return false, "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ct container
		if err := json.Unmarshal([]byte(line), &ct); err != nil {
			continue
		}
		if primary == "" {
			primary = ct.Name
		}
		if strings.EqualFold(ct.State, "running") {
			if running {
				continue
			}
			running = true
			primary = ct.Name
		}
	}
	return running, primary, nil
}

func (c *Compose) up(ctx context.Context, dir string, forceRecreate bool) error {
	args := []string{"up", "-d", "--remove-orphans"}
	if forceRecreate {
		args = append(args, "--force-recreate")
	}
	_, err := c.run(ctx, dir, args...)
	return err
}

// down stops and removes the project's containers. Persistent state lives in the
// bind mounts, so this is safe and is the bridge's idle action.
func (c *Compose) down(ctx context.Context, dir string) error {
	_, err := c.run(ctx, dir, "down")
	return err
}

// dataDir finds the host path of the member's persistent Hermes home, i.e. the
// bind mount whose target is /opt/data, by resolving its compose config. It
// falls back to <project>/data so a missing/odd config still fails safe.
func (c *Compose) dataDir(ctx context.Context, dir string) (string, error) {
	out, err := c.run(ctx, dir, "config", "--format", "json")
	if err != nil {
		return "", err
	}
	var cfg struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `json:"type"`
				Source string `json:"source"`
				Target string `json:"target"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return "", fmt.Errorf("parse compose config for %s: %w", dir, err)
	}
	for _, svc := range cfg.Services {
		for _, v := range svc.Volumes {
			if v.Type == "bind" && v.Target == "/opt/data" && v.Source != "" {
				return v.Source, nil
			}
		}
	}
	return filepath.Join(dir, "data"), nil
}

// ensureAPIEnv makes the member's Hermes API server reachable and authenticated.
// Hermes loads its persistent <data>/.env with override=true, so that file — not
// the Docker environment — is the only reliable place to set these. It returns
// the effective key and whether the file changed (a change needs a restart for
// the running gateway to pick it up).
func (c *Compose) ensureAPIEnv(ctx context.Context, dir, port, fallbackKey string) (key string, changed bool, err error) {
	data, err := c.dataDir(ctx, dir)
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(data, ".env")
	content, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	existing := envValue(string(content), "API_SERVER_KEY")
	key = fallbackKey
	if usableKey(existing) {
		key = existing
	}
	if !usableKey(key) {
		key, err = randomKey()
		if err != nil {
			return "", false, err
		}
	}
	want := []kv{
		{"API_SERVER_ENABLED", "true"},
		{"API_SERVER_HOST", "0.0.0.0"},
		{"API_SERVER_PORT", port},
		{"API_SERVER_KEY", key},
	}
	updated, changed := upsertEnv(string(content), want)
	if !changed {
		return key, false, nil
	}
	if err := os.MkdirAll(data, 0700); err != nil {
		return "", false, err
	}
	if err := writeFileAtomic(path, []byte(updated), 0600); err != nil {
		return "", false, err
	}
	return key, true, nil
}

type kv struct{ k, v string }

func envValue(content, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(t, prefix))
		}
	}
	return ""
}

// upsertEnv sets each key in place (preserving order and comments) or appends it.
// It reports the new content and whether anything actually changed.
func upsertEnv(content string, kvs []kv) (string, bool) {
	lines := strings.Split(content, "\n")
	trailing := false
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		trailing = true
		lines = lines[:len(lines)-1]
	}
	found := make(map[string]bool, len(kvs))
	changed := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		for _, item := range kvs {
			if strings.HasPrefix(t, item.k+"=") {
				found[item.k] = true
				if strings.TrimRight(line, " \t") != item.k+"="+item.v {
					lines[i] = item.k + "=" + item.v
					changed = true
				}
			}
		}
	}
	for _, item := range kvs {
		if !found[item.k] {
			lines = append(lines, item.k+"="+item.v)
			changed = true
		}
	}
	out := strings.Join(lines, "\n")
	if trailing || changed {
		out += "\n"
	}
	return out, changed
}

func usableKey(k string) bool { return len(strings.TrimSpace(k)) >= 16 }

func randomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

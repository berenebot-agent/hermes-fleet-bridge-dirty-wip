package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// errContainerNotFound is returned by inspect when Docker reports HTTP 404.
var errContainerNotFound = errors.New("container not found")

type Docker struct{ client *http.Client }
type ContainerInfo struct {
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Env []string `json:"Env"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func newDocker(socket string) *Docker {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	return &Docker{client: &http.Client{Transport: tr, Timeout: 15 * time.Second}}
}
func (d *Docker) request(ctx context.Context, method, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1048576))
	return resp.StatusCode, b, err
}
func (d *Docker) inspect(ctx context.Context, name string) (ContainerInfo, error) {
	var c ContainerInfo
	status, b, err := d.request(ctx, "GET", "/containers/"+url.PathEscape(name)+"/json")
	if err != nil {
		return c, err
	}
	if status == 404 {
		return c, fmt.Errorf("%w: %s", errContainerNotFound, name)
	}
	if status != 200 {
		return c, fmt.Errorf("Docker inspect %s: HTTP %d: %.160s", name, status, b)
	}
	err = json.Unmarshal(b, &c)
	return c, err
}
func (d *Docker) start(ctx context.Context, name string) error {
	status, b, err := d.request(ctx, "POST", "/containers/"+url.PathEscape(name)+"/start")
	if err != nil {
		return err
	}
	if status != 204 && status != 304 {
		return fmt.Errorf("Docker start %s: HTTP %d: %.160s", name, status, b)
	}
	return nil
}
func (d *Docker) stop(ctx context.Context, name string) error {
	status, b, err := d.request(ctx, "POST", "/containers/"+url.PathEscape(name)+"/stop?t=10")
	if err != nil {
		return err
	}
	if status != 204 && status != 304 {
		return fmt.Errorf("Docker stop %s: HTTP %d: %.160s", name, status, b)
	}
	return nil
}
func infoEnv(info ContainerInfo, k string) string {
	prefix := k + "="
	for _, v := range info.Config.Env {
		if strings.HasPrefix(v, prefix) {
			return strings.TrimPrefix(v, prefix)
		}
	}
	return ""
}

// On Linux with host networking, reach the inspected container via its Docker bridge IP.
// Never trust or use container-supplied DNS names to construct an arbitrary destination.
// The API key is supplied separately (the bridge owns it via the member's .env).
func (c Config) hermesEndpoint(info ContainerInfo) (string, error) {
	port := c.DefaultPort
	if p := infoEnv(info, "API_SERVER_PORT"); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil || v < 1 || v > 65535 {
			return "", fmt.Errorf("invalid API_SERVER_PORT in %s", info.Name)
		}
		port = v
	}
	var host string
	if info.HostConfig.NetworkMode == "host" {
		host = "127.0.0.1"
	} else {
		if c.PreferredNetwork != "" {
			if x, ok := info.NetworkSettings.Networks[c.PreferredNetwork]; ok {
				host = x.IPAddress
			} else {
				return "", fmt.Errorf("container %s not on HERMES_NETWORK=%s", info.Name, c.PreferredNetwork)
			}
		}
		if host == "" {
			keys := make([]string, 0, len(info.NetworkSettings.Networks))
			for k := range info.NetworkSettings.Networks {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if ip := info.NetworkSettings.Networks[k].IPAddress; net.ParseIP(ip) != nil {
					host = ip
					break
				}
			}
		}
	}
	if net.ParseIP(host) == nil {
		return "", fmt.Errorf("container %s has no reachable IPv4 address; use Docker bridge or host networking", info.Name)
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)), nil
}

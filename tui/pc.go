package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PCConfig holds the Prism Central connection. It is read from ~/.cursor/mcp.json
// by default, but may be overridden via the TUI's Nutanix settings (which can
// also supply a user/password service account instead of an API key).
type PCConfig struct {
	Host     string
	Port     string
	APIKey   string
	User     string
	Password string
}

// authHeader sets the appropriate auth header on a PC request: the API key when
// present, otherwise HTTP basic auth from the user/password service account.
func (c *PCConfig) authHeader(req *http.Request) {
	if strings.TrimSpace(c.APIKey) != "" {
		req.Header.Set("X-Ntnx-Api-Key", c.APIKey)
		return
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
}

// LoadPCConfig reads PC connection details from the user's Cursor MCP config.
// The API key is never stored in source; it is read at runtime.
func LoadPCConfig() *PCConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	for _, key := range []string{"nutanix-v4-mcp", "nutanix"} {
		srv, ok := doc.MCPServers[key]
		if !ok {
			continue
		}
		host := first(srv.Env["PC_HOST"], srv.Env["PRISM_CENTRAL_HOST"])
		apiKey := first(srv.Env["PC_API_KEY"], srv.Env["PRISM_CENTRAL_API_KEY"])
		if host != "" && apiKey != "" {
			port := srv.Env["PC_PORT"]
			if port == "" {
				port = "9440"
			}
			return &PCConfig{Host: host, Port: port, APIKey: apiKey}
		}
	}
	return nil
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// PCClient performs read-only Prism Central v4 API calls.
type PCClient struct {
	cfg  *PCConfig
	base string
	http *http.Client
}

func NewPCClient(cfg *PCConfig) *PCClient {
	return &PCClient{
		cfg:  cfg,
		base: fmt.Sprintf("https://%s:%s", cfg.Host, cfg.Port),
		http: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// VM is a condensed view of a Prism Central VM.
type VM struct {
	Name       string
	ExtID      string
	Power      string
	IP         string
	VCPU       int
	MemGiB     float64
	DiskGiB    float64
	Role       string
}

type rawVM struct {
	Name             string `json:"name"`
	ExtID            string `json:"extId"`
	PowerState       string `json:"powerState"`
	NumSockets       int    `json:"numSockets"`
	NumCoresPerSock  int    `json:"numCoresPerSocket"`
	MemorySizeBytes  int64  `json:"memorySizeBytes"`
	Nics             []struct {
		NetworkInfo    *nicNet `json:"networkInfo"`
		NicNetworkInfo *nicNet `json:"nicNetworkInfo"`
	} `json:"nics"`
	Disks []struct {
		BackingInfo struct {
			DiskSizeBytes int64 `json:"diskSizeBytes"`
		} `json:"backingInfo"`
	} `json:"disks"`
}

type nicNet struct {
	IPv4Config *struct {
		IPAddress struct {
			Value string `json:"value"`
		} `json:"ipAddress"`
	} `json:"ipv4Config"`
	IPv4Info *struct {
		LearnedIPAddresses []struct {
			Value string `json:"value"`
		} `json:"learnedIpAddresses"`
	} `json:"ipv4Info"`
}

func (c *PCClient) ListVMs() ([]VM, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/vmm/v4.2/ahv/config/vms?$limit=100", nil)
	c.cfg.authHeader(req)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("list vms %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data []rawVM `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	vms := make([]VM, 0, len(out.Data))
	for _, r := range out.Data {
		vms = append(vms, summarizeVM(r))
	}
	return vms, nil
}

func (c *PCClient) ClusterNames() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/clustermgmt/v4.2/config/clusters?$limit=50", nil)
	c.cfg.authHeader(req)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	var names []string
	for _, c := range out.Data {
		if c.Name != "" && strings.ToUpper(c.Name) != "UNNAMED" {
			names = append(names, c.Name)
		}
	}
	return names
}

// ImageNames lists the names of DISK images on Prism Central (ISO images are
// skipped since deploys clone a disk image). Returns nil on any error so the
// settings form falls back to free-text entry.
func (c *PCClient) ImageNames() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/vmm/v4.2/content/images?$limit=100", nil)
	c.cfg.authHeader(req)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	var names []string
	for _, img := range out.Data {
		if img.Name == "" || strings.EqualFold(img.Type, "ISO_IMAGE") {
			continue
		}
		names = append(names, img.Name)
	}
	return names
}

// SubnetNames lists the names of subnets on Prism Central. Returns nil on any
// error so the settings form falls back to free-text entry.
func (c *PCClient) SubnetNames() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/networking/v4.2/config/subnets?$limit=100", nil)
	c.cfg.authHeader(req)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	var names []string
	for _, s := range out.Data {
		if s.Name != "" {
			names = append(names, s.Name)
		}
	}
	return names
}

func summarizeVM(r rawVM) VM {
	ip := "-"
	for _, n := range r.Nics {
		info := n.NetworkInfo
		if info == nil {
			info = n.NicNetworkInfo
		}
		if info == nil {
			continue
		}
		if info.IPv4Config != nil && info.IPv4Config.IPAddress.Value != "" {
			ip = info.IPv4Config.IPAddress.Value
			break
		}
		if info.IPv4Info != nil {
			for _, l := range info.IPv4Info.LearnedIPAddresses {
				if l.Value != "" {
					ip = l.Value
					break
				}
			}
		}
	}
	var diskBytes int64
	for _, d := range r.Disks {
		if d.BackingInfo.DiskSizeBytes > diskBytes {
			diskBytes = d.BackingInfo.DiskSizeBytes
		}
	}
	return VM{
		Name:    r.Name,
		ExtID:   r.ExtID,
		Power:   r.PowerState,
		IP:      ip,
		VCPU:    r.NumSockets * r.NumCoresPerSock,
		MemGiB:  float64(r.MemorySizeBytes) / (1024 * 1024 * 1024),
		DiskGiB: float64(diskBytes) / (1024 * 1024 * 1024),
		Role:    vmRole(r.Name),
	}
}

func vmRole(name string) string {
	n := strings.ToLower(name)
	if strings.Contains(n, "worker") || strings.Contains(n, "ollama-") {
		return "worker"
	}
	if strings.Contains(n, "gateway") || strings.HasPrefix(n, "olla-") {
		return "gateway"
	}
	return "vm"
}

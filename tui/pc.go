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
	ImageExtID string // source image extId, when the disk still references it
}

type rawVM struct {
	Name            string `json:"name"`
	ExtID           string `json:"extId"`
	PowerState      string `json:"powerState"`
	NumSockets      int    `json:"numSockets"`
	NumCoresPerSock int    `json:"numCoresPerSocket"`
	MemorySizeBytes int64  `json:"memorySizeBytes"`
	Nics            []struct {
		NetworkInfo    *nicNet `json:"networkInfo"`
		NicNetworkInfo *nicNet `json:"nicNetworkInfo"`
	} `json:"nics"`
	Disks []struct {
		BackingInfo struct {
			DiskSizeBytes int64 `json:"diskSizeBytes"`
			DataSource    *struct {
				Reference *struct {
					ImageExtID string `json:"imageExtId"`
				} `json:"reference"`
			} `json:"dataSource"`
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

// pcAPIVersions is the set of Nutanix v4 API minor versions we try, newest
// first. Different Prism Central releases serve different minors: pc.2024.x
// commonly tops out at v4.0/v4.1 while newer builds add v4.2. Calling a minor a
// PC doesn't serve returns 404, so getVersioned walks this list and uses the
// first that responds — instead of hardcoding v4.2 and silently getting nothing.
var pcAPIVersions = []string{"v4.2", "v4.1", "v4.0"}

// getVersioned performs a GET against pathTmpl (which must contain a single %s
// for the API version), trying each supported minor in turn and returning the
// body of the first non-404 response. A 404 means "this PC doesn't serve that
// minor" so we fall through; any other 4xx/5xx (e.g. 401 auth) is returned
// immediately since retrying another version won't help.
func (c *PCClient) getVersioned(pathTmpl string) ([]byte, error) {
	var lastErr error
	for _, v := range pcAPIVersions {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+fmt.Sprintf(pathTmpl, v), nil)
		c.cfg.authHeader(req)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			cancel()
			return nil, err // transport errors are version-independent: fail fast
		}
		if resp.StatusCode == http.StatusNotFound {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			cancel()
			lastErr = fmt.Errorf("HTTP 404 (API %s not served)", v)
			continue
		}
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			cancel()
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		}
		b, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if rerr != nil {
			return nil, rerr
		}
		return b, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no supported v4 API version found")
	}
	return nil, lastErr
}

func (c *PCClient) ListVMs() ([]VM, error) {
	body, err := c.getVersioned("/api/vmm/%s/ahv/config/vms?$limit=100")
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []rawVM `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	vms := make([]VM, 0, len(out.Data))
	for _, r := range out.Data {
		vms = append(vms, summarizeVM(r))
	}
	return vms, nil
}

// listNames runs a v4 "list" GET and returns the .data[].name values. It
// surfaces HTTP/transport errors (instead of swallowing them) so the caller can
// tell "PC said there are none" apart from "the query failed". pathTmpl must
// contain a single %s for the API version (see getVersioned).
func (c *PCClient) listNames(pathTmpl string) ([]string, error) {
	body, err := c.getVersioned(pathTmpl)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	var names []string
	for _, d := range out.Data {
		if d.Name == "" || strings.EqualFold(d.Name, "UNNAMED") || strings.EqualFold(d.Type, "ISO_IMAGE") {
			continue
		}
		names = append(names, d.Name)
	}
	return names, nil
}

func (c *PCClient) ClusterNames() ([]string, error) {
	return c.listNames("/api/clustermgmt/%s/config/clusters?$limit=50")
}

// ImageNames lists the names of DISK images on Prism Central (ISO images are
// skipped since deploys clone a disk image).
func (c *PCClient) ImageNames() ([]string, error) {
	return c.listNames("/api/vmm/%s/content/images?$limit=100")
}

// ImagesByID returns a map of image extId -> name, used to resolve the source
// image of a VM whose disk still references it.
func (c *PCClient) ImagesByID() (map[string]string, error) {
	body, err := c.getVersioned("/api/vmm/%s/content/images?$limit=100")
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			Name  string `json:"name"`
			ExtID string `json:"extId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(out.Data))
	for _, d := range out.Data {
		if d.ExtID != "" {
			m[d.ExtID] = d.Name
		}
	}
	return m, nil
}

// SubnetNames lists the names of subnets on Prism Central.
func (c *PCClient) SubnetNames() ([]string, error) {
	return c.listNames("/api/networking/%s/config/subnets?$limit=100")
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
	imageExtID := ""
	for _, d := range r.Disks {
		if d.BackingInfo.DiskSizeBytes > diskBytes {
			diskBytes = d.BackingInfo.DiskSizeBytes
		}
		if imageExtID == "" && d.BackingInfo.DataSource != nil &&
			d.BackingInfo.DataSource.Reference != nil {
			imageExtID = d.BackingInfo.DataSource.Reference.ImageExtID
		}
	}
	return VM{
		Name:       r.Name,
		ExtID:      r.ExtID,
		Power:      r.PowerState,
		IP:         ip,
		VCPU:       r.NumSockets * r.NumCoresPerSock,
		MemGiB:     float64(r.MemorySizeBytes) / (1024 * 1024 * 1024),
		DiskGiB:    float64(diskBytes) / (1024 * 1024 * 1024),
		Role:       vmRole(r.Name),
		ImageExtID: imageExtID,
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

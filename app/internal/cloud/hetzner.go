// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Package cloud creates and deletes burst workers. Only the calls a burst
// needs are here; nothing reads or changes resources without the m2test label.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Label marks everything a burst made, so gc can find it.
const Label = "m2test"

var ErrNotFound = errors.New("not found")

// Server is a cloud server as far as a burst cares.
type Server struct {
	ID     int64
	Name   string
	IP     string
	Status string
	Labels map[string]string
}

// Spec is what Create asks for.
type Spec struct {
	Name     string
	Type     string
	Location string
	Image    string
	UserData string
	Labels   map[string]string
	SSHKey   int64
	Firewall int64
}

// APIError is an error the provider answered with.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("hetzner: %s (%d): %s", e.Code, e.Status, e.Message)
}

// Refused says the API turned a request down, so it changed nothing. Any
// other error leaves it unknown.
func Refused(err error) bool {
	var api *APIError
	if errors.Is(err, ErrNotFound) {
		return true
	}
	if !errors.As(err, &api) {
		return false
	}
	switch api.Status {
	case 400, 401, 403, 409, 412, 422, 429:
		return true
	}
	return false
}

// Locked says the request was turned down for now (an action running, a
// rate limit), so trying again later may work.
func Locked(err error) bool {
	var api *APIError
	return errors.As(err, &api) && (api.Status == 423 || api.Status == 429)
}

// Hetzner talks to the Hetzner Cloud API.
type Hetzner struct {
	Token    string
	Endpoint string // https://api.hetzner.cloud/v1 unless a test says otherwise
	Client   *http.Client
}

func NewHetzner(token string) *Hetzner {
	return &Hetzner{Token: token, Endpoint: "https://api.hetzner.cloud/v1", Client: &http.Client{Timeout: 60 * time.Second}}
}

func (h *Hetzner) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.Endpoint+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string }
		}
		json.Unmarshal(data, &e)
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
		}
		return &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

type hzServer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

func (s hzServer) server() Server {
	return Server{ID: s.ID, Name: s.Name, IP: s.PublicNet.IPv4.IP, Status: s.Status, Labels: s.Labels}
}

func (h *Hetzner) Create(ctx context.Context, s Spec) (Server, error) {
	body := map[string]any{
		"name":        s.Name,
		"server_type": s.Type,
		"location":    s.Location,
		"image":       s.Image,
		"user_data":   s.UserData,
		"labels":      s.Labels,
		"ssh_keys":    []int64{s.SSHKey},
		"firewalls":   []map[string]int64{{"firewall": s.Firewall}},
		"public_net":  map[string]bool{"enable_ipv4": true, "enable_ipv6": false},
	}
	var out struct {
		Server      hzServer
		Action      action
		NextActions []action `json:"next_actions"`
	}
	if err := h.do(ctx, "POST", "/servers", body, &out); err != nil {
		return Server{}, err
	}
	// The firewall goes on in a next action: until it has, SSH is open.
	if err := h.wait(ctx, append([]action{out.Action}, out.NextActions...)); err != nil {
		return out.Server.server(), fmt.Errorf("server %d made, but %w", out.Server.ID, err)
	}
	return out.Server.server(), nil
}

func (h *Hetzner) Get(ctx context.Context, id int64) (Server, error) {
	var out struct{ Server hzServer }
	err := h.do(ctx, "GET", "/servers/"+strconv.FormatInt(id, 10), nil, &out)
	return out.Server.server(), err
}

func (h *Hetzner) Delete(ctx context.Context, id int64) error {
	err := h.do(ctx, "DELETE", "/servers/"+strconv.FormatInt(id, 10), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (h *Hetzner) List(ctx context.Context, selector string) ([]Server, error) {
	var all []Server
	for page := 1; page > 0; {
		var out struct {
			Servers []hzServer
			Meta    struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				}
			}
		}
		q := url.Values{"label_selector": {selector}, "page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if err := h.do(ctx, "GET", "/servers?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		for _, s := range out.Servers {
			all = append(all, s.server())
		}
		page = 0
		if n := out.Meta.Pagination.NextPage; n != nil {
			page = *n
		}
	}
	return all, nil
}

// ServerType gives a type's gross hourly price in a location, with the
// primary IPv4 each server gets, and its Go architecture.
func (h *Hetzner) ServerType(ctx context.Context, serverType, location string) (price float64, goarch string, err error) {
	var out struct {
		ServerTypes []struct {
			Architecture string
			Prices       []struct {
				Location    string
				PriceHourly struct{ Gross string } `json:"price_hourly"`
			}
		} `json:"server_types"`
	}
	if err := h.do(ctx, "GET", "/server_types?"+url.Values{"name": {serverType}}.Encode(), nil, &out); err != nil {
		return 0, "", err
	}
	if len(out.ServerTypes) != 1 {
		return 0, "", fmt.Errorf("hetzner: no server type %q", serverType)
	}
	t := out.ServerTypes[0]
	switch t.Architecture {
	case "x86":
		goarch = "amd64"
	case "arm":
		goarch = "arm64"
	default:
		return 0, "", fmt.Errorf("hetzner: %s has architecture %q", serverType, t.Architecture)
	}
	ip, err := h.ipv4Price(ctx, location)
	if err != nil {
		return 0, "", err
	}
	for _, p := range t.Prices {
		if p.Location == location {
			price, err = strconv.ParseFloat(p.PriceHourly.Gross, 64)
			return price + ip, goarch, err
		}
	}
	return 0, "", fmt.Errorf("hetzner: %s is not sold in %s", serverType, location)
}

func (h *Hetzner) ipv4Price(ctx context.Context, location string) (float64, error) {
	var out struct {
		Pricing struct {
			Currency   string
			PrimaryIPs []struct {
				Type   string
				Prices []struct {
					Location    string
					PriceHourly struct{ Gross string } `json:"price_hourly"`
				}
			} `json:"primary_ips"`
		}
	}
	if err := h.do(ctx, "GET", "/pricing", nil, &out); err != nil {
		return 0, err
	}
	if out.Pricing.Currency != "EUR" {
		return 0, fmt.Errorf("hetzner: prices are in %q; the cost cap is in euros", out.Pricing.Currency)
	}
	for _, ip := range out.Pricing.PrimaryIPs {
		for _, p := range ip.Prices {
			if ip.Type == "ipv4" && p.Location == location {
				return strconv.ParseFloat(p.PriceHourly.Gross, 64)
			}
		}
	}
	return 0, fmt.Errorf("hetzner: no IPv4 price for %s", location)
}

// SSHKey returns the ID of the pool key's entry, adding it if missing.
// It goes on root only so Hetzner doesn't email a root password; cloud-init then locks root.
func (h *Hetzner) SSHKey(ctx context.Context, name, publicKey string) (int64, error) {
	var out struct {
		SSHKeys []struct {
			ID        int64  `json:"id"`
			PublicKey string `json:"public_key"`
		} `json:"ssh_keys"`
	}
	if err := h.do(ctx, "GET", "/ssh_keys?"+url.Values{"name": {name}}.Encode(), nil, &out); err != nil {
		return 0, err
	}
	for _, k := range out.SSHKeys {
		if sameKey(k.PublicKey, publicKey) {
			return k.ID, nil
		}
		if err := h.do(ctx, "DELETE", "/ssh_keys/"+strconv.FormatInt(k.ID, 10), nil, nil); err != nil {
			return 0, err
		}
	}
	var made struct {
		SSHKey struct {
			ID int64 `json:"id"`
		} `json:"ssh_key"`
	}
	body := map[string]any{"name": name, "public_key": publicKey, "labels": map[string]string{Label: "pool"}}
	if err := h.do(ctx, "POST", "/ssh_keys", body, &made); err != nil {
		return 0, err
	}
	return made.SSHKey.ID, nil
}

func (h *Hetzner) Firewall(ctx context.Context, name string, sources []string) (int64, error) {
	rules := []map[string]any{{"direction": "in", "protocol": "tcp", "port": "22", "source_ips": sources}}
	var out struct {
		Firewalls []struct {
			ID int64 `json:"id"`
		}
	}
	if err := h.do(ctx, "GET", "/firewalls?"+url.Values{"name": {name}}.Encode(), nil, &out); err != nil {
		return 0, err
	}
	var made struct {
		Firewall struct {
			ID int64 `json:"id"`
		}
		Actions []action
	}
	if len(out.Firewalls) == 1 {
		made.Firewall.ID = out.Firewalls[0].ID
		path := "/firewalls/" + strconv.FormatInt(made.Firewall.ID, 10) + "/actions/set_rules"
		if err := h.do(ctx, "POST", path, map[string]any{"rules": rules}, &made); err != nil {
			return 0, err
		}
	} else {
		body := map[string]any{"name": name, "rules": rules, "labels": map[string]string{Label: "firewall"}}
		if err := h.do(ctx, "POST", "/firewalls", body, &made); err != nil {
			return 0, err
		}
	}
	// The rules aren't in force until their actions finish.
	return made.Firewall.ID, h.wait(ctx, made.Actions)
}

type action struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

var (
	ActionPoll = 2 * time.Second
	// A server billing behind an action that never ends must still be let go.
	ActionWait = 10 * time.Minute
)

func (h *Hetzner) wait(ctx context.Context, actions []action) error {
	ctx, cancel := context.WithTimeout(ctx, ActionWait)
	defer cancel()
	for _, a := range actions {
		for a.Status == "running" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(ActionPoll):
			}
			var out struct{ Action action }
			if err := h.do(ctx, "GET", "/actions/"+strconv.FormatInt(a.ID, 10), nil, &out); err != nil {
				return err
			}
			a = out.Action
		}
		if a.Status != "success" {
			msg := a.Status
			if a.Error != nil {
				msg = a.Error.Message
			}
			return fmt.Errorf("hetzner: action %d: %s", a.ID, msg)
		}
	}
	return nil
}

// sameKey compares type and key, as the comment may not survive the API.
func sameKey(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	return len(fa) >= 2 && len(fb) >= 2 && fa[0] == fb[0] && fa[1] == fb[1]
}

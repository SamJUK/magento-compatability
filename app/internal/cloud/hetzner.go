// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

// Package cloud creates and deletes burst workers. Only the calls a burst
// needs are here. Firewalls and keys it changes only if labelled m2test;
// which servers are the burst's is the caller's to check.
package cloud

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var hzID = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// Label marks everything a burst made, so gc can find it.
const Label = "m2test"

var ErrNotFound = errors.New("not found")

type Server struct {
	ID     string // "" until the provider has said
	Name   string
	IP     string
	Status string
	Labels map[string]string
}

type Spec struct {
	Name     string
	Type     string
	Location string
	Image    string
	UserData string
	Labels   map[string]string
	SSHKey   string
	Firewall string
	// AWS only: the image's root device and its size, and the subnets
	// tried in turn while a zone has no capacity.
	RootDevice string
	DiskGB     int32
	Subnets    []string
}

type APIError struct {
	Provider string // hetzner unless set
	Status   int    // how to treat it, as an HTTP status
	Sent     int    // the status as sent, where that differs
	Code     string
	Message  string
}

func (e *APIError) Error() string {
	p := e.Provider
	if p == "" {
		p = "hetzner"
	}
	return fmt.Sprintf("%s: %s (%d): %s", p, e.Code, cmp.Or(e.Sent, e.Status), e.Message)
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
			return fmt.Errorf("%s %s: %w (%s)", method, path, ErrNotFound, e.Error.Message)
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
	return Server{ID: strconv.FormatInt(s.ID, 10), Name: s.Name, IP: s.PublicNet.IPv4.IP, Status: s.Status, Labels: s.Labels}
}

func (h *Hetzner) Create(ctx context.Context, s Spec) (Server, error) {
	body := map[string]any{
		"name":        s.Name,
		"server_type": s.Type,
		"location":    s.Location,
		"image":       s.Image,
		"user_data":   s.UserData,
		"labels":      s.Labels,
		"ssh_keys":    []json.Number{json.Number(s.SSHKey)},
		"firewalls":   []map[string]json.Number{{"firewall": json.Number(s.Firewall)}},
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

func (h *Hetzner) Get(ctx context.Context, id string) (Server, error) {
	if !hzID.MatchString(id) {
		return Server{}, fmt.Errorf("hetzner: bad server id %q", id)
	}
	var out struct{ Server hzServer }
	err := h.do(ctx, "GET", "/servers/"+id, nil, &out)
	return out.Server.server(), err
}

func (h *Hetzner) Delete(ctx context.Context, id string) error {
	if !hzID.MatchString(id) {
		return fmt.Errorf("hetzner: bad server id %q", id)
	}
	err := h.do(ctx, "DELETE", "/servers/"+id, nil, nil)
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
			price, err = parsePrice(p.PriceHourly.Gross)
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
				return parsePrice(p.PriceHourly.Gross)
			}
		}
	}
	return 0, fmt.Errorf("hetzner: no IPv4 price for %s", location)
}

// parsePrice refuses what would slip past the cost cap: NaN compares false.
func parsePrice(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, fmt.Errorf("hetzner: price %q", s)
	}
	return f, nil
}

// SSHKey returns the ID of the pool key's entry, adding it if missing.
// It goes on root only so Hetzner doesn't email a root password; cloud-init then locks root.
func (h *Hetzner) SSHKey(ctx context.Context, name, publicKey string) (string, error) {
	id, err := h.sshKey(ctx, name, publicKey)
	return strconv.FormatInt(id, 10), err
}

func (h *Hetzner) KeyID(ctx context.Context, name string) (string, error) {
	var out struct {
		SSHKeys []struct {
			ID int64 `json:"id"`
		} `json:"ssh_keys"`
	}
	if err := h.do(ctx, "GET", "/ssh_keys?"+url.Values{"name": {name}}.Encode(), nil, &out); err != nil || len(out.SSHKeys) == 0 {
		return "", err
	}
	return strconv.FormatInt(out.SSHKeys[0].ID, 10), nil
}

func (h *Hetzner) sshKey(ctx context.Context, name, publicKey string) (int64, error) {
	var out struct {
		SSHKeys []struct {
			ID        int64             `json:"id"`
			PublicKey string            `json:"public_key"`
			Labels    map[string]string `json:"labels"`
		} `json:"ssh_keys"`
	}
	if err := h.do(ctx, "GET", "/ssh_keys?"+url.Values{"name": {name}}.Encode(), nil, &out); err != nil {
		return 0, err
	}
	for _, k := range out.SSHKeys {
		if sameKey(k.PublicKey, publicKey) {
			return k.ID, nil
		}
		if k.Labels[Label] != "pool" {
			return 0, fmt.Errorf("hetzner: ssh key %s isn't labelled %s=pool; remove it by hand if it's a stale pool key", name, Label)
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

func (h *Hetzner) Firewall(ctx context.Context, name string, sources []string) (string, error) {
	id, err := h.firewall(ctx, name, sources)
	return strconv.FormatInt(id, 10), err
}

func (h *Hetzner) firewall(ctx context.Context, name string, sources []string) (int64, error) {
	rules := []map[string]any{{"direction": "in", "protocol": "tcp", "port": "22", "source_ips": sources}}
	var out struct {
		Firewalls []struct {
			ID     int64             `json:"id"`
			Labels map[string]string `json:"labels"`
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
		if out.Firewalls[0].Labels[Label] != "firewall" {
			return 0, fmt.Errorf("hetzner: firewall %s isn't labelled %s=firewall, so it isn't ours to change", name, Label)
		}
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

// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The bodies below follow the shapes in Hetzner's API reference
// (docs.hetzner.cloud), trimmed to the fields read.

type call struct {
	method, path, query string
	body                map[string]any
}

func fake(t *testing.T, routes map[string]func(w http.ResponseWriter, c call)) (*Hetzner, *[]call) {
	t.Helper()
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s %s without the token", r.Method, r.URL.Path)
		}
		c := call{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			if err := json.Unmarshal(data, &c.body); err != nil {
				t.Errorf("body: %v", err)
			}
		}
		calls = append(calls, c)
		h := routes[r.Method+" "+r.URL.Path]
		if h == nil {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		h(w, c)
	}))
	t.Cleanup(srv.Close)
	h := NewHetzner("tok")
	h.Endpoint = srv.URL + "/v1"
	return h, &calls
}

func write(code int, body string) func(http.ResponseWriter, call) {
	return func(w http.ResponseWriter, _ call) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

const serverJSON = `{"id": 42, "name": "hz-1", "status": "initializing", "labels": {"m2test": "worker"},
	"public_net": {"ipv4": {"id": 1, "ip": "203.0.113.7", "blocked": false}, "ipv6": null}}`

func init() { ActionPoll = time.Millisecond }

func TestCreateSendsTheSpec(t *testing.T) {
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){
		"POST /v1/servers":  write(201, `{"server": `+serverJSON+`, "action": {"id": 1, "status": "running"}, "next_actions": [{"id": 2, "status": "running"}], "root_password": null}`),
		"GET /v1/actions/1": write(200, `{"action": {"id": 1, "status": "success"}}`),
		"GET /v1/actions/2": write(200, `{"action": {"id": 2, "status": "success"}}`),
	})
	s, err := h.Create(context.Background(), Spec{Name: "hz-1", Type: "ccx33", Location: "hel1", Image: "debian-13",
		UserData: "#cloud-config\n", Labels: map[string]string{"m2test": "worker"}, SSHKey: "7", Firewall: "9"})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "42" || s.IP != "203.0.113.7" {
		t.Fatalf("got %+v", s)
	}
	if len(*calls) != 3 {
		t.Fatalf("didn't wait for both actions: %+v", *calls)
	}
	b := (*calls)[0].body
	if b["server_type"] != "ccx33" || b["location"] != "hel1" || b["image"] != "debian-13" || b["user_data"] != "#cloud-config\n" {
		t.Fatalf("body %v", b)
	}
	if got, _ := json.Marshal(b["firewalls"]); string(got) != `[{"firewall":9}]` {
		t.Fatalf("firewalls %s", got)
	}
	if got, _ := json.Marshal(b["ssh_keys"]); string(got) != `[7]` {
		t.Fatalf("ssh_keys %s", got)
	}
	if got, _ := json.Marshal(b["public_net"]); string(got) != `{"enable_ipv4":true,"enable_ipv6":false}` {
		t.Fatalf("public_net %s", got)
	}
}

func TestCreateReportsAServerWhoseFirewallFailed(t *testing.T) {
	h, _ := fake(t, map[string]func(http.ResponseWriter, call){
		"POST /v1/servers":  write(201, `{"server": `+serverJSON+`, "action": {"id": 1, "status": "success"}, "next_actions": [{"id": 2, "status": "running"}]}`),
		"GET /v1/actions/2": write(200, `{"action": {"id": 2, "status": "error", "error": {"code": "x", "message": "firewall not applied"}}}`),
	})
	s, err := h.Create(context.Background(), Spec{})
	if err == nil || s.ID != "42" || Refused(err) {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestCreateGivesUpOnAnActionThatNeverEnds(t *testing.T) {
	old := ActionWait
	ActionWait = 50 * time.Millisecond
	defer func() { ActionWait = old }()
	h, _ := fake(t, map[string]func(http.ResponseWriter, call){
		"POST /v1/servers":  write(201, `{"server": `+serverJSON+`, "action": {"id": 1, "status": "success"}, "next_actions": [{"id": 2, "status": "running"}]}`),
		"GET /v1/actions/2": write(200, `{"action": {"id": 2, "status": "running"}}`),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := h.Create(ctx, Spec{})
	if err == nil || s.ID != "42" || ctx.Err() != nil {
		t.Fatalf("got %+v, %v (outer ctx %v)", s, err, ctx.Err())
	}
}

func TestAPIErrorsKeepTheirCode(t *testing.T) {
	h, _ := fake(t, map[string]func(http.ResponseWriter, call){
		"POST /v1/servers": write(403, `{"error": {"code": "resource_limit_exceeded", "message": "server limit exceeded", "details": {}}}`),
	})
	_, err := h.Create(context.Background(), Spec{})
	var api *APIError
	if !errors.As(err, &api) || api.Code != "resource_limit_exceeded" || api.Status != 403 {
		t.Fatalf("got %v", err)
	}
}

func TestNotFound(t *testing.T) {
	notFound := write(404, `{"error": {"code": "not_found", "message": "server with ID '42' not found"}}`)
	h, _ := fake(t, map[string]func(http.ResponseWriter, call){"GET /v1/servers/42": notFound, "DELETE /v1/servers/42": notFound})
	if _, err := h.Get(context.Background(), "42"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: %v", err)
	}
	if err := h.Delete(context.Background(), "42"); err != nil {
		t.Fatalf("deleting a server already gone: %v", err)
	}
}

func TestListFollowsPages(t *testing.T) {
	page := 0
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/servers": func(w http.ResponseWriter, c call) {
			page++
			next := "2"
			if page == 2 {
				next = "null"
			}
			io.WriteString(w, `{"servers": [`+strings.Replace(serverJSON, "42", []string{"", "1", "2"}[page], 1)+`],
				"meta": {"pagination": {"page": `+[]string{"", "1", "2"}[page]+`, "per_page": 50, "previous_page": null, "next_page": `+next+`, "last_page": 2, "total_entries": 2}}}`)
		},
	})
	got, err := h.List(context.Background(), "m2test=worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("got %+v", got)
	}
	if q := (*calls)[1].query; !strings.Contains(q, "label_selector=m2test%3Dworker") || !strings.Contains(q, "page=2") {
		t.Fatalf("query %s", q)
	}
}

const serverTypes = `{"server_types": [{"id": 96, "name": "ccx33", "cores": 8, "memory": 32, "architecture": "x86",
	"prices": [{"location": "fsn1", "price_hourly": {"net": "0.0800", "gross": "0.0952"}},
	           {"location": "hel1", "price_hourly": {"net": "0.0800", "gross": "0.0952000000000000"}}]}],
	"meta": {"pagination": {"page": 1, "next_page": null}}}`

const pricing = `{"pricing": {"currency": "EUR", "vat_rate": "19.00", "primary_ips": [
	{"type": "ipv4", "prices": [{"location": "hel1", "price_hourly": {"net": "0.0008", "gross": "0.0010"}}]},
	{"type": "ipv6", "prices": [{"location": "hel1", "price_hourly": {"net": "0", "gross": "0"}}]}]}}`

func TestServerType(t *testing.T) {
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){"GET /v1/server_types": write(200, serverTypes), "GET /v1/pricing": write(200, pricing)})
	price, arch, err := h.ServerType(context.Background(), "ccx33", "hel1")
	if err != nil || math.Abs(price-0.0962) > 1e-9 || arch != "amd64" {
		t.Fatalf("got %v %s %v", price, arch, err)
	}
	if (*calls)[0].query != "name=ccx33" {
		t.Fatalf("query %s", (*calls)[0].query)
	}
	if _, _, err := h.ServerType(context.Background(), "ccx33", "ash"); err == nil {
		t.Fatal("a location without a price was accepted")
	}
	h, _ = fake(t, map[string]func(http.ResponseWriter, call){"GET /v1/server_types": write(200, serverTypes),
		"GET /v1/pricing": write(200, strings.Replace(pricing, "EUR", "USD", 1))})
	if _, _, err := h.ServerType(context.Background(), "ccx33", "hel1"); err == nil {
		t.Fatal("dollar prices were taken as euros")
	}
}

func TestServerTypeArm(t *testing.T) {
	h, _ := fake(t, map[string]func(http.ResponseWriter, call){"GET /v1/server_types": write(200, strings.Replace(serverTypes, `"x86"`, `"arm"`, 1)), "GET /v1/pricing": write(200, pricing)})
	if _, arch, err := h.ServerType(context.Background(), "ccx33", "hel1"); err != nil || arch != "arm64" {
		t.Fatalf("got %s %v", arch, err)
	}
}

func TestSSHKeyReplacesAStaleKey(t *testing.T) {
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/ssh_keys":      write(200, `{"ssh_keys": [{"id": 3, "name": "m2test-pool", "public_key": "ssh-ed25519 OLD", "labels": {"m2test": "pool"}}]}`),
		"DELETE /v1/ssh_keys/3": write(204, ""),
		"POST /v1/ssh_keys":     write(201, `{"ssh_key": {"id": 4, "name": "m2test-pool", "public_key": "ssh-ed25519 NEW"}}`),
	})
	id, err := h.SSHKey(context.Background(), "m2test-pool", "ssh-ed25519 NEW")
	if err != nil || id != "4" {
		t.Fatalf("got %s %v", id, err)
	}
	if len(*calls) != 3 || (*calls)[2].body["public_key"] != "ssh-ed25519 NEW" {
		t.Fatalf("calls %+v", *calls)
	}
}

func TestSSHKeyReusesAMatchingKey(t *testing.T) {
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/ssh_keys": write(200, `{"ssh_keys": [{"id": 3, "name": "m2test-pool", "public_key": "ssh-ed25519 SAME"}]}`),
	})
	if id, err := h.SSHKey(context.Background(), "m2test-pool", "ssh-ed25519 SAME"); err != nil || id != "3" || len(*calls) != 1 {
		t.Fatalf("got %s %v after %d calls", id, err, len(*calls))
	}
}

func TestSSHKeyIgnoresTheComment(t *testing.T) {
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/ssh_keys": write(200, `{"ssh_keys": [{"id": 3, "name": "m2test-pool", "public_key": "ssh-ed25519 SAME"}]}`),
	})
	if id, err := h.SSHKey(context.Background(), "m2test-pool", "ssh-ed25519 SAME m2test-pool"); err != nil || id != "3" || len(*calls) != 1 {
		t.Fatalf("got %s %v after %d calls", id, err, len(*calls))
	}
}

func TestFirewallWaitsForItsRules(t *testing.T) {
	polls := 0
	h, _ := fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/firewalls":                      write(200, `{"firewalls": [{"id": 9, "name": "m2test", "labels": {"m2test": "firewall"}}]}`),
		"POST /v1/firewalls/9/actions/set_rules": write(201, `{"actions": [{"id": 5, "status": "running"}]}`),
		"GET /v1/actions/5": func(w http.ResponseWriter, c call) {
			if polls++; polls < 3 {
				write(200, `{"action": {"id": 5, "status": "running"}}`)(w, c)
			} else {
				write(200, `{"action": {"id": 5, "status": "success"}}`)(w, c)
			}
		},
	})
	if id, err := h.Firewall(context.Background(), "m2test", []string{"198.51.100.4/32"}); err != nil || id != "9" || polls != 3 {
		t.Fatalf("got %s %v after %d polls", id, err, polls)
	}

	h, _ = fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/firewalls":                      write(200, `{"firewalls": [{"id": 9, "name": "m2test", "labels": {"m2test": "firewall"}}]}`),
		"POST /v1/firewalls/9/actions/set_rules": write(201, `{"actions": [{"id": 5, "status": "running"}]}`),
		"GET /v1/actions/5":                      write(200, `{"action": {"id": 5, "status": "error", "error": {"code": "x", "message": "rules not applied"}}}`),
	})
	if _, err := h.Firewall(context.Background(), "m2test", []string{"198.51.100.4/32"}); err == nil || !strings.Contains(err.Error(), "rules not applied") {
		t.Fatalf("a failed action: %v", err)
	}
}

func TestFirewallLeavesSomeoneElsesAlone(t *testing.T) {
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/firewalls": write(200, `{"firewalls": [{"id": 9, "name": "m2test", "labels": {}}]}`),
	})
	if _, err := h.Firewall(context.Background(), "m2test", []string{"198.51.100.4/32"}); err == nil || len(*calls) != 1 {
		t.Fatalf("changed: %v, %d calls", err, len(*calls))
	}
}

func TestFirewall(t *testing.T) {
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/firewalls":                      write(200, `{"firewalls": [{"id": 9, "name": "m2test", "labels": {"m2test": "firewall"}}]}`),
		"POST /v1/firewalls/9/actions/set_rules": write(201, `{"actions": []}`),
	})
	id, err := h.Firewall(context.Background(), "m2test", []string{"198.51.100.4/32"})
	if err != nil || id != "9" {
		t.Fatalf("got %s %v", id, err)
	}
	rules, _ := json.Marshal((*calls)[1].body["rules"])
	if string(rules) != `[{"direction":"in","port":"22","protocol":"tcp","source_ips":["198.51.100.4/32"]}]` {
		t.Fatalf("rules %s", rules)
	}

	h, calls = fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/firewalls":  write(200, `{"firewalls": []}`),
		"POST /v1/firewalls": write(201, `{"firewall": {"id": 10, "name": "m2test"}, "actions": []}`),
	})
	if id, err := h.Firewall(context.Background(), "m2test", []string{"198.51.100.4/32"}); err != nil || id != "10" {
		t.Fatalf("made %s %v", id, err)
	}
	if (*calls)[1].body["name"] != "m2test" || (*calls)[1].body["rules"] == nil {
		t.Fatalf("create body %v", (*calls)[1].body)
	}
}

func hostKeys(t *testing.T) Host {
	return Host{
		PoolKey:        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPool m2test-pool",
		HostPublicKey:  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHost hz-1",
		HostPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nAAAABG5vbmU=\n-----END OPENSSH PRIVATE KEY-----\n",
		Dir:            "/var/lib/m2test",
	}
}

func TestUserDataIsValidCloudConfig(t *testing.T) {
	out, err := UserData(hostKeys(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "#cloud-config\n") {
		t.Fatal("no #cloud-config header")
	}
	var doc struct {
		SSHKeys map[string]string `yaml:"ssh_keys"`
		GenKeys []string          `yaml:"ssh_genkeytypes"`
		Users   []struct {
			Name string
			Keys []string `yaml:"ssh_authorized_keys"`
		}
		RunCmd [][]string `yaml:"runcmd"`
		Files  []struct {
			Path    string
			Content string
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc.SSHKeys["ed25519_private"] != hostKeys(t).HostPrivateKey || doc.SSHKeys["ed25519_public"] != hostKeys(t).HostPublicKey {
		t.Fatalf("host keys %q", doc.SSHKeys)
	}
	// cloud-init's schema wants at least one; ed25519 is given, so none is made.
	if len(doc.GenKeys) != 1 || doc.GenKeys[0] != "ed25519" {
		t.Fatalf("ssh_genkeytypes %v", doc.GenKeys)
	}
	if len(doc.Users) != 1 || doc.Users[0].Name != "m2test" || len(doc.Users[0].Keys) != 1 ||
		doc.Users[0].Keys[0] != "restrict,port-forwarding "+hostKeys(t).PoolKey {
		t.Fatalf("users %+v", doc.Users)
	}
	script := doc.RunCmd[0][2]
	if !strings.HasSuffix(strings.TrimSpace(script), "touch /var/lib/m2test/"+Provisioned) || doc.RunCmd[0][1] != "-ec" {
		t.Fatalf("the marker must be the last step of a set -e script:\n%s", script)
	}
	blocked := false
	for _, f := range doc.Files {
		blocked = blocked || f.Path == "/etc/systemd/system/docker.service.d/m2test.conf" &&
			strings.HasPrefix(f.Content, "[Service]\nExecStartPost=") && strings.Contains(f.Content, "iptables -I DOCKER-USER -d 169.254.169.254 -j DROP")
	}
	if !blocked {
		t.Fatal("metadata service not blocked on each Docker start")
	}
	if !strings.Contains(script, "iptables -C DOCKER-USER -d 169.254.169.254 -j DROP") {
		t.Fatal("provisioning doesn't check the metadata block is in force")
	}
}

func TestUserDataRefusesInjection(t *testing.T) {
	for name, edit := range map[string]func(*Host){
		"pool key newline": func(h *Host) { h.PoolKey += "\nruncmd: [reboot]" },
		"host key":         func(h *Host) { h.HostPublicKey = "ssh-rsa AAAA" },
		"private key": func(h *Host) {
			h.HostPrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\n'; reboot\n-----END OPENSSH PRIVATE KEY-----\n"
		},
		"dir": func(h *Host) { h.Dir = "/var/lib/m2test; reboot" },
	} {
		h := hostKeys(t)
		edit(&h)
		if _, err := UserData(h); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestKeyIDAsksByName(t *testing.T) {
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/ssh_keys": func(w http.ResponseWriter, c call) {
			if c.query == "name=m2test-pool-a1" {
				w.Write([]byte(`{"ssh_keys": [{"id": 3}]}`))
				return
			}
			w.Write([]byte(`{"ssh_keys": []}`))
		},
	})
	if id, err := h.KeyID(context.Background(), "m2test-pool-a1"); id != "3" || err != nil {
		t.Errorf("own key: %q %v", id, err)
	}
	if id, err := h.KeyID(context.Background(), "m2test-pool-b2"); id != "" || err != nil || len(*calls) != 2 {
		t.Errorf("no key: %q %v", id, err)
	}
}

func TestParsePriceRefusesWhatSkipsTheCap(t *testing.T) {
	for _, bad := range []string{"NaN", "Inf", "-0.01", "0", "x"} {
		if _, err := parsePrice(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if f, err := parsePrice("0.0952"); err != nil || f != 0.0952 {
		t.Errorf("control: %v %v", f, err)
	}
}

func TestSSHKeyLeavesAnUnlabelledKey(t *testing.T) {
	h, calls := fake(t, map[string]func(http.ResponseWriter, call){
		"GET /v1/ssh_keys": write(200, `{"ssh_keys": [{"id": 3, "name": "m2test-pool", "public_key": "ssh-ed25519 OLD"}]}`),
	})
	if _, err := h.SSHKey(context.Background(), "m2test-pool", "ssh-ed25519 NEW"); err == nil || len(*calls) != 1 {
		t.Fatalf("%v after %d calls", err, len(*calls))
	}
}

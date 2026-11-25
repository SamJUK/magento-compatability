// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package cloud

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"
)

const Provisioned = "provisioned"

type Host struct {
	PoolKey        string // the only key m2test accepts
	HostPrivateKey string // pinned by the coordinator before the server exists
	HostPublicKey  string
	Dir            string
	// Deadline, if set, powers the server off then and at any boot after
	// it; for an EC2 instance that is termination.
	Deadline time.Time
}

var (
	validPublic  = regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/]+=*( [A-Za-z0-9@._-]+)?$`)
	validPrivate = regexp.MustCompile(`^-----BEGIN OPENSSH PRIVATE KEY-----\n[A-Za-z0-9+/=\n]+-----END OPENSSH PRIVATE KEY-----\n?$`)
	validDir     = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

// UserData renders the cloud-config. Everything in it is checked first,
// since it lands in YAML and a root shell script.
func UserData(h Host) (string, error) {
	switch {
	case !validPublic.MatchString(h.PoolKey):
		return "", fmt.Errorf("bad pool key")
	case !validPublic.MatchString(h.HostPublicKey):
		return "", fmt.Errorf("bad host key")
	case !validPrivate.MatchString(h.HostPrivateKey):
		return "", fmt.Errorf("bad host private key")
	case !validDir.MatchString(h.Dir):
		return "", fmt.Errorf("bad dir %q", h.Dir)
	}
	var b strings.Builder
	err := userData.Execute(&b, map[string]any{
		"PoolKey": h.PoolKey, "HostPublic": h.HostPublicKey, "Dir": h.Dir, "Provisioned": Provisioned,
		"Deadline": h.Deadline.Unix(), "DeadlineAt": h.Deadline.UTC().Format("2006-01-02 15:04:05 UTC"), "HasDeadline": !h.Deadline.IsZero(),
		"HostPrivate": strings.TrimSpace(strings.ReplaceAll(h.HostPrivateKey, "\n", "\n      ")),
	})
	return b.String(), err
}

var userData = template.Must(template.New("").Parse(`#cloud-config
{{- if .HasDeadline}}
# Every boot, earliest. Only systemctl reaches systemd this early: D-Bus is
# not up, so systemd-run here hangs the boot.
bootcmd:
  - - sh
    - -c
    - |
      [ "$(date +%s)" -lt {{.Deadline}} ] || { logger -s -t m2test "past the deadline, powering off"; exec systemctl --no-block poweroff; }
      printf '[Timer]\nOnCalendar={{.DeadlineAt}}\nAccuracySec=1s\nPersistent=true\n[Install]\nWantedBy=timers.target\n' > /etc/systemd/system/m2test-deadline.timer
      printf '[Service]\nType=oneshot\nExecStart=/usr/bin/systemctl --no-block poweroff\n' > /etc/systemd/system/m2test-deadline.service
      systemctl daemon-reload && systemctl enable m2test-deadline.timer && systemctl start --no-block m2test-deadline.timer ||
        { logger -s -t m2test "could not arm the deadline, powering off"; exec systemctl --no-block poweroff; }
{{- end}}
ssh_deletekeys: true
ssh_genkeytypes: [ed25519]
ssh_keys:
  ed25519_private: |
      {{.HostPrivate}}
  ed25519_public: {{.HostPublic}}
disable_root: true
ssh_pwauth: false
users:
  - name: m2test
    shell: /bin/sh
    lock_passwd: true
    ssh_authorized_keys:
      - restrict,port-forwarding {{.PoolKey}}
package_update: true
packages: [ca-certificates, curl, dbus-user-session, libpam-systemd]
write_files:
  - path: /etc/ssh/sshd_config.d/00-m2test.conf
    content: |
      PermitRootLogin no
      PasswordAuthentication no
      Match User m2test
        AllowTcpForwarding remote
        AllowStreamLocalForwarding remote
        PermitListen none
        PermitOpen none
        AllowAgentForwarding no
        X11Forwarding no
        PermitTTY no
        StreamLocalBindUnlink yes
        ClientAliveInterval 15
        ClientAliveCountMax 3
      Match all
  # Containers must not read the metadata service: it serves this user data,
  # host key included. Set on every Docker start, so it outlives a reboot.
  - path: /etc/systemd/system/docker.service.d/m2test.conf
    content: |
      [Service]
      ExecStartPost=/bin/sh -c 'iptables -C DOCKER-USER -d 169.254.169.254 -j DROP || iptables -I DOCKER-USER -d 169.254.169.254 -j DROP'
  - path: /etc/docker/daemon.json
    content: '{"log-driver": "local", "log-opts": {"max-size": "20m", "max-file": "3"}}'
  - path: /etc/sysctl.d/90-m2test.conf
    content: |
      vm.max_map_count = 262144
      net.ipv6.conf.all.disable_ipv6 = 1
      net.ipv6.conf.default.disable_ipv6 = 1
runcmd:
  - - sh
    - -ec
    - |
      sshd -t
      systemctl try-reload-or-restart ssh
      sysctl --system
      fallocate -l 4G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
      install -d -m 0755 /etc/apt/keyrings
      curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
      printf 'Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: trixie\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' > /etc/apt/sources.list.d/docker.sources
      apt-get update
      DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
      iptables -C DOCKER-USER -d 169.254.169.254 -j DROP
      usermod -aG docker m2test
      loginctl enable-linger m2test
      install -d -o m2test -m 0700 {{.Dir}}
      touch {{.Dir}}/{{.Provisioned}}
`))

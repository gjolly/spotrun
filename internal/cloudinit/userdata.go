package cloudinit

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"text/template"
)

type Params struct {
	SSHPublicKey  string
	Image         string
	OutputDir     string
	Env           map[string]string
	StorageType   string
	RegistryUser  string
	RegistryToken string
}

var scriptTemplate = template.Must(template.New("userdata").Funcs(template.FuncMap{
	"registryHost": func(image string) string {
		parts := strings.SplitN(image, "/", 2)
		if len(parts) > 1 && strings.Contains(parts[0], ".") {
			return parts[0]
		}
		return "docker.io"
	},
}).Parse(`#!/bin/bash
set -euo pipefail

mkdir -p /var/log/spotrun /spotrun-output

exec >> /var/log/spotrun/output.log 2>&1

# SSH key setup
mkdir -p /home/ubuntu/.ssh
chmod 700 /home/ubuntu/.ssh
echo '{{ .SSHPublicKey }}' >> /home/ubuntu/.ssh/authorized_keys
chmod 600 /home/ubuntu/.ssh/authorized_keys
chown -R ubuntu:ubuntu /home/ubuntu/.ssh

# Install docker
apt-get update -y
apt-get install -y docker.io
systemctl enable --now docker
{{ if .RegistryUser }}
# Registry auth
docker login {{ registryHost .Image }} -u '{{ .RegistryUser }}' -p '{{ .RegistryToken }}'
{{ end }}
{{ if eq .StorageType "nvme" }}
# Mount first available NVMe device
for dev in /dev/nvme1n1 /dev/nvme2n1 /dev/nvme3n1; do
  if [ -b "$dev" ]; then
    mkfs.ext4 -F "$dev"
    mount "$dev" /spotrun-output
    break
  fi
done
{{ end }}
# Pull and run workload
docker pull {{ .Image }}

set +e
docker run --rm \
  -v /spotrun-output:{{ .OutputDir }} \
{{ range $k, $v := .Env }}  -e {{ $k }}={{ $v }} \
{{ end }}  {{ .Image }}
echo $? > /var/log/spotrun/exitcode
`))

func Generate(p Params) (string, error) {
	var buf bytes.Buffer
	if err := scriptTemplate.Execute(&buf, p); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
// SPDX-License-Identifier: OSL-3.0

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/samjuk/magento-compatability/internal/result"
)

const serviceLabel = `{{index .Config.Labels "com.docker.compose.service"}}`

// captureImages records the platform and repo digest of the image each running
// service container uses. Inspection is best effort: anything that fails is
// left out of the map, and nil is returned when nothing could be read.
func captureImages(ctx context.Context, cp *Compose) map[string]result.Image {
	ids := strings.Fields(cp.output(ctx, "docker", cp.args("ps", "-a", "-q")...))
	if len(ids) == 0 {
		return nil
	}
	serviceImages := parseServiceImages(cp.output(ctx, "docker",
		append([]string{"inspect", "-f", serviceLabel + "\t{{.Image}}"}, ids...)...))
	if len(serviceImages) == 0 {
		return nil
	}

	var imageIDs []string
	for _, id := range serviceImages {
		imageIDs = append(imageIDs, id)
	}
	details := parseImageDetails(cp.output(ctx, "docker",
		append([]string{"image", "inspect", "-f", `{{.Id}}` + "\t" + `{{.Os}}/{{.Architecture}}` + "\t" + `{{json .RepoDigests}}`}, imageIDs...)...))
	hostPlatform := strings.TrimSpace(cp.output(ctx, "docker", "version", "-f", "{{.Server.Os}}/{{.Server.Arch}}"))

	images := map[string]result.Image{}
	for svc, id := range serviceImages {
		d, ok := details[id]
		if !ok {
			continue
		}
		img := result.Image{Platform: d.platform, Digest: d.digest}
		if d.platform != "" && strings.Contains(hostPlatform, "/") {
			emulated := d.platform != hostPlatform
			img.Emulated = &emulated
		}
		if img != (result.Image{}) {
			images[svc] = img
		}
	}
	if len(images) == 0 {
		return nil
	}
	return images
}

// output runs a command and returns its stdout, or "" when it fails.
func (cp *Compose) output(ctx context.Context, name string, args ...string) string {
	cmd := cp.command(ctx, name, args...)
	cmd.Env = cp.env
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return out.String()
}

// parseServiceImages reads "service<TAB>image id" lines.
func parseServiceImages(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		svc, id, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && svc != "" && id != "" {
			m[svc] = id
		}
	}
	return m
}

type imageDetail struct{ platform, digest string }

// parseImageDetails reads "image id<TAB>os/arch<TAB>repo digests as JSON" lines.
func parseImageDetails(out string) map[string]imageDetail {
	m := map[string]imageDetail{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		d := imageDetail{}
		if p := f[1]; p != "/" && strings.Count(p, "/") == 1 && !strings.HasPrefix(p, "/") && !strings.HasSuffix(p, "/") {
			d.platform = p
		}
		var digests []string
		if json.Unmarshal([]byte(f[2]), &digests) == nil && len(digests) > 0 {
			d.digest = digests[0]
		}
		m[f[0]] = d
	}
	return m
}

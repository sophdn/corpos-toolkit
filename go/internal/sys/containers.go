package sys

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ContainerRow is one container in a sys.containers listing, tagged with the
// runtime it came from.
type ContainerRow struct {
	Runtime string `json:"runtime"` // podman | docker
	ID      string `json:"id"`
	Image   string `json:"image"`
	Names   string `json:"names"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Ports   string `json:"ports"`
}

// ContainersParams filters a sys.containers listing.
type ContainersParams struct {
	RunningOnly bool `json:"running_only,omitempty"` // omit -a when true
}

// ContainersResult is the success shape for sys.containers. RuntimesQueried
// names the runtimes that were actually probed (fail-soft: an absent runtime
// contributes no rows and is not listed).
type ContainersResult struct {
	Containers      []ContainerRow `json:"containers"`
	Count           int            `json:"count"`
	RuntimesQueried []string       `json:"runtimes_queried"`
}

// idShort truncates a container id to the conventional 12-char short form.
func idShort(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// --- podman: `podman ps --format json` is a JSON array ---

type podmanPort struct {
	HostIP        string `json:"host_ip"`
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	Protocol      string `json:"protocol"`
}

type podmanContainer struct {
	ID     string       `json:"Id"`
	Image  string       `json:"Image"`
	Names  []string     `json:"Names"`
	State  string       `json:"State"`
	Status string       `json:"Status"`
	Ports  []podmanPort `json:"Ports"`
}

func parsePodmanPS(b []byte) ([]ContainerRow, error) {
	var raw []podmanContainer
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse podman json: %w", err)
	}
	rows := make([]ContainerRow, 0, len(raw))
	for _, c := range raw {
		rows = append(rows, ContainerRow{
			Runtime: "podman",
			ID:      idShort(c.ID),
			Image:   c.Image,
			Names:   strings.Join(c.Names, ","),
			State:   c.State,
			Status:  c.Status,
			Ports:   renderPodmanPorts(c.Ports),
		})
	}
	return rows, nil
}

func renderPodmanPorts(ports []podmanPort) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		host := ""
		if p.HostIP != "" {
			host = p.HostIP + ":"
		}
		parts = append(parts, fmt.Sprintf("%s%d->%d/%s", host, p.HostPort, p.ContainerPort, p.Protocol))
	}
	return strings.Join(parts, ", ")
}

// --- docker: `docker ps --format '{{json .}}'` is newline-delimited objects ---

type dockerContainer struct {
	ID     string `json:"ID"`
	Image  string `json:"Image"`
	Names  string `json:"Names"`
	State  string `json:"State"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
}

func parseDockerPS(b []byte) ([]ContainerRow, error) {
	rows := []ContainerRow{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c dockerContainer
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("parse docker json line: %w", err)
		}
		rows = append(rows, ContainerRow{
			Runtime: "docker",
			ID:      idShort(c.ID),
			Image:   c.Image,
			Names:   c.Names,
			State:   c.State,
			Status:  c.Status,
			Ports:   c.Ports,
		})
	}
	return rows, nil
}

// HandleContainers lists containers from podman and docker, each probed and
// fail-soft (see testdata/INTROSPECTION_CONTRACT.md). Read-only.
func HandleContainers(ctx context.Context, params json.RawMessage) (ContainersResult, error) {
	var p ContainersParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ContainersResult{}, fmt.Errorf("sys.containers: invalid params: %w", err)
		}
	}
	res := ContainersResult{Containers: []ContainerRow{}, RuntimesQueried: []string{}}

	if _, err := exec.LookPath("podman"); err == nil {
		args := []string{"ps", "--format", "json"}
		if !p.RunningOnly {
			args = append(args, "-a")
		}
		if out, err := runHostCmd(ctx, "podman", args...); err == nil {
			res.RuntimesQueried = append(res.RuntimesQueried, "podman")
			if rows, perr := parsePodmanPS([]byte(out)); perr == nil {
				res.Containers = append(res.Containers, rows...)
			}
		}
	}
	if _, err := exec.LookPath("docker"); err == nil {
		args := []string{"ps", "--format", "{{json .}}"}
		if !p.RunningOnly {
			args = append(args, "-a")
		}
		if out, err := runHostCmd(ctx, "docker", args...); err == nil {
			res.RuntimesQueried = append(res.RuntimesQueried, "docker")
			if rows, perr := parseDockerPS([]byte(out)); perr == nil {
				res.Containers = append(res.Containers, rows...)
			}
		}
	}
	res.Count = len(res.Containers)
	return res, nil
}

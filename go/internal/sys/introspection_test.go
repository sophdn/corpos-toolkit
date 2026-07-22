package sys

// introspection_test.go is the net for the self-defined read-only introspection
// actions. The pure parsers are pinned against captured real tool output (ps via
// /proc/<pid>/stat, ss -tulnpH, systemctl -o json, podman/docker ps); the live
// handlers get fail-soft smokes. See testdata/INTROSPECTION_CONTRACT.md.

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func TestParseProcStat_Simple(t *testing.T) {
	// Real /proc/<pid>/stat for `cat`: pid 60216, comm cat, state R, ppid 60174,
	// rss 435 pages (field 24).
	line := "60216 (cat) R 60174 60216 60174 0 -1 4194304 127 0 0 0 0 0 0 0 20 0 1 0 237660 17518592 435 18446744073709551615 1 1 1 0 0 0 0 0 0 0 0 0 17 15 0 0"
	ps, ok := parseProcStat(line)
	if !ok {
		t.Fatal("parseProcStat returned ok=false")
	}
	if ps.PID != 60216 || ps.PPID != 60174 || ps.State != "R" || ps.Comm != "cat" || ps.RSSPages != 435 {
		t.Errorf("parseProcStat = %+v, want pid 60216 ppid 60174 state R comm cat rss 435", ps)
	}
}

func TestParseProcStat_CommWithParensAndSpaces(t *testing.T) {
	// comm must be taken between the first '(' and the LAST ')'.
	line := "1000 (weird )( name) S 50 1 1 0 -1 0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16"
	ps, ok := parseProcStat(line)
	if !ok {
		t.Fatal("ok=false")
	}
	if ps.Comm != "weird )( name" {
		t.Errorf("comm = %q, want %q", ps.Comm, "weird )( name")
	}
	if ps.PID != 1000 || ps.PPID != 50 || ps.State != "S" || ps.RSSPages != 15 {
		t.Errorf("ps = %+v, want pid 1000 ppid 50 state S rss 15", ps)
	}
}

func TestParseProcStat_Malformed(t *testing.T) {
	for _, bad := range []string{"", "no parens here", "123 (unterminated"} {
		if _, ok := parseProcStat(bad); ok {
			t.Errorf("parseProcStat(%q) = ok, want not ok", bad)
		}
	}
}

func TestParseCmdline(t *testing.T) {
	if got := parseCmdline([]byte("rg\x00--files\x00-g\x00*.go\x00")); got != "rg --files -g *.go" {
		t.Errorf("parseCmdline = %q", got)
	}
	if got := parseCmdline([]byte("")); got != "" {
		t.Errorf("empty cmdline = %q, want empty", got)
	}
	if got := parseCmdline([]byte("solo")); got != "solo" {
		t.Errorf("solo = %q", got)
	}
}

func TestParseSSLine(t *testing.T) {
	cases := []struct {
		line                string
		wantProto, wantAddr string
		wantPort, wantPID   int
		wantProc            string
		wantOK              bool
	}{
		{`tcp LISTEN 0 512 127.0.0.1:8081 0.0.0.0:* users:(("llama-server",pid=2112,fd=16))`, "tcp", "127.0.0.1", 8081, 2112, "llama-server", true},
		{`tcp LISTEN 0 4096 *:3000 *:* users:(("toolkit-server",pid=5074,fd=6))`, "tcp", "*", 3000, 5074, "toolkit-server", true},
		{`udp UNCONN 0 0 127.0.0.53%lo:53 0.0.0.0:*`, "udp", "127.0.0.53%lo", 53, 0, "", true},
		{`tcp LISTEN 0 128 [::]:8080 [::]:* users:(("svc",pid=99,fd=3))`, "tcp", "[::]", 8080, 99, "svc", true},
		{`garbage`, "", "", 0, 0, "", false},
	}
	for _, c := range cases {
		row, ok := parseSSLine(c.line)
		if ok != c.wantOK {
			t.Errorf("parseSSLine(%q) ok=%v, want %v", c.line, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if row.Proto != c.wantProto || row.LocalAddr != c.wantAddr || row.LocalPort != c.wantPort || row.PID != c.wantPID || row.Process != c.wantProc {
			t.Errorf("parseSSLine(%q) = %+v, want proto %s addr %s port %d pid %d proc %s", c.line, row, c.wantProto, c.wantAddr, c.wantPort, c.wantPID, c.wantProc)
		}
	}
}

func TestParseSystemctlUnits(t *testing.T) {
	js := []byte(`[{"unit":"at-spi-dbus-bus.service","load":"loaded","active":"active","sub":"running","description":"Accessibility services bus"},{"unit":"corpos-network.service","load":"loaded","active":"active","sub":"exited","description":"corpos-net"}]`)
	rows, err := parseSystemctlUnits(js)
	if err != nil {
		t.Fatalf("parseSystemctlUnits: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Unit != "at-spi-dbus-bus.service" || rows[0].Active != "active" || rows[0].Sub != "running" {
		t.Errorf("row0 = %+v", rows[0])
	}
	if rows[1].Sub != "exited" {
		t.Errorf("row1 sub = %q, want exited", rows[1].Sub)
	}
}

func TestParsePodmanPS(t *testing.T) {
	js := []byte(`[{"Id":"706c2e4b9366abcdef","Image":"img:tag","Names":["toolkit-server"],"State":"running","Status":"Up 44 minutes","Ports":[{"host_ip":"","container_port":3000,"host_port":3001,"protocol":"tcp"}]}]`)
	rows, err := parsePodmanPS(js)
	if err != nil {
		t.Fatalf("parsePodmanPS: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Runtime != "podman" || r.ID != "706c2e4b9366" || r.Image != "img:tag" || r.Names != "toolkit-server" || r.State != "running" {
		t.Errorf("row = %+v", r)
	}
	if r.Ports != "3001->3000/tcp" {
		t.Errorf("ports = %q, want 3001->3000/tcp", r.Ports)
	}
}

func TestParseDockerPS(t *testing.T) {
	// docker emits newline-delimited JSON objects.
	nd := []byte(`{"ID":"aba71fa11583","Image":"img","Names":"kiwix","State":"running","Status":"Up 39 minutes","Ports":"127.0.0.1:8888->8080/tcp"}
{"ID":"2e68843e7271","Image":"img2","Names":"kiwix2","State":"running","Status":"Up 39 minutes","Ports":""}`)
	rows, err := parseDockerPS(nd)
	if err != nil {
		t.Fatalf("parseDockerPS: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Runtime != "docker" || rows[0].ID != "aba71fa11583" || rows[0].Names != "kiwix" || rows[0].Ports != "127.0.0.1:8888->8080/tcp" {
		t.Errorf("row0 = %+v", rows[0])
	}
}

// ---- live fail-soft smokes ----

func TestHandlePS_Live(t *testing.T) {
	res, err := HandlePS(context.Background(), mustRaw(t, `{}`))
	if err != nil {
		t.Fatalf("HandlePS: %v", err)
	}
	// Our own pid must appear.
	self := os.Getpid()
	found := false
	for _, p := range res.Processes {
		if p.PID == self {
			found = true
		}
	}
	if !found {
		t.Errorf("self pid %d not in %d processes", self, len(res.Processes))
	}
}

func TestHandlePS_ContainsFilter(t *testing.T) {
	res, err := HandlePS(context.Background(), mustRaw(t, `{"contains":"this-matches-no-process-xyzzy"}`))
	if err != nil {
		t.Fatalf("HandlePS: %v", err)
	}
	if len(res.Processes) != 0 {
		t.Errorf("filter matched %d, want 0", len(res.Processes))
	}
}

func TestHandlePorts_Live(t *testing.T) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("ss not installed")
	}
	if _, err := HandlePorts(context.Background(), mustRaw(t, `{}`)); err != nil {
		t.Fatalf("HandlePorts: %v", err)
	}
}

func TestHandleUnits_Live(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl not installed")
	}
	if _, err := HandleUnits(context.Background(), mustRaw(t, `{}`)); err != nil {
		t.Fatalf("HandleUnits: %v", err)
	}
}

func TestHandleContainers_Live(t *testing.T) {
	// Fail-soft: returns without error even if a runtime is absent.
	res, err := HandleContainers(context.Background(), mustRaw(t, `{}`))
	if err != nil {
		t.Fatalf("HandleContainers: %v", err)
	}
	if len(res.RuntimesQueried) == 0 && (hasBin("podman") || hasBin("docker")) {
		t.Error("a runtime is installed but none reported queried")
	}
}

func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func mustRaw(t *testing.T, s string) []byte {
	t.Helper()
	return []byte(s)
}

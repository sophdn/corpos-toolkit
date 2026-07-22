package sys

import (
	"context"
	"testing"
)

func TestParseProcStat_TooFewFields(t *testing.T) {
	if _, ok := parseProcStat("123 (proc) R 1 2 3"); ok {
		t.Error("expected ok=false for too-few-fields stat")
	}
}

func TestParseProcStat_NonNumericPID(t *testing.T) {
	if _, ok := parseProcStat("abc (proc) R 1"); ok {
		t.Error("expected ok=false for non-numeric pid")
	}
}

func TestParseSystemctlUnits_EmptyAndBad(t *testing.T) {
	rows, err := parseSystemctlUnits([]byte("   "))
	if err != nil || len(rows) != 0 {
		t.Errorf("empty input: rows=%v err=%v, want [] nil", rows, err)
	}
	if _, err := parseSystemctlUnits([]byte("not json")); err == nil {
		t.Error("bad json = nil error, want error")
	}
}

func TestParsePodmanPS_EmptyAndBad(t *testing.T) {
	rows, err := parsePodmanPS([]byte("[]"))
	if err != nil || len(rows) != 0 {
		t.Errorf("empty array: rows=%v err=%v", rows, err)
	}
	if _, err := parsePodmanPS([]byte("{bad")); err == nil {
		t.Error("bad json = nil error, want error")
	}
}

func TestRenderPodmanPorts(t *testing.T) {
	got := renderPodmanPorts([]podmanPort{
		{HostIP: "1.2.3.4", ContainerPort: 80, HostPort: 8080, Protocol: "tcp"},
		{HostIP: "", ContainerPort: 53, HostPort: 5353, Protocol: "udp"},
	})
	want := "1.2.3.4:8080->80/tcp, 5353->53/udp"
	if got != want {
		t.Errorf("renderPodmanPorts = %q, want %q", got, want)
	}
	if renderPodmanPorts(nil) != "" {
		t.Errorf("nil ports = %q, want empty", renderPodmanPorts(nil))
	}
}

func TestParseDockerPS_BadLine(t *testing.T) {
	if _, err := parseDockerPS([]byte("{not valid json}\n")); err == nil {
		t.Error("bad docker json = nil error, want error")
	}
	rows, err := parseDockerPS([]byte("\n\n"))
	if err != nil || len(rows) != 0 {
		t.Errorf("blank lines: rows=%v err=%v", rows, err)
	}
}

func TestIntrospection_InvalidParams(t *testing.T) {
	bad := []byte(`{"limit":"not-an-int"}`)
	if _, err := HandlePS(context.Background(), bad); err == nil {
		t.Error("HandlePS bad params = nil error, want error")
	}
	if _, err := HandlePorts(context.Background(), []byte(`{"proto":123}`)); err == nil {
		t.Error("HandlePorts bad params = nil error, want error")
	}
	if _, err := HandleUnits(context.Background(), []byte(`{"type":123}`)); err == nil {
		t.Error("HandleUnits bad params = nil error, want error")
	}
	if _, err := HandleContainers(context.Background(), []byte(`{"running_only":"x"}`)); err == nil {
		t.Error("HandleContainers bad params = nil error, want error")
	}
}

func TestIDShort(t *testing.T) {
	if idShort("0123456789abcdef") != "0123456789ab" {
		t.Errorf("idShort long = %q", idShort("0123456789abcdef"))
	}
	if idShort("short") != "short" {
		t.Errorf("idShort short = %q", idShort("short"))
	}
}

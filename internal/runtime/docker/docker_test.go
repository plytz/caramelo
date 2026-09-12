package docker

import (
	"os"
	"strings"
	"testing"
)

func TestRenderDaemonJSON(t *testing.T) {
	got, err := RenderDaemonJSON(DaemonConfig{DataRoot: "/mnt/caramelo/docker"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "{\n  \"data-root\": \"/mnt/caramelo/docker\",\n  \"userland-proxy\": false\n}\n"
	if string(got) != want {
		t.Errorf("daemon.json =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderDaemonJSONIsStable(t *testing.T) {
	cfg := DaemonConfig{DataRoot: "/mnt/caramelo/docker", UserlandProxy: true}
	first, err := RenderDaemonJSON(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := RenderDaemonJSON(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("rendering is not stable:\n%q\n%q", first, again)
		}
	}
	if !strings.Contains(string(first), `"userland-proxy": true`) {
		t.Errorf("userland-proxy not rendered: %s", first)
	}
}

func TestPaths(t *testing.T) {
	if got := SocketPath("999"); got != "/run/user/999/docker.sock" {
		t.Errorf("SocketPath = %q", got)
	}
	if got := BusPath("999"); got != "/run/user/999/bus" {
		t.Errorf("BusPath = %q", got)
	}
	if got := DockerHost("999"); got != "unix:///run/user/999/docker.sock" {
		t.Errorf("DockerHost = %q", got)
	}
	if got := DaemonJSONPath("/var/lib/caramelo"); got != "/var/lib/caramelo/.config/docker/daemon.json" {
		t.Errorf("DaemonJSONPath = %q", got)
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return b
}

func TestParseInfoRootless(t *testing.T) {
	info, err := ParseInfo(readTestdata(t, "info-rootless.json"))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	if !info.Rootless() {
		t.Errorf("Rootless() = false for %v", info.SecurityOptions)
	}
	if !info.OverlayStorage() {
		t.Errorf("OverlayStorage() = false for driver %q", info.StorageDriver)
	}
	if info.StorageDriver != "overlayfs" {
		t.Errorf("StorageDriver = %q, want overlayfs (containerd snapshotter)", info.StorageDriver)
	}
	if info.DataRoot != "/mnt/caramelo/docker" {
		t.Errorf("DataRoot = %q", info.DataRoot)
	}
	if info.ServerVersion != "29.8.0" {
		t.Errorf("ServerVersion = %q", info.ServerVersion)
	}
	if !info.CgroupV2() || info.CgroupVersionInt() != 2 || info.CgroupDriver != "systemd" {
		t.Errorf("cgroup = %q/%q, want 2/systemd", info.CgroupVersion, info.CgroupDriver)
	}
	if len(info.Warnings) != 0 {
		t.Errorf("warnings = %v, want none once cgroup delegation is in place", info.Warnings)
	}
}

func TestParseInfoRootfulAndBroken(t *testing.T) {
	rootful := `{"ServerVersion":"29.8.0","Driver":"overlay2","DockerRootDir":"/var/lib/docker",
	  "CgroupDriver":"systemd","CgroupVersion":"2",
	  "SecurityOptions":["name=apparmor","name=seccomp,profile=builtin"],
	  "Warnings":["WARNING: bridge-nf-call-iptables is disabled"]}`
	info, err := ParseInfo([]byte(rootful))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	if info.Rootless() {
		t.Error("Rootless() = true without name=rootless")
	}
	if !info.OverlayStorage() {
		t.Error("overlay2 not recognised as overlay")
	}
	if len(info.Warnings) != 1 {
		t.Errorf("warnings = %v", info.Warnings)
	}
	if _, err := ParseInfo([]byte("Cannot connect to the Docker daemon")); err == nil {
		t.Error("accepted non-JSON output")
	}
	if _, err := ParseInfo([]byte(`{"Driver":"overlayfs"}`)); err == nil {
		t.Error("accepted info without ServerVersion")
	}
}

func TestParseVersionRootless(t *testing.T) {
	v := ParseVersion(string(readTestdata(t, "version-rootless.txt")))
	if v.NetworkDriver != "slirp4netns" {
		t.Errorf("NetworkDriver = %q, want slirp4netns", v.NetworkDriver)
	}
	if v.PortDriver != "builtin" {
		t.Errorf("PortDriver = %q, want builtin", v.PortDriver)
	}
	if v.Drivers() != "slirp4netns/builtin" {
		t.Errorf("Drivers() = %q", v.Drivers())
	}
	if v.RootlessKit != "3.1.0" {
		t.Errorf("RootlessKit = %q", v.RootlessKit)
	}
	if v.ServerVersion != "29.8.0" {
		t.Errorf("ServerVersion = %q", v.ServerVersion)
	}
	if v.ContainerdVer != "v2.3.5" || v.RuncVer != "1.5.1" || v.Slirp4netns != "1.2.1" {
		t.Errorf("component versions = %q %q %q", v.ContainerdVer, v.RuncVer, v.Slirp4netns)
	}
	if v.ClientVersion != "29.8.0" {
		t.Errorf("ClientVersion = %q", v.ClientVersion)
	}
}

func TestParseVersionRootfulHasNoRootlesskit(t *testing.T) {
	const rootful = `Client: Docker Engine - Community
 Version:           29.8.0
 Context:           default

Server: Docker Engine - Community
 Engine:
  Version:          29.8.0
  API version:      1.56 (minimum version 1.40)
 containerd:
  Version:          v2.3.5
`
	v := ParseVersion(rootful)
	if v.ServerVersion != "29.8.0" {
		t.Errorf("ServerVersion = %q", v.ServerVersion)
	}
	if v.Drivers() != "" {
		t.Errorf("Drivers() = %q, want empty for a rootful daemon", v.Drivers())
	}
}

func TestParseVersionEmpty(t *testing.T) {
	if got := ParseVersion(""); got.Drivers() != "" || got.ServerVersion != "" {
		t.Errorf("ParseVersion(\"\") = %+v", got)
	}
}

package docker

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Info struct {
	ServerVersion   string   `json:"ServerVersion"`
	StorageDriver   string   `json:"Driver"`
	DataRoot        string   `json:"DockerRootDir"`
	SecurityOptions []string `json:"SecurityOptions"`
	CgroupDriver    string   `json:"CgroupDriver"`
	CgroupVersion   string   `json:"CgroupVersion"`
	Warnings        []string `json:"Warnings"`
	Name            string   `json:"Name"`
	OSType          string   `json:"OSType"`
	Architecture    string   `json:"Architecture"`
}

func ParseInfo(b []byte) (Info, error) {
	var i Info
	if err := json.Unmarshal(b, &i); err != nil {
		return Info{}, fmt.Errorf("parse docker info JSON: %w", err)
	}
	if i.ServerVersion == "" {
		return Info{}, fmt.Errorf("docker info has no ServerVersion")
	}
	return i, nil
}

func (i Info) Rootless() bool {
	for _, opt := range i.SecurityOptions {
		for _, field := range strings.Split(opt, ",") {
			if strings.TrimPrefix(strings.TrimSpace(field), "name=") == "rootless" {
				return true
			}
		}
	}
	return false
}

func (i Info) OverlayStorage() bool { return strings.Contains(i.StorageDriver, "overlay") }

func (i Info) CgroupV2() bool { return i.CgroupVersion == "2" }

func (i Info) CgroupVersionInt() int {
	n, err := strconv.Atoi(strings.TrimSpace(i.CgroupVersion))
	if err != nil {
		return 0
	}
	return n
}

type Version struct {
	ServerVersion string
	RootlessKit   string
	NetworkDriver string
	PortDriver    string
	Slirp4netns   string
	ContainerdVer string
	RuncVer       string
	ClientVersion string
	ClientContext string
}

func ParseVersion(s string) Version {
	var v Version
	var top, section string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		key, value, hasValue := strings.Cut(strings.TrimSpace(line), ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch {
		case indent == 0:
			top, section = key, ""
			continue
		case !hasValue || value == "":
			section = key
			continue
		}
		switch {
		case top == "Client" && section == "":
			switch key {
			case "Version":
				v.ClientVersion = value
			case "Context":
				v.ClientContext = value
			}
		case section == "Engine" && key == "Version":
			v.ServerVersion = value
		case section == "rootlesskit":
			switch key {
			case "Version":
				v.RootlessKit = value
			case "NetworkDriver":
				v.NetworkDriver = value
			case "PortDriver":
				v.PortDriver = value
			}
		case section == "slirp4netns" && key == "Version":
			v.Slirp4netns = value
		case section == "containerd" && key == "Version":
			v.ContainerdVer = value
		case section == "runc" && key == "Version":
			v.RuncVer = value
		}
	}
	return v
}

func (v Version) Drivers() string {
	if v.NetworkDriver == "" && v.PortDriver == "" {
		return ""
	}
	return v.NetworkDriver + "/" + v.PortDriver
}

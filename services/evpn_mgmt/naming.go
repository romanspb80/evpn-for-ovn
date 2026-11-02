package main

import (
	"os"
	"os/exec"
	"strings"
)

func kubernetesNodeName() string {
	cmd := exec.Command("kubectl", "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return ""
	}
	return name
}

func ovnLogicalSwitchName() string {
	for _, key := range []string{"OVN_LS_NAME", "NETWORK_NAME", "NODE_NAME", "K8S_NODE_NAME"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	if value := kubernetesNodeName(); value != "" {
		return value
	}
	if value, err := os.Hostname(); err == nil && strings.TrimSpace(value) != "" && value != "localhost" && !strings.Contains(value, ".") {
		return value
	}
	return "ovn-local"
}

func networkName() string {
	return ovnLogicalSwitchName()
}

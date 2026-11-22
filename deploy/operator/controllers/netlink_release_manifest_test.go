package controllers

import (
	"bytes"
	"io"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

func TestOperatorCoreManifestExcludesLegacyManagementServices(t *testing.T) {
	manifest, err := os.ReadFile("../core-resources.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	forbidden := map[string]struct{}{
		"evpn-mgmt":      {},
		"ovsdb-mgmt":     {},
		"bgp-ad-handler": {},
	}
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(&obj.Object)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("decode Operator core manifest: %v", err)
		}
		if _, ok := forbidden[obj.GetName()]; ok {
			t.Errorf("legacy resource %s %q must not be in the Netlink core profile", obj.GetKind(), obj.GetName())
		}
		if obj.GetName() == "ovn-ovs" {
			containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			if err != nil {
				t.Fatalf("read OVN deployment containers: %v", err)
			}
			for _, value := range containers {
				container, ok := value.(map[string]interface{})
				if !ok {
					t.Fatal("invalid OVN deployment container")
				}
				if container["name"] == "bgp-ad-handler-agent" {
					t.Fatal("legacy Netlink writer must not be deployed beside the Node-Agent")
				}
			}
		}
	}
}

func TestLegacyKindManagementServicesAreClusterInternal(t *testing.T) {
	manifest, err := os.ReadFile("../../kind/k8s.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	management := map[string]struct{}{
		"evpn-mgmt":  {},
		"ovsdb-mgmt": {},
	}
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(&obj.Object)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("decode legacy Kind manifest: %v", err)
		}
		name := obj.GetName()
		if _, ok := management[name]; ok {
			switch obj.GetKind() {
			case "Service":
				serviceType, found, err := unstructured.NestedString(obj.Object, "spec", "type")
				if err != nil {
					t.Fatalf("read %s Service type: %v", name, err)
				}
				if !found || serviceType != "ClusterIP" {
					t.Errorf("management Service %q must be ClusterIP, got %q", name, serviceType)
				}
				ports, _, err := unstructured.NestedSlice(obj.Object, "spec", "ports")
				if err != nil {
					t.Fatalf("read %s Service ports: %v", name, err)
				}
				for _, value := range ports {
					port, ok := value.(map[string]interface{})
					if !ok {
						t.Fatalf("invalid port in %s Service", name)
					}
					if _, found := port["nodePort"]; found {
						t.Errorf("management Service %q must not publish a NodePort", name)
					}
				}
			case "Deployment":
				hostNetwork, found, err := unstructured.NestedBool(obj.Object, "spec", "template", "spec", "hostNetwork")
				if err != nil {
					t.Fatalf("read %s Deployment hostNetwork: %v", name, err)
				}
				if found && hostNetwork {
					t.Errorf("management Deployment %q must not use host networking", name)
				}
			}
		}
		if obj.GetAPIVersion() == "gateway.networking.k8s.io/v1" &&
			(obj.GetKind() == "Gateway" || obj.GetKind() == "GRPCRoute") {
			t.Errorf("legacy management manifest must not define external Gateway resources: %s %q", obj.GetKind(), name)
		}
	}
}

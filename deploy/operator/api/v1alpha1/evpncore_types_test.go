package v1alpha1

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

func TestAPIAndManifestSchemaConsistency(t *testing.T) {
	disabled, err := json.Marshal(EvpnCoreSpec{Enabled: false})
	if err != nil || !bytes.Contains(disabled, []byte(`"enabled":false`)) {
		t.Fatalf("required enabled field omitted: %s %v", disabled, err)
	}
	if SchemeGroupVersion.Group != "evpn.romanspb80.github.io" {
		t.Fatal("wrong API group")
	}
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	kinds, _, err := s.ObjectKinds(&EvpnCore{})
	if err != nil || len(kinds) != 1 || kinds[0] != SchemeGroupVersion.WithKind("EvpnCore") {
		t.Fatalf("registration: %v %v", kinds, err)
	}
	data, err := os.ReadFile("../../manifests/operator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("evpn.core.example.com")) {
		t.Fatal("placeholder group in install manifest")
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	found := false
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if obj.GetKind() != "CustomResourceDefinition" {
			continue
		}
		found = true
		var crd apiextensionsv1.CustomResourceDefinition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &crd); err != nil {
			t.Fatal(err)
		}
		if crd.Spec.Group != SchemeGroupVersion.Group || crd.Name != "evpncores."+SchemeGroupVersion.Group ||
			crd.Spec.Scope != apiextensionsv1.NamespaceScoped || crd.Spec.Versions[0].Name != SchemeGroupVersion.Version {
			t.Fatal("CRD/Go API disagreement")
		}
		properties := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties
		checkSchemaFields(t, reflect.TypeOf(EvpnCoreSpec{}), properties["spec"], "spec")
		checkSchemaFields(t, reflect.TypeOf(EvpnCoreStatus{}), properties["status"], "status")
		bgp := properties["spec"].Properties["bgp"]
		provider := bgp.Properties["provider"]
		if bgp.Default == nil || provider.Default == nil || string(provider.Default.Raw) != `"k8gobgp"` ||
			len(provider.Enum) != 1 || string(provider.Enum[0].Raw) != `"k8gobgp"` {
			t.Fatal("provider default/rejection differs from runtime contract")
		}
	}
	if !found {
		t.Fatal("no CRD in install manifest")
	}
	example, err := os.ReadFile("../../examples/evpncore.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(example), "apiVersion: "+SchemeGroupVersion.String()) {
		t.Fatal("example group mismatch")
	}
}

func TestAdministrativeRBACBoundary(t *testing.T) {
	data, err := os.ReadFile("../../manifests/operator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	admin, operator, leaseRole, bindings := false, false, false, 0
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		switch obj.GetKind() {
		case "ClusterRole":
			var role rbacv1.ClusterRole
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role); err != nil {
				t.Fatal(err)
			}
			if role.AggregationRule != nil || len(role.Labels) != 0 {
				t.Fatal("shipped role must not aggregate into ordinary edit/admin roles")
			}
			for _, rule := range role.Rules {
				for _, resource := range rule.Resources {
					if resource == "leases" || resource == "pods" {
						t.Fatalf("unneeded cluster-wide privilege: %+v", rule)
					}
					if resource != "evpncores" {
						continue
					}
					switch role.Name {
					case "evpn-core-administrator":
						admin = true
						for _, verb := range []string{"create", "update", "patch", "delete"} {
							if !hasVerb(rule.Verbs, verb) {
								t.Fatalf("admin missing %s", verb)
							}
						}
					case "evpn-core-operator":
						operator = true
						for _, verb := range []string{"create", "update", "delete", "*"} {
							if hasVerb(rule.Verbs, verb) {
								t.Fatalf("operator has unnecessary CR permission %s", verb)
							}
						}
					default:
						t.Fatal("unexpected CR writer role")
					}
				}
			}
		case "Role":
			var role rbacv1.Role
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role); err != nil {
				t.Fatal(err)
			}
			leaseRole = role.Namespace == "evpn-operator-system" && len(role.Rules) == 1 && reflect.DeepEqual(role.Rules[0].Resources, []string{"leases"})
		case "ClusterRoleBinding", "RoleBinding":
			var binding rbacv1.ClusterRoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &binding); err != nil {
				t.Fatal(err)
			}
			bindings++
			if binding.RoleRef.Name == "evpn-core-administrator" {
				t.Fatal("administrative role must not have a default binding")
			}
			if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" ||
				binding.Subjects[0].Namespace != "evpn-operator-system" || binding.Subjects[0].Name != "evpn-core-operator" {
				t.Fatal("shipped binding delegates operator authority to ordinary users")
			}
		}
	}
	if !admin || !operator || !leaseRole || bindings != 2 {
		t.Fatalf("incomplete administrative boundary: admin=%t operator=%t leases=%t bindings=%d", admin, operator, leaseRole, bindings)
	}
}

func hasVerb(verbs []string, wanted string) bool {
	for _, verb := range verbs {
		if verb == wanted {
			return true
		}
	}
	return false
}

func checkSchemaFields(t *testing.T, typ reflect.Type, schema apiextensionsv1.JSONSchemaProps, path string) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		prop, ok := schema.Properties[name]
		if !ok {
			t.Errorf("%s.%s missing from CRD schema", path, name)
			continue
		}
		if field.Type.Kind() == reflect.Struct && field.Type.PkgPath() == typ.PkgPath() {
			checkSchemaFields(t, field.Type, prop, path+"."+name)
		}
		if field.Type.Kind() == reflect.Slice && field.Type.Elem().Kind() == reflect.Struct && field.Type.Elem().PkgPath() == typ.PkgPath() {
			if prop.Items == nil || prop.Items.Schema == nil {
				t.Errorf("%s.%s has no item schema", path, name)
				continue
			}
			checkSchemaFields(t, field.Type.Elem(), *prop.Items.Schema, path+"."+name+"[]")
		}
	}
}

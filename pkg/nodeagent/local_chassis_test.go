package nodeagent

import (
	"testing"

	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestLocalOVNChassisBinding(t *testing.T) {
	root := &vswitch.OpenvSwitch{UUID: "ovs-root", ExternalIDs: map[string]string{"system-id": "chassis-a"}}
	chassis := &exportChassis{UUID: "chassis-uuid", Name: "chassis-a"}
	for _, tc := range []struct {
		name     string
		expected string
		roots    []*vswitch.OpenvSwitch
		chassis  []*exportChassis
		wantErr  bool
	}{
		{"matching", "chassis-a", []*vswitch.OpenvSwitch{root}, []*exportChassis{chassis}, false},
		{"wrong local OVS", "chassis-b", []*vswitch.OpenvSwitch{root}, []*exportChassis{chassis}, true},
		{"missing OVS root", "chassis-a", nil, []*exportChassis{chassis}, true},
		{"ambiguous OVS roots", "chassis-a", []*vswitch.OpenvSwitch{root, root}, []*exportChassis{chassis}, true},
		{"missing SB chassis", "chassis-a", []*vswitch.OpenvSwitch{root}, nil, true},
		{"ambiguous SB chassis", "chassis-a", []*vswitch.OpenvSwitch{root}, []*exportChassis{chassis, chassis}, true},
		{"missing chassis UUID", "chassis-a", []*vswitch.OpenvSwitch{root}, []*exportChassis{{Name: "chassis-a"}}, true},
		{"noncanonical name", " chassis-a", []*vswitch.OpenvSwitch{root}, []*exportChassis{chassis}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLocalOVNChassis(tc.expected, tc.roots, tc.chassis)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

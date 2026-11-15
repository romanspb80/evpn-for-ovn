package nodeagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/client"
	vswitch "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func verifyLocalOVNChassis(ctx context.Context, ovs, sb client.Client, expected string) error {
	var roots []*vswitch.OpenvSwitch
	if err := selectExportRows(ctx, ovs, "Open_vSwitch", []string{"_uuid", "external_ids"}, &roots); err != nil {
		return fmt.Errorf("local OVN chassis verification: %w", err)
	}
	var chassis []*exportChassis
	if err := selectExportRows(ctx, sb, "Chassis", []string{"_uuid", "name"}, &chassis); err != nil {
		return fmt.Errorf("local OVN chassis verification: %w", err)
	}
	return validateLocalOVNChassis(expected, roots, chassis)
}

func validateLocalOVNChassis(expected string, roots []*vswitch.OpenvSwitch, chassis []*exportChassis) error {
	if expected == "" || strings.TrimSpace(expected) != expected {
		return fmt.Errorf("local OVN chassis requires an explicit canonical name")
	}
	if len(roots) != 1 || roots[0] == nil || roots[0].UUID == "" {
		return fmt.Errorf("local OVS must expose exactly one identified Open_vSwitch root")
	}
	if roots[0].ExternalIDs["system-id"] != expected {
		return fmt.Errorf("configured OVN chassis %q differs from local OVS system-id", expected)
	}
	count := 0
	for _, row := range chassis {
		if row != nil && row.Name == expected {
			if row.UUID == "" {
				return fmt.Errorf("local OVN Southbound chassis has no UUID")
			}
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("configured OVN chassis %q has %d Southbound matches; expected one", expected, count)
	}
	return nil
}

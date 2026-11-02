package ovnnb

import (
	"strings"
	"testing"
)

func TestNorthboundSchemaTracksPinnedRelease(t *testing.T) {
	if _, err := FullDatabaseModel(); err != nil {
		t.Fatalf("FullDatabaseModel() returned unexpected error: %v", err)
	}

	if !strings.Contains(schema, "\"name\": \"OVN_Northbound\"") {
		t.Fatalf("schema name drifted from OVN Northbound: %s", schema[:200])
	}

	if !strings.Contains(schema, "\"version\": \"7.19.0\"") {
		t.Fatalf("OVN schema version drifted from v26.09.0: %s", schema[:200])
	}

	// Representative columns from the OVN v26.09.0 NB schema; if these disappear,
	// the generated model is no longer aligned with the pinned runtime baseline.
	for _, needle := range []string{"\"dhcp_relay\"", "\"sample_est\"", "\"sample_new\"", "\"status\""} {
		if !strings.Contains(schema, needle) {
			t.Fatalf("OVN schema is missing expected field %q: %s", needle, schema[:200])
		}
	}

	var lrp LogicalRouterPort
	if lrp.DhcpRelay != nil {
		t.Fatal("LogicalRouterPort.DhcpRelay should be nil when zero-valued; compiler check ensures field exists")
	}

	var acl ACL
	_ = acl.Tier
	_ = acl.SampleEst
	_ = acl.SampleNew
}

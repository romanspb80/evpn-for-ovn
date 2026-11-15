package domain

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type goldenFixture struct {
	Cases []struct {
		Name     string   `json:"name"`
		Endpoint Endpoint `json:"endpoint"`
		Expected struct {
			OwnershipKey     string `json:"ownershipKey"`
			EndpointRouteKey string `json:"endpointRouteKey"`
			RouteIdentity    string `json:"routeIdentity"`
			RD               string `json:"rd"`
			ESI              string `json:"esi"`
			EthernetTag      uint32 `json:"ethernetTag"`
			MAC              string `json:"mac"`
			IP               string `json:"ip"`
		} `json:"expected"`
	} `json:"cases"`
}

func loadGoldenFixture(t *testing.T) goldenFixture {
	t.Helper()
	raw, err := os.ReadFile("../testdata/type2-domain-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture goldenFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestGoldenEndpointAndRouteIdentity(t *testing.T) {
	fixture := loadGoldenFixture(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := tc.Endpoint.OwnershipKey(); got != tc.Expected.OwnershipKey {
				t.Fatalf("ownership key: got %q want %q", got, tc.Expected.OwnershipKey)
			}
			if got := tc.Endpoint.RouteKey(); got != tc.Expected.EndpointRouteKey {
				t.Fatalf("endpoint route key: got %q want %q", got, tc.Expected.EndpointRouteKey)
			}
			if got := PrimaryIP(tc.Endpoint.IPAddresses); got != tc.Expected.IP {
				t.Fatalf("primary IP: got %q want %q", got, tc.Expected.IP)
			}
			if got, err := CanonicalMAC(tc.Endpoint.MACAddress); err != nil || got != tc.Expected.MAC {
				t.Fatalf("canonical MAC: got %q err=%v want %q", got, err, tc.Expected.MAC)
			}
			identity := RouteIdentity{
				RD: tc.Expected.RD, ESI: tc.Expected.ESI, EthernetTag: tc.Expected.EthernetTag,
				MAC: tc.Expected.MAC, IP: tc.Expected.IP,
			}
			if got := identity.Key(); got != tc.Expected.RouteIdentity {
				t.Fatalf("route identity: got %q want %q", got, tc.Expected.RouteIdentity)
			}
			if !identity.Equal(identity) {
				t.Fatal("identity did not equal itself")
			}
		})
	}
}

func TestPlanIsDeterministicAndWithdrawalFirst(t *testing.T) {
	first := Endpoint{
		NetworkID: "n", EndpointID: "first", Incarnation: "i1", MACAddress: "02:00:00:00:00:01",
		IPAddresses: []string{"192.0.2.1"}, VNI: 100, RouteDistinguisher: "65000:1",
		RouteTargets: []string{"65000:1"}, LocalVTEP: "192.0.2.10", Advertise: true,
	}
	second := first
	second.EndpointID, second.MACAddress = "second", "02:00:00:00:00:02"
	desired := []DesiredRoute{{Key: "wire-b", Endpoint: second}, {Key: "wire-a", Endpoint: first}}
	wantAdds := []Operation{{Action: Advertise, Key: "wire-a"}, {Action: Advertise, Key: "wire-b"}}
	got, err := Plan(desired, nil, nil)
	if err != nil || !reflect.DeepEqual(got, wantAdds) {
		t.Fatalf("initial plan: got=%+v err=%v want=%+v", got, err, wantAdds)
	}
	reversed, err := Plan([]DesiredRoute{desired[1], desired[0]}, nil, nil)
	if err != nil || !reflect.DeepEqual(reversed, wantAdds) {
		t.Fatalf("input ordering changed plan: got=%+v err=%v", reversed, err)
	}
	duplicates, err := Plan([]DesiredRoute{{Key: "wire-a", Endpoint: first}, {Key: "wire-a", Endpoint: first}}, nil, nil)
	if err != nil || !reflect.DeepEqual(duplicates, []Operation{{Action: Advertise, Key: "wire-a"}}) {
		t.Fatalf("duplicate equivalent observation: got=%+v err=%v", duplicates, err)
	}

	updated := first
	updated.LocalVTEP = "192.0.2.20"
	owned := []OwnedRoute{
		{Key: "wire-b", Endpoint: second, PathID: "uuid-b"},
		{Key: "wire-a", Endpoint: first, PathID: "uuid-a"},
	}
	got, err = Plan([]DesiredRoute{{Key: "wire-a", Endpoint: updated}}, owned, nil)
	want := []Operation{
		{Action: Withdraw, Key: "wire-a", PathID: "uuid-a"},
		{Action: Withdraw, Key: "wire-b", PathID: "uuid-b"},
		{Action: Advertise, Key: "wire-a"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("update/withdraw plan: got=%+v err=%v want=%+v", got, err, want)
	}
	noOp, err := Plan([]DesiredRoute{{Key: "wire-a", Endpoint: first}}, []OwnedRoute{
		{Key: "wire-a", Endpoint: first, PathID: "uuid-a"},
	}, nil)
	if err != nil || len(noOp) != 0 {
		t.Fatalf("no-op plan: got=%+v err=%v", noOp, err)
	}
}

func TestPlanRejectsConflictingAndForeignRoutes(t *testing.T) {
	first := Endpoint{EndpointID: "owner-a", Incarnation: "one", Advertise: true}
	conflict := first
	conflict.Incarnation = "two"
	if _, err := Plan([]DesiredRoute{
		{Key: "wire", Endpoint: first}, {Key: "wire", Endpoint: conflict},
	}, nil, nil); err == nil {
		t.Fatal("conflicting desired owners accepted")
	}
	if _, err := Plan([]DesiredRoute{{Key: "wire", Endpoint: first}}, nil, map[string]bool{"wire": true}); err == nil {
		t.Fatal("unclaimed foreign route was not protected")
	}
}

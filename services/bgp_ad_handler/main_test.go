package main

import (
	"testing"

	"github.com/osrg/gobgp/v4/api"
)

func pathWithPeerAndNextHop(peer string, nextHops ...string) *api.Path {
	return &api.Path{
		NeighborIp: peer,
		Pattrs: []*api.Attribute{{
			Attr: &api.Attribute_MpReach{
				MpReach: &api.MpReachNLRIAttribute{NextHops: nextHops},
			},
		}},
	}
}

func TestRemoteVTEPFromPathUsesEVPNNextHop(t *testing.T) {
	tests := []struct {
		name string
		path *api.Path
		want string
	}{
		{
			name: "use advertised next hop instead of BGP peer",
			path: pathWithPeerAndNextHop("192.0.2.2", "192.0.2.4"),
			want: "192.0.2.4",
		},
		{
			name: "do not use peer for local unspecified next hop",
			path: pathWithPeerAndNextHop("192.0.2.2", "0.0.0.0"),
		},
		{
			name: "do not use peer when next hop is missing",
			path: &api.Path{NeighborIp: "192.0.2.2"},
		},
		{
			name: "do not use peer when next hop is invalid",
			path: pathWithPeerAndNextHop("192.0.2.2", "not-an-ip"),
		},
		{
			name: "nil path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := remoteVTEPFromPath(test.path); got != test.want {
				t.Fatalf("remoteVTEPFromPath() = %q, want %q", got, test.want)
			}
		})
	}
}

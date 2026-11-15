package domain

import "fmt"

// RouteIdentity is the Type-2 wire prefix identity; labels are replacement
// attributes and are intentionally excluded.
type RouteIdentity struct {
	RD          string
	ESI         string
	EthernetTag uint32
	MAC         string
	IP          string
}

func (r RouteIdentity) Key() string {
	return fmt.Sprintf("%q|%q|%d|%q|%q", r.RD, r.ESI, r.EthernetTag, r.MAC, r.IP)
}

func (r RouteIdentity) Equal(other RouteIdentity) bool {
	return r.RD == other.RD && r.ESI == other.ESI && r.EthernetTag == other.EthernetTag &&
		r.MAC == other.MAC && r.IP == other.IP
}

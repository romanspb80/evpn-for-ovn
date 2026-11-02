package evpn

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The typed library silently drops NDA_PORT, protocol, nexthop IDs and newer
// attributes. Refuse representations the receipt cannot describe or delete.
type ownedKernelHandle struct {
	*netlink.Handle
	dump *nl.SocketHandle
}

func newOwnedKernelHandle(ns netns.NsHandle) (*ownedKernelHandle, error) {
	h, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	socket, err := nl.GetNetlinkSocketAt(ns, netns.None(), unix.NETLINK_ROUTE)
	if err != nil {
		h.Close()
		return nil, err
	}
	timeout := unix.Timeval{Sec: 2}
	if err := errors.Join(socket.SetSendTimeout(&timeout), socket.SetReceiveTimeout(&timeout)); err != nil {
		socket.Close()
		h.Close()
		return nil, err
	}
	return &ownedKernelHandle{Handle: h, dump: &nl.SocketHandle{Socket: socket}}, nil
}

func (h *ownedKernelHandle) Close() error {
	return errors.Join(h.Handle.Close(), h.dump.Close())
}

func (h *ownedKernelHandle) NeighList(index, family int) ([]netlink.Neigh, error) {
	request := nl.NewNetlinkRequest(unix.RTM_GETNEIGH, unix.NLM_F_DUMP)
	request.Sockets = map[int]*nl.SocketHandle{unix.NETLINK_ROUTE: h.dump}
	request.AddData(&netlink.Ndmsg{Family: uint8(family), Index: uint32(index)})
	messages, err := request.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWNEIGH)
	if err != nil {
		return nil, err
	}
	var entries []netlink.Neigh
	for _, message := range messages {
		headerSize := (&netlink.Ndmsg{}).Len()
		if len(message) < headerSize {
			return nil, fmt.Errorf("truncated neighbor dump header")
		}
		if int(message[0]) != family || int(nl.NativeEndian().Uint32(message[4:8])) != index {
			continue
		}
		if err := validateOwnedNeighborAttributes(message[headerSize:], index); err != nil {
			return nil, err
		}
		entry, err := netlink.NeighDeserialize(message)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}

func validateOwnedNeighborAttributes(data []byte, index int) error {
	attributes, err := nl.ParseRouteAttr(data)
	if err != nil {
		return err
	}
	seen := map[uint16]bool{}
	for _, attribute := range attributes {
		kind, value := attribute.Attr.Type, attribute.Value
		if seen[kind] {
			return fmt.Errorf("duplicate neighbor attribute %d", kind)
		}
		seen[kind] = true
		valid := false
		switch kind {
		case netlink.NDA_DST:
			valid = len(value) == 4 || len(value) == 16
		case netlink.NDA_LLADDR:
			valid = len(value) == 6
		case netlink.NDA_VLAN:
			valid = len(value) == 2
		case netlink.NDA_VNI, netlink.NDA_MASTER, netlink.NDA_FLAGS_EXT, netlink.NDA_PROBES:
			valid = len(value) == 4
		case netlink.NDA_IFINDEX:
			valid = len(value) == 4 && int(nl.NativeEndian().Uint32(value)) == index
		case netlink.NDA_CACHEINFO:
			valid = len(value) == 16
		default:
			return fmt.Errorf("unsupported neighbor attribute %d; ownership observation refused", kind)
		}
		if !valid {
			return fmt.Errorf("invalid neighbor attribute %d", kind)
		}
	}
	return nil
}

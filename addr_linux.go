package netlink

import (
	"fmt"
	"log"
	"net"
	"strings"
	"syscall"

	"github.com/currantlabs/netlink/nl"
)

// IFA_FLAGS is a u32 attribute.
const IFA_FLAGS = 0x8

// AddrAdd will add an IP address to a link device.
// Equivalent to: `ip addr add $addr dev $link`
func AddrAdd(link Link, addr *Addr) error {

	req := nl.NewNetlinkRequest(syscall.RTM_NEWADDR, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL|syscall.NLM_F_ACK)
	return addrHandle(link, addr, req)
}

// AddrDel will delete an IP address from a link device.
// Equivalent to: `ip addr del $addr dev $link`
func AddrDel(link Link, addr *Addr) error {
	req := nl.NewNetlinkRequest(syscall.RTM_DELADDR, syscall.NLM_F_ACK)
	return addrHandle(link, addr, req)
}

func addrHandle(link Link, addr *Addr, req *nl.NetlinkRequest) error {
	base := link.Attrs()
	if addr.Label != "" && !strings.HasPrefix(addr.Label, base.Name) {
		return fmt.Errorf("label must begin with interface name")
	}
	ensureIndex(base)

	family := nl.GetIPFamily(addr.IP)

	msg := nl.NewIfAddrmsg(family)
	msg.Index = uint32(base.Index)
	msg.Scope = uint8(addr.Scope)
	prefixlen, _ := addr.Mask.Size()
	msg.Prefixlen = uint8(prefixlen)
	req.AddData(msg)

	var addrData []byte
	if family == FAMILY_V4 {
		addrData = addr.IP.To4()
	} else {
		addrData = addr.IP.To16()
	}

	localData := nl.NewRtAttr(syscall.IFA_LOCAL, addrData)
	req.AddData(localData)

	addressData := nl.NewRtAttr(syscall.IFA_ADDRESS, addrData)
	req.AddData(addressData)

	if addr.Flags != 0 {
		b := make([]byte, 4)
		native.PutUint32(b, uint32(addr.Flags))
		flagsData := nl.NewRtAttr(IFA_FLAGS, b)
		req.AddData(flagsData)
	}

	if addr.Label != "" {
		labelData := nl.NewRtAttr(syscall.IFA_LABEL, nl.ZeroTerminated(addr.Label))
		req.AddData(labelData)
	}

	_, err := req.Execute(syscall.NETLINK_ROUTE, 0)
	return err
}

// AddrList gets a list of IP addresses in the system.
// Equivalent to: `ip addr show`.
// The list can be filtered by link and ip family.
func AddrList(link Link, family int) ([]Addr, error) {
	req := nl.NewNetlinkRequest(syscall.RTM_GETADDR, syscall.NLM_F_DUMP)
	msg := nl.NewIfInfomsg(family)
	req.AddData(msg)

	msgs, err := req.Execute(syscall.NETLINK_ROUTE, syscall.RTM_NEWADDR)
	if err != nil {
		return nil, err
	}

	index := 0
	if link != nil {
		base := link.Attrs()
		ensureIndex(base)
		index = base.Index
	}

	var res []Addr
	for _, m := range msgs {
		msg := nl.DeserializeIfAddrmsg(m)

		if link != nil && msg.Index != uint32(index) {
			// Ignore messages from other interfaces
			continue
		}

		attrs, err := nl.ParseRouteAttr(m[msg.Len():])
		if err != nil {
			return nil, err
		}

		var local, dst *net.IPNet
		var addr Addr
		for _, attr := range attrs {
			switch attr.Attr.Type {
			case syscall.IFA_ADDRESS:
				dst = &net.IPNet{
					IP:   attr.Value,
					Mask: net.CIDRMask(int(msg.Prefixlen), 8*len(attr.Value)),
				}
			case syscall.IFA_LOCAL:
				local = &net.IPNet{
					IP:   attr.Value,
					Mask: net.CIDRMask(int(msg.Prefixlen), 8*len(attr.Value)),
				}
			case syscall.IFA_LABEL:
				addr.Label = string(attr.Value[:len(attr.Value)-1])
			case IFA_FLAGS:
				addr.Flags = int(native.Uint32(attr.Value[0:4]))
			}
		}

		// IFA_LOCAL should be there but if not, fall back to IFA_ADDRESS
		if local != nil {
			addr.IPNet = local
		} else {
			addr.IPNet = dst
		}
		addr.Scope = int(msg.Scope)

		res = append(res, addr)
	}

	return res, nil
}

type AddrUpdate struct {
	LinkAddress net.IPNet
	LinkIndex   int
	NewAddr     bool // true=added false=deleted
}

// AddrSubscribe takes a chan down which notifications will be sent
// when addresses change.  Close the 'done' chan to stop subscription.
func AddrSubscribe(ch chan<- AddrUpdate, done <-chan struct{}) error {
	s, err := nl.Subscribe(syscall.NETLINK_ROUTE, syscall.RTNLGRP_IPV4_IFADDR, syscall.RTNLGRP_IPV6_IFADDR)
	if err != nil {
		return err
	}
	if done != nil {
		go func() {
			<-done
			s.Close()
		}()
	}
	go func() {
		defer close(ch)
		for {
			msgs, err := s.Receive()
			if err != nil {
				log.Printf("netlink.AddrSubscribe: Receive() error: %v", err)
				return
			}
			for _, m := range msgs {
				msgType := m.Header.Type
				if msgType != syscall.RTM_NEWADDR && msgType != syscall.RTM_DELADDR {
					log.Printf("netlink.AddrSubscribe: bad message type: %d", msgType)
					continue
				}

				msg := nl.DeserializeIfAddrmsg(m.Data)

				attrs, err := nl.ParseRouteAttr(m.Data[msg.Len():])
				if err != nil {
					log.Printf("netlink.AddrSubscribe: nl.ParseRouteAttr() error: %v", err)
					return
				}

				var local, dst *net.IPNet
				var addr Addr
				for _, attr := range attrs {
					switch attr.Attr.Type {
					case syscall.IFA_ADDRESS:
						dst = &net.IPNet{
							IP:   attr.Value,
							Mask: net.CIDRMask(int(msg.Prefixlen), 8*len(attr.Value)),
						}
					case syscall.IFA_LOCAL:
						local = &net.IPNet{
							IP:   attr.Value,
							Mask: net.CIDRMask(int(msg.Prefixlen), 8*len(attr.Value)),
						}
					case syscall.IFA_LABEL:
						addr.Label = string(attr.Value[:len(attr.Value)-1])
					case IFA_FLAGS:
						addr.Flags = int(native.Uint32(attr.Value[0:4]))
					}
				}

				// IFA_LOCAL should be there but if not, fall back to IFA_ADDRESS
				if local != nil {
					addr.IPNet = local
				} else {
					addr.IPNet = dst
				}

				ch <- AddrUpdate{LinkAddress: *addr.IPNet, LinkIndex: int(msg.Index), NewAddr: msgType == syscall.RTM_NEWADDR}
			}
		}
	}()

	return nil
}

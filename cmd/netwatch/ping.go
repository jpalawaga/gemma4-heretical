package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// magic is embedded in every echo payload so we never mistake an unrelated
// ICMP reply (or a late reply to an earlier probe) for the one we're waiting on.
var magic = []byte("netwatch")

// pinger sends ICMP echo requests over a single shared socket. It is not safe
// for concurrent use; the monitor loop probes one target at a time.
type pinger struct {
	conn *icmp.PacketConn
	udp  bool // true when using the unprivileged SOCK_DGRAM path
	id   int
	seq  uint16
}

// newPinger prefers Linux's unprivileged ICMP datagram socket, which works for
// any process whose GID falls inside net.ipv4.ping_group_range. If that is
// unavailable it falls back to a raw socket, which needs root or CAP_NET_RAW.
func newPinger() (*pinger, error) {
	if c, err := icmp.ListenPacket("udp4", "0.0.0.0"); err == nil {
		return &pinger{conn: c, udp: true, id: os.Getpid() & 0xffff}, nil
	} else {
		c, rawErr := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
		if rawErr != nil {
			return nil, fmt.Errorf("no usable ICMP socket (unprivileged: %v; raw: %v)", err, rawErr)
		}
		return &pinger{conn: c, id: os.Getpid() & 0xffff}, nil
	}
}

func (p *pinger) Close() error { return p.conn.Close() }

// resolve turns a target into an IPv4 address. IP literals skip DNS entirely,
// which matters here: during an outage the resolver is usually dead too, and we
// want the probe to fail on its own timeout rather than on a DNS stall.
func resolve(target string, timeout time.Duration) (*net.IPAddr, error) {
	if ip := net.ParseIP(target); ip != nil {
		v4 := ip.To4()
		if v4 == nil {
			return nil, fmt.Errorf("%s is not an IPv4 address", target)
		}
		return &net.IPAddr{IP: v4}, nil
	}
	r := &net.Resolver{}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, err := r.LookupIPAddr(ctx, target)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if v4 := a.IP.To4(); v4 != nil {
			return &net.IPAddr{IP: v4}, nil
		}
	}
	return nil, fmt.Errorf("no IPv4 address for %s", target)
}

// Ping sends one echo request and waits for the matching reply. A nil error
// means the target answered within timeout.
func (p *pinger) Ping(dst *net.IPAddr, timeout time.Duration) (time.Duration, error) {
	p.seq++
	seq := int(p.seq)

	payload := make([]byte, 8+len(magic))
	binary.BigEndian.PutUint64(payload, uint64(seq))
	copy(payload[8:], magic)

	wb, err := (&icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{ID: p.id, Seq: seq, Data: payload},
	}).Marshal(nil)
	if err != nil {
		return 0, err
	}

	var dest net.Addr = dst
	if p.udp {
		// The datagram socket expects a UDPAddr; the kernel supplies the ID.
		dest = &net.UDPAddr{IP: dst.IP}
	}

	deadline := time.Now().Add(timeout)
	if err := p.conn.SetDeadline(deadline); err != nil {
		return 0, err
	}

	start := time.Now()
	if _, err := p.conn.WriteTo(wb, dest); err != nil {
		return 0, err
	}

	rb := make([]byte, 1500)
	for {
		n, peer, err := p.conn.ReadFrom(rb)
		if err != nil {
			return 0, err // includes the deadline expiring
		}
		if !peerIP(peer).Equal(dst.IP) {
			continue
		}
		rm, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), rb[:n])
		if err != nil {
			continue
		}
		echo, ok := rm.Body.(*icmp.Echo)
		if !ok || rm.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		// The ID is rewritten by the kernel on unprivileged sockets, so the
		// sequence number and payload are what actually identify our reply.
		if echo.Seq != seq || !bytes.Equal(echo.Data, payload) {
			continue
		}
		return time.Since(start), nil
	}
}

func peerIP(addr net.Addr) net.IP {
	switch a := addr.(type) {
	case *net.IPAddr:
		return a.IP
	case *net.UDPAddr:
		return a.IP
	}
	return nil
}

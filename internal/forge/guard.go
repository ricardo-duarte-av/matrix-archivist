package forge

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// IsInternal reports addresses on loopback, private, link-local, CGNAT and similar networks.
func IsInternal(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip)
}

// BlockInternal makes the client refuse to connect to internal addresses. The check runs on
// the resolved IP of every connection (including redirects), so a public-looking hostname
// that resolves to an internal address is refused too.
func (c *Client) BlockInternal() {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("refusing connection to %s: %w", address, err)
			}
			if IsInternal(ap.Addr()) {
				return fmt.Errorf("refusing connection to internal address %s (set allow_internal_hosts to allow)", ap.Addr())
			}
			return nil
		},
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil // a proxy would hide the real destination
	t.DialContext = dialer.DialContext
	c.HTTP = &http.Client{Transport: t}
}

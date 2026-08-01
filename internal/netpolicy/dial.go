package netpolicy

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

type connectionDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type boundDialer struct {
	policy              *Policy
	endpoint            Endpoint
	dialer              connectionDialer
	tlsConfig           *tls.Config
	tlsHandshakeTimeout time.Duration
}

func (d *boundDialer) DialContext(ctx context.Context, network, authority string) (net.Conn, error) {
	if d == nil || d.policy == nil || d.dialer == nil || ctx == nil || !d.endpoint.valid() || !validDialNetwork(network) {
		return nil, policyError(CodeInvalidRequest)
	}
	hostname, port, err := net.SplitHostPort(authority)
	if err != nil || strings.ToLower(hostname) != d.endpoint.hostname || port != d.endpoint.port {
		return nil, policyError(CodeInvalidEndpoint)
	}
	if err := d.policy.revalidateEndpoint(ctx, d.endpoint); err != nil {
		return nil, err
	}
	for _, address := range d.endpoint.addresses {
		if !addressMatchesNetwork(address, network) {
			continue
		}
		connection, err := d.dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), d.endpoint.port))
		if err != nil {
			if ctx.Err() != nil {
				return nil, policyError(CodeDialFailed)
			}
			continue
		}
		if connection == nil {
			return nil, policyError(CodeDialFailed)
		}
		if err := d.validateRemoteAddress(connection.RemoteAddr()); err != nil {
			_ = connection.Close()
			return nil, err
		}
		return connection, nil
	}
	return nil, policyError(CodeDialFailed)
}

func (d *boundDialer) DialTLSContext(ctx context.Context, network, authority string) (net.Conn, error) {
	if d == nil || d.tlsConfig == nil || ctx == nil {
		return nil, policyError(CodeInvalidRequest)
	}
	connection, err := d.DialContext(ctx, network, authority)
	if err != nil {
		return nil, err
	}
	config := d.tlsConfig.Clone()
	if config.ServerName == "" {
		config.ServerName = d.endpoint.hostname
	}
	tlsConnection := tls.Client(connection, config)
	handshakeContext := ctx
	cancel := func() {}
	if d.tlsHandshakeTimeout > 0 {
		handshakeContext, cancel = context.WithTimeout(ctx, d.tlsHandshakeTimeout)
	}
	defer cancel()
	if err := tlsConnection.HandshakeContext(handshakeContext); err != nil {
		_ = connection.Close()
		return nil, policyError(CodeDialFailed)
	}
	return tlsConnection, nil
}

func (d *boundDialer) validateRemoteAddress(remote net.Addr) error {
	tcpAddress, ok := remote.(*net.TCPAddr)
	if !ok || tcpAddress == nil {
		return policyError(CodeAddressForbidden)
	}
	remoteAddress, ok := netip.AddrFromSlice(tcpAddress.IP)
	if !ok {
		return policyError(CodeAddressForbidden)
	}
	remoteAddress = remoteAddress.Unmap()
	port, err := strconv.Atoi(d.endpoint.port)
	if err != nil || tcpAddress.Port != port {
		return policyError(CodeAddressForbidden)
	}
	class, err := classifyAddress(remoteAddress)
	if err != nil || class != d.endpoint.AddressClass {
		return policyError(CodeAddressClassChanged)
	}
	for _, bound := range d.endpoint.addresses {
		if remoteAddress == bound {
			return nil
		}
	}
	return policyError(CodeAddressForbidden)
}

func validDialNetwork(network string) bool {
	return network == "tcp" || network == "tcp4" || network == "tcp6"
}

func addressMatchesNetwork(address netip.Addr, network string) bool {
	if network == "tcp4" {
		return address.Is4()
	}
	if network == "tcp6" {
		return address.Is6()
	}
	return true
}

package netpolicy

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestDialRejectsRebindingBeforeRequest(t *testing.T) {
	resolver := &fixedResolver{addresses: map[string][]net.IPAddr{
		"api.example.test": {{IP: net.ParseIP("203.0.113.10")}},
	}}
	policy := newPolicyWithResolver(resolver)
	endpoint, err := policy.ValidateInitial(context.Background(), "https://api.example.test/v1?opaque=dial-marker", PurposeProvider)
	if err != nil {
		t.Fatal("dial endpoint fixture was rejected")
	}
	clientValue, err := NewClientFactory(policy).New(endpoint, ClientOptions{})
	if err != nil {
		t.Fatal("controlled dial client construction failed")
	}
	client := clientValue.(*controlledClient)

	rebindingDialer := &dialTestDialer{remote: &net.TCPAddr{IP: net.ParseIP("10.0.0.9"), Port: 443}}
	client.transport.dialer.dialer = rebindingDialer
	resolver.addresses["api.example.test"] = []net.IPAddr{{IP: net.ParseIP("10.0.0.9")}}
	if _, err := client.transport.base.DialContext(context.Background(), "tcp", "api.example.test:443"); err == nil {
		t.Fatal("DNS rebinding was accepted by the dial path")
	}
	if len(rebindingDialer.addresses) != 0 {
		t.Fatal("DNS rebinding reached the operating-system dialer")
	}

	resolver.addresses["api.example.test"] = []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}
	wrongRemote := &dialTestDialer{remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.99"), Port: 443}}
	client.transport.dialer.dialer = wrongRemote
	if _, err := client.transport.base.DialContext(context.Background(), "tcp", "api.example.test:443"); err == nil {
		t.Fatal("a connection outside the bound address set was accepted")
	}
	if wrongRemote.connection == nil || wrongRemote.connection.writes != 0 || !wrongRemote.connection.closed {
		t.Fatal("an out-of-bounds connection was not closed before request bytes")
	}

	boundRemote := &dialTestDialer{remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 443}}
	client.transport.dialer.dialer = boundRemote
	connection, err := client.transport.base.DialContext(context.Background(), "tcp", "api.example.test:443")
	if err != nil {
		t.Fatal("the originally bound address was rejected")
	}
	defer connection.Close()
	if len(boundRemote.addresses) != 1 || boundRemote.addresses[0] != "203.0.113.10:443" {
		t.Fatal("dial path used DNS hostname instead of the bound address")
	}

	resolver.addresses["api.example.test"] = []net.IPAddr{{IP: net.ParseIP("10.0.0.9")}}
	tlsDialer := &dialTestDialer{remote: &net.TCPAddr{IP: net.ParseIP("10.0.0.9"), Port: 443}}
	client.transport.dialer.dialer = tlsDialer
	if _, err := client.transport.base.DialTLSContext(context.Background(), "tcp", "api.example.test:443"); err == nil {
		t.Fatal("TLS dial bypassed DNS rebinding protection")
	}
	if len(tlsDialer.addresses) != 0 {
		t.Fatal("TLS rebinding reached the operating-system dialer")
	}
}

type dialTestDialer struct {
	remote     net.Addr
	addresses  []string
	connection *dialTestConn
}

func (d *dialTestDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	d.connection = &dialTestConn{remote: d.remote}
	return d.connection, nil
}

type dialTestConn struct {
	remote net.Addr
	writes int
	closed bool
}

func (*dialTestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *dialTestConn) Write(data []byte) (int, error) { c.writes += len(data); return len(data), nil }
func (c *dialTestConn) Close() error                   { c.closed = true; return nil }
func (*dialTestConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *dialTestConn) RemoteAddr() net.Addr           { return c.remote }
func (*dialTestConn) SetDeadline(time.Time) error      { return nil }
func (*dialTestConn) SetReadDeadline(time.Time) error  { return nil }
func (*dialTestConn) SetWriteDeadline(time.Time) error { return nil }

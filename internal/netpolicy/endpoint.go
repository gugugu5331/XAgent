package netpolicy

import (
	"net/netip"
	"net/url"
)

type Purpose uint8

const (
	PurposeProvider Purpose = iota + 1
	PurposeMCP
	PurposeHook
)

func (p Purpose) valid() bool {
	return p == PurposeProvider || p == PurposeMCP || p == PurposeHook
}

type Origin string

type AddressClass uint8

const (
	AddressUnresolved AddressClass = iota
	AddressLoopback
	AddressPrivate
	AddressLinkLocal
	AddressPublic
)

type Endpoint struct {
	URL          *url.URL
	Origin       Origin
	AddressClass AddressClass
	Purpose      Purpose

	seal      *endpointSeal
	hostname  string
	port      string
	addresses []netip.Addr
}

type endpointSeal struct{}

func (e Endpoint) valid() bool {
	return e.seal != nil && e.URL != nil && e.Origin != "" && e.Purpose.valid() &&
		e.hostname != "" && e.port != "" && e.AddressClass != AddressUnresolved && len(e.addresses) != 0
}

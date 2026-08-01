package netpolicy

import "net/url"

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

	seal *endpointSeal
}

type endpointSeal struct{}

func (e Endpoint) valid() bool {
	return e.seal != nil && e.URL != nil && e.Origin != "" && e.Purpose.valid()
}

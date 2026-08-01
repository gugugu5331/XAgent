package netpolicy

import (
	"context"
	"net"
	"net/netip"
	"sort"
)

type addressResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

func newPolicyWithResolver(resolver addressResolver) *Policy {
	return &Policy{resolver: resolver}
}

func (p *Policy) resolve(ctx context.Context, hostname string) (AddressClass, []netip.Addr, error) {
	if p == nil || p.resolver == nil || ctx == nil || hostname == "" {
		return AddressUnresolved, nil, policyError(CodeResolutionFailed)
	}
	resolved, err := p.resolver.LookupIPAddr(ctx, hostname)
	if err != nil || len(resolved) == 0 {
		return AddressUnresolved, nil, policyError(CodeResolutionFailed)
	}
	unique := make(map[netip.Addr]struct{}, len(resolved))
	class := AddressUnresolved
	for _, candidate := range resolved {
		address, ok := netip.AddrFromSlice(candidate.IP)
		if !ok {
			return AddressUnresolved, nil, policyError(CodeAddressForbidden)
		}
		address = address.Unmap()
		candidateClass, err := classifyAddress(address)
		if err != nil {
			return AddressUnresolved, nil, err
		}
		if class == AddressUnresolved {
			class = candidateClass
		} else if class != candidateClass {
			return AddressUnresolved, nil, policyError(CodeAddressClassMixed)
		}
		unique[address] = struct{}{}
	}
	addresses := make([]netip.Addr, 0, len(unique))
	for address := range unique {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(first, second int) bool {
		return addresses[first].Compare(addresses[second]) < 0
	})
	return class, addresses, nil
}

func classifyAddress(address netip.Addr) (AddressClass, error) {
	if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() {
		return AddressUnresolved, policyError(CodeAddressForbidden)
	}
	if address.IsLoopback() {
		return AddressLoopback, nil
	}
	if address.IsPrivate() {
		return AddressPrivate, nil
	}
	if address.IsLinkLocalUnicast() {
		return AddressLinkLocal, nil
	}
	return AddressPublic, nil
}

func (p *Policy) revalidateEndpoint(ctx context.Context, endpoint Endpoint) error {
	if p == nil || ctx == nil || !endpoint.valid() {
		return policyError(CodeInvalidRequest)
	}
	if net.ParseIP(endpoint.hostname) != nil {
		return nil
	}
	class, _, err := p.resolve(ctx, endpoint.hostname)
	if err != nil {
		return err
	}
	if class != endpoint.AddressClass {
		return policyError(CodeAddressClassChanged)
	}
	return nil
}

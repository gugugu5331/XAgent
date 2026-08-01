package netpolicy

import (
	"context"
	"net/http"
	"net/url"
)

func (p *Policy) ValidateRequest(ctx context.Context, endpoint Endpoint, target *url.URL) error {
	return p.validateTarget(ctx, endpoint, target)
}

func (p *Policy) ValidateRedirect(ctx context.Context, endpoint Endpoint, next *url.URL) error {
	return p.validateTarget(ctx, endpoint, next)
}

func (p *Policy) validateTarget(ctx context.Context, endpoint Endpoint, target *url.URL) error {
	if p == nil || p.resolver == nil || ctx == nil || target == nil {
		return policyError(CodeInvalidRequest)
	}
	select {
	case <-ctx.Done():
		return policyError(CodeInvalidRequest)
	default:
	}
	bound, err := snapshotEndpoint(endpoint)
	if err != nil {
		return err
	}
	if target.Fragment != "" {
		return policyError(CodeInvalidEndpoint)
	}
	origin, err := requestOrigin(target)
	if err != nil {
		return err
	}
	if origin != bound.Origin {
		return policyError(CodeInvalidEndpoint)
	}
	return p.revalidateEndpoint(ctx, bound)
}

func (t *guardedTransport) checkRedirect(request *http.Request, _ []*http.Request) error {
	if t == nil || request == nil || request.URL == nil {
		return policyError(CodeInvalidRequest)
	}
	stripSensitiveHeaders(request.Header, t.sensitiveHeaders)
	return t.policy.ValidateRedirect(request.Context(), t.endpoint, request.URL)
}

package mcpclient

import "context"

type ClosableHTTPTransport struct {
	*HTTPTransport
}

func NewClosableHTTPTransport(config HTTPConfig) (*ClosableHTTPTransport, error) {
	transport, err := NewHTTPTransport(config)
	if err != nil {
		return nil, err
	}
	return &ClosableHTTPTransport{HTTPTransport: transport}, nil
}

func (t *ClosableHTTPTransport) Close(ctx context.Context) error {
	return nil
}

package provider

import (
	"errors"
	"net/http"

	"xagent/internal/redact"
)

type borrowedProviderTestClient struct {
	client *http.Client
}

func borrowProviderTestClient(client *http.Client) *borrowedProviderTestClient {
	return &borrowedProviderTestClient{client: client}
}

func providerTestRuntimeRedactor() *redact.RuntimeRedactor {
	return redact.NewRuntimeRedactor()
}

func (c *borrowedProviderTestClient) Do(request *http.Request) (*http.Response, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("borrowed provider test client is unavailable")
	}
	return c.client.Do(request)
}

func (c *borrowedProviderTestClient) SDKHTTPClient() *http.Client {
	if c == nil {
		return nil
	}
	return c.client
}

func (c *borrowedProviderTestClient) CloseIdleConnections() {
	if c != nil && c.client != nil {
		c.client.CloseIdleConnections()
	}
}

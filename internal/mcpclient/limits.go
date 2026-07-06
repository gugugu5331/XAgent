package mcpclient

import (
	"fmt"
	"io"
)

func maxResponseBytes(value int64) int64 {
	if value <= 0 {
		return DefaultMaxResponseBytes
	}
	return value
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("mcp response exceeds max response size")
	}
	return data, nil
}

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
)

type Client struct {
	socket    string
	transport *http.Transport
	http      *http.Client
}

func NewClient(socket string) *Client {
	transport := &http.Transport{
		// Each invocation has one connection; do not replay a CNI request after
		// a lost response or reuse a connection for transport-level retries.
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &Client{socket: socket, transport: transport, http: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) Close() { c.transport.CloseIdleConnections() }

func (c *Client) Call(ctx context.Context, request Request) (*Response, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("daemon request requires a caller deadline")
	}
	request.Version, request.Deadline = ProtocolVersion, deadline
	if err := request.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxMessageBytes {
		return nil, errors.New("daemon request exceeds size limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix"+requestPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxMessageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxMessageBytes {
		return nil, errors.New("daemon response exceeds size limit")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
	}
	var result Response
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	if (result.Error == nil) == (len(result.Result) == 0) {
		return nil, errors.New("daemon response must contain exactly one result or error")
	}
	return &result, nil
}

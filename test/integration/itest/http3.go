//go:build integration

package itest

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const (
	HTTP3Timeout          = 20 * time.Second
	HTTP3HandshakeTimeout = 10 * time.Second
)

func (c *EdgeClient) HTTP3Get(ctx context.Context, rawURL string) (*EdgeResponse, error) {
	tr := c.http3Transport()
	defer tr.Close()

	ctx, cancel := context.WithTimeout(ctx, c.timeout(HTTP3Timeout))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("http/3 GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("http/3 GET %s: %w", rawURL, err)
	}
	out := &EdgeResponse{
		Status: resp.StatusCode,
		Proto:  resp.Proto,
		Header: resp.Header,
		Body:   string(body),
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		out.Cert = resp.TLS.PeerCertificates[0]
	}
	return out, nil
}

func (c *EdgeClient) http3Transport() *http3.Transport {
	return &http3.Transport{
		TLSClientConfig: &tls.Config{RootCAs: c.Roots, MinVersion: tls.VersionTLS13},
		QUICConfig:      &quic.Config{HandshakeIdleTimeout: c.timeout(HTTP3HandshakeTimeout)},
		Dial: func(ctx context.Context, addr string, tlsConf *tls.Config, quicConf *quic.Config) (*quic.Conn, error) {
			target, err := c.hostAddr("udp", addr)
			if err != nil {
				return nil, err
			}
			remote, err := net.ResolveUDPAddr("udp", target)
			if err != nil {
				return nil, err
			}
			pc, err := net.ListenUDP("udp", nil)
			if err != nil {
				return nil, err
			}
			conn, err := quic.Dial(ctx, pc, remote, tlsConf, quicConf)
			if err != nil {
				pc.Close()
				return nil, err
			}
			return conn, nil
		},
	}
}

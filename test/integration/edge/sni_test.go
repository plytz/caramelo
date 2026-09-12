//go:build integration

package edge

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/test/integration/itest"
)

func TestSNIAndHostMustAgree(t *testing.T) {
	m := begin(t)
	needRepo(t)

	addr := m.HostAddr(t, 443)

	t.Run("a handshake with no server name gets no certificate", func(t *testing.T) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: itest.Scale(10 * time.Second)}, "tcp", addr,
			&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Logf("no sni: %v", err)
			return
		}
		defer conn.Close()
		certs := conn.ConnectionState().PeerCertificates
		if len(certs) == 0 {
			t.Log("no sni: the handshake completed without a certificate")
			return
		}
		for _, name := range certs[0].DNSNames {
			if name == hostX || name == hostSecond {
				t.Errorf("a handshake without SNI was served the certificate for %s; "+
					"a name is only ever served to a client that asked for it by name", name)
			}
		}
		t.Logf("no sni: served %q (%v)", certs[0].Subject.CommonName, certs[0].DNSNames)
	})

	t.Run("a routed sni does not smuggle an unrouted host into a pool", func(t *testing.T) {
		status, body, err := requestWithSNI(t, addr, hostX, hostUnknown, "/")
		if err != nil {
			t.Fatalf("%v", err)
		}
		t.Logf("sni %s, host %s: HTTP %d", hostX, hostUnknown, status)
		if status == http.StatusOK {
			t.Errorf("HTTP 200 for Host %s over a handshake for %s: the pool a request reaches "+
				"is decided by its Host header, so an unrouted Host must never be served", hostUnknown, hostX)
		}
		if strings.Contains(body, envX) {
			t.Errorf("the answer names %s: %q", envX, body)
		}
	})

	t.Run("the same connection still serves the name it was opened for", func(t *testing.T) {
		status, body, err := requestWithSNI(t, addr, hostX, hostX, "/")
		if err != nil {
			t.Fatalf("%v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("HTTP %d for Host %s over a handshake for %s, want 200", status, hostX, hostX)
		}
		if !strings.Contains(body, envX) {
			t.Errorf("GET %s = %q, want it to name the environment", urlX, body)
		}
	})
}

func requestWithSNI(t *testing.T, addr, serverName, hostHeader, path string) (int, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(30*time.Second))
	defer cancel()

	raw, err := (&net.Dialer{Timeout: itest.Scale(10 * time.Second)}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, "", fmt.Errorf("dial %s: %w", addr, err)
	}
	conn := tls.Client(raw, &tls.Config{
		ServerName: serverName,
		RootCAs:    internet.Roots,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	defer conn.Close()
	if err := conn.HandshakeContext(ctx); err != nil {
		return 0, "", fmt.Errorf("tls handshake for %s: %w", serverName, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return 0, "", err
		}
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: caramelo-itest\r\n\r\n",
		path, hostHeader)
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, "", fmt.Errorf("write the request: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		return 0, "", fmt.Errorf("read the response: %w", err)
	}
	defer resp.Body.Close()
	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		body.Write(buf[:n])
		if readErr != nil || body.Len() > 1<<20 {
			break
		}
	}
	return resp.StatusCode, body.String(), nil
}

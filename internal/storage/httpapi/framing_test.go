package httpapi

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memoryConn struct{ io.Reader }

func (*memoryConn) Write(data []byte) (int, error) { return len(data), nil }
func (*memoryConn) Close() error                   { return nil }
func (*memoryConn) LocalAddr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080} }
func (*memoryConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
}
func (*memoryConn) SetDeadline(time.Time) error      { return nil }
func (*memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (*memoryConn) SetWriteDeadline(time.Time) error { return nil }

func TestDuplicateContentLengthSurvivesHTTPParserNormalization(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		extra := ""
		if duplicate {
			extra = "content-length: 2\r\n"
		}
		// Exercise a long, valid header across bufio's read fragments as well.
		wire := "POST /api/pools/plan HTTP/1.1\r\nHost: localhost\r\nX-Padding: " + strings.Repeat("x", 6000) + "\r\nContent-Type: application/json\r\nContent-Length: 2\r\n" + extra + "Authorization: Bearer " + testToken + "\r\n\r\n{}"
		conn := &framedConn{Conn: &memoryConn{Reader: strings.NewReader(wire)}}
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Header.Values("Content-Length")) != 1 {
			t.Fatal("test requires net/http duplicate normalization")
		}
		req = req.WithContext(ConnContext(context.Background(), conn))
		s, b := fixture()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if duplicate {
			expectStatus(t, w, 400)
			if len(b.calls) != 0 {
				t.Fatal("duplicate framing reached backend")
			}
		} else {
			expectStatus(t, w, 200)
		}
	}
}

func TestHeaderScanLimitCannotHideDuplicateFraming(t *testing.T) {
	// This fits net/http's read-ahead margin above MaxHeaderBytes, but puts the
	// duplicate fields beyond our bounded raw-header inspection.
	wire := "POST /api/pools/plan HTTP/1.1\r\nHost: localhost\r\nX-Padding: " + strings.Repeat("x", 65*1024) + "\r\nContent-Type: application/json\r\nContent-Length: 2\r\ncontent-length: 2\r\nAuthorization: Bearer " + testToken + "\r\n\r\n{}"
	conn := &framedConn{Conn: &memoryConn{Reader: strings.NewReader(wire)}}
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(ConnContext(context.Background(), conn))
	s, b := fixture()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	expectStatus(t, w, 431)
	if len(b.calls) != 0 {
		t.Fatal("oversized ambiguous framing reached backend")
	}
}

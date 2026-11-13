package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"strings"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
)

type framingKey struct{}

// StrictListener retains framing evidence which net/http otherwise discards:
// ReadRequest merges duplicate identical Content-Length fields. This console
// rejects all duplicate framing fields. Responses close the connection, so one
// request header is inspected per connection; no request bodies are buffered.
func StrictListener(listener net.Listener) net.Listener { return &strictListener{listener} }

type strictListener struct{ net.Listener }

func (listener *strictListener) Accept() (net.Conn, error) {
	conn, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &framedConn{Conn: conn}, nil
}

// ConnContext accompanies StrictListener on the http.Server. The original
// header is still parsed by net/http; this only retains its ambiguity flag.
func ConnContext(ctx context.Context, conn net.Conn) context.Context {
	if framed, ok := conn.(*framedConn); ok {
		return context.WithValue(ctx, framingKey{}, framed)
	}
	return ctx
}

type framedConn struct {
	net.Conn
	reader       io.Reader
	framingError error
}

func (conn *framedConn) Read(data []byte) (int, error) {
	if conn.reader == nil {
		reader := bufio.NewReader(conn.Conn)
		var header bytes.Buffer
		var currentLine bytes.Buffer
		lengths := 0
		// net/http still enforces wire syntax, header limits and read deadlines.
		for header.Len() <= 64*1024 {
			part, err := reader.ReadSlice('\n')
			header.Write(part)
			currentLine.Write(part)
			if header.Len() > 64*1024 {
				// net/http allows a small read-ahead margin above MaxHeaderBytes.
				// Do not let that margin bypass the raw duplicate-field check.
				conn.framingError = model.NewProblem(431, "请求头过大")
				break
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				break
			}
			text := strings.TrimRight(currentLine.String(), "\r\n")
			currentLine.Reset()
			if text == "" {
				break
			}
			name, _, found := strings.Cut(text, ":")
			if found && strings.EqualFold(name, "Content-Length") {
				lengths++
				if lengths > 1 {
					conn.framingError = model.NewProblem(400, "不接受重复的 Content-Length")
				}
			}
		}
		conn.reader = io.MultiReader(bytes.NewReader(header.Bytes()), reader)
	}
	return conn.reader.Read(data)
}

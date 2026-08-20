package mcpserve

import (
	"io"
	"net"
)

// Proxy connects a stdio MCP client to a session socket and copies
// bytes both ways until either side closes. It is protocol-blind on
// purpose: the proxy exists because agent harnesses spawn a command
// and speak MCP over its stdin/stdout, and any parsing here would be a
// second implementation of what the server already does.
func Proxy(socket string, in io.Reader, out io.Writer) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, conn)
		done <- err
	}()
	_, _ = io.Copy(conn, in)
	// stdin ended: half-close so the server sees EOF, then drain what
	// it still has to say — cutting the socket here would race the
	// response to the client's last request.
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	return <-done
}

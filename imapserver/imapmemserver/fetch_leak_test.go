package imapmemserver_test

import (
	"bytes"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// closeOnFetchConn closes its own end as soon as it has read a FETCH command,
// the way a test fault or a dying network does, so the response write fails.
type closeOnFetchConn struct {
	net.Conn
}

func (c closeOnFetchConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if strings.Contains(string(p[:n]), "FETCH") {
		_ = c.Conn.Close()
	}
	return n, err
}

type closeOnFetchListener struct{ net.Listener }

func (l closeOnFetchListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return closeOnFetchConn{c}, nil
}

// A write error while a FETCH response is being sent used to make the handler
// return without closing the FetchResponseWriter. The connection's encoder lock
// was never released, so the serve loop blocked forever writing the tagged
// status response and its goroutine leaked.
func TestFetchWriteErrorReleasesTheEncoderLock(t *testing.T) {
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("u", "p")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(closeOnFetchListener{ln}) }()
	t.Cleanup(func() { _ = srv.Close() })

	// Bigger than the connection's write buffer, so the body write itself fails
	// rather than only the final flush.
	body := append([]byte("Subject: hi\r\n\r\n"), bytes.Repeat([]byte("x"), 1<<20)...)
	c, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login("u", "p").Wait(); err != nil {
		t.Fatal(err)
	}
	appendCmd := c.Append("INBOX", int64(len(body)), nil)
	if _, err := appendCmd.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := appendCmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := appendCmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Fetch(imap.UIDSetNum(1), &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	}).Collect()

	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		if !strings.Contains(string(buf), "newResponseEncoder") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a serve goroutine is stuck waiting for the encoder lock after the write failed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

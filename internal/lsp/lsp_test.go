package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer answers a hardcoded scripted reply for each request.
type fakeServer struct {
	in     io.Reader
	out    *bytes.Buffer
	outMu  sync.Mutex
	notify chan string
}

func TestFramingAndCall(t *testing.T) {
	cReader, sWriter := io.Pipe()
	sReader, cWriter := io.Pipe()

	cli := NewClient(cReader, cWriter)
	go cli.Run(context.Background())

	// Server: read one frame, reply with id echo + result {"ok":true}, also emit a notification.
	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bytes.NewBuffer(nil)
		_ = br
		// We'll piggyback Client's reader logic by manually parsing one frame.
		s := newFrameReader(sReader)
		req, err := s.read()
		if err != nil {
			t.Errorf("server read: %v", err)
			return
		}
		var parsed struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(req, &parsed); err != nil {
			t.Errorf("server decode: %v", err)
			return
		}
		if parsed.Method != "ping" {
			t.Errorf("expected ping, got %q", parsed.Method)
		}
		// notification first
		writeFrame(sWriter, []byte(`{"jsonrpc":"2.0","method":"hello","params":{"x":1}}`))
		// then reply
		writeFrame(sWriter, fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%d,"result":{"ok":true}}`, parsed.ID))
	}()

	gotNotif := make(chan json.RawMessage, 1)
	cli.OnNotification("hello", func(p json.RawMessage) { gotNotif <- p })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := cli.Call(ctx, "ping", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(string(res), `"ok":true`) {
		t.Fatalf("unexpected result: %s", res)
	}
	select {
	case p := <-gotNotif:
		if !strings.Contains(string(p), `"x":1`) {
			t.Fatalf("bad notif params: %s", p)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not delivered")
	}
	<-done
}

// frameReader is a stripped-down helper that mirrors readMessage; we use
// it inline to drive the fake server side without exposing Client internals.
type frameReader struct{ r io.Reader }

func newFrameReader(r io.Reader) *frameReader { return &frameReader{r: r} }

func (f *frameReader) read() ([]byte, error) {
	var headerBuf bytes.Buffer
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(f.r, one); err != nil {
			return nil, err
		}
		headerBuf.WriteByte(one[0])
		if bytes.HasSuffix(headerBuf.Bytes(), []byte("\r\n\r\n")) {
			break
		}
	}
	hdr := headerBuf.String()
	var n int
	for line := range strings.SplitSeq(hdr, "\r\n") {
		if strings.HasPrefix(line, "Content-Length:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			fmt.Sscanf(v, "%d", &n)
		}
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(f.r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func writeFrame(w io.Writer, body []byte) {
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body))
	w.Write(body)
}

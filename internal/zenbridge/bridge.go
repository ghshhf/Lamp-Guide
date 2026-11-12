// Package zenbridge bridges the embedded zen-gate gateway to the desktop UI.
//
// The gateway's own admin surface is a loopback-only HTTP API guarded by an
// Origin check, which a WebView2 page served from a virtual https origin can
// never satisfy — and should not have to. Instead of weakening that guard, the
// UI calls the gateway in-process: AdminRequest builds an httptest-style request
// that looks exactly like a loopback GET/POST from 127.0.0.1 and feeds it to
// the gateway's real router. The dashboard therefore talks to the same handler
// code the HTTP surface does, with no second implementation to keep in sync.
package zenbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"zenui/internal/zen/gateway"
)

// Bridge owns the running gateway.
type Bridge struct {
	srv *gateway.Server
}

// New wraps a started gateway server.
func New(srv *gateway.Server) *Bridge { return &Bridge{srv: srv} }

// AdminRequest performs GET when body is nil, POST otherwise, against
// /admin/api/<path>, and returns the response the UI would have parsed.
func (b *Bridge) AdminRequest(path string, body any) (int, []byte, error) {
	if b == nil || b.srv == nil {
		return 0, nil, fmt.Errorf("网关未启动")
	}

	var rdr io.Reader
	method := http.MethodGet
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(raw)
		method = http.MethodPost
	}

	// Admin routes may reach the network (proxy-test, subs/refresh,
	// update/check). Without a cancelable context they hang the WebView
	// dispatcher goroutine forever after the JS side has given up.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req := httptest.NewRequest(method, "http://127.0.0.1/admin/api/"+strings.TrimPrefix(path, "/"), rdr).WithContext(ctx)
	// These are the two facts adminAllowed checks: a loopback Host and a
	// loopback RemoteAddr. Nothing else about the request is special-cased.
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:0"
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	// A mutating request must carry a same-origin Origin or none at all.
	req.Header.Set("origin", "http://127.0.0.1")

	rec := httptest.NewRecorder()
	b.srv.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, nil, err
	}
	return res.StatusCode, out, nil
}

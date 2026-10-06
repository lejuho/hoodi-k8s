package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeNode is a minimal JSON-RPC server that answers the three calls check() makes.
type fakeNode struct {
	syncing string // raw JSON for eth_syncing: "false" or an object
	peers   uint64
	head    uint64
	ageSec  int64 // how old the latest block is
	broken  bool  // answer every call with a JSON-RPC error
}

func (f fakeNode) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if f.broken {
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`)
			return
		}
		var result string
		switch req.Method {
		case "eth_syncing":
			result = f.syncing
		case "net_peerCount":
			result = fmt.Sprintf(`"0x%x"`, f.peers)
		case "eth_getBlockByNumber":
			ts := time.Now().Unix() - f.ageSec
			result = fmt.Sprintf(`{"number":"0x%x","timestamp":"0x%x"}`, f.head, ts)
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%s}`, result)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func healthy() fakeNode {
	return fakeNode{syncing: "false", peers: 25, head: 3762967, ageSec: 5}
}

// setup points the package-level config at the fake servers and restores it afterwards.
func setup(t *testing.T, node string, ref string) {
	t.Helper()
	oldRPC, oldRef, oldAge, oldPeers, oldBehind := rpcURL, refURL, maxAge, minPeers, maxBehind
	rpcURL, refURL, maxAge, minPeers, maxBehind = node, ref, 60, 3, 5
	t.Cleanup(func() {
		rpcURL, refURL, maxAge, minPeers, maxBehind = oldRPC, oldRef, oldAge, oldPeers, oldBehind
	})
}

func hasReason(s status, sub string) bool {
	for _, r := range s.Reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func TestCheck(t *testing.T) {
	refAt := func(head uint64) fakeNode { n := healthy(); n.head = head; return n }

	cases := []struct {
		name       string
		node       fakeNode
		ref        *fakeNode // nil = no reference configured
		refDown    bool      // reference configured but unreachable
		wantReady  bool
		wantReason string
		wantWarn   bool
	}{
		{name: "healthy node is ready", node: healthy(), wantReady: true},
		{name: "syncing node is not ready",
			node: func() fakeNode { n := healthy(); n.syncing = `{"currentBlock":"0x1","highestBlock":"0x10"}`; return n }(),
			wantReason: "syncing"},
		{name: "too few peers",
			node: func() fakeNode { n := healthy(); n.peers = 2; return n }(),
			wantReason: "peers 2 < 3"},
		{name: "stale head (CL stopped)",
			node: func() fakeNode { n := healthy(); n.ageSec = 144; return n }(),
			wantReason: "head age"},
		{name: "head age exactly at limit is not ready",
			node: func() fakeNode { n := healthy(); n.ageSec = 60; return n }(),
			wantReason: "head age"},
		{name: "within 5 blocks of reference is ready",
			node: healthy(), ref: func() *fakeNode { r := refAt(3762967 + 5); return &r }(), wantReady: true},
		{name: "behind reference by 10 blocks",
			node: healthy(), ref: func() *fakeNode { r := refAt(3762967 + 10); return &r }(),
			wantReason: "behind reference by 10"},
		{name: "reference down only warns (does not remove the node)",
			node: healthy(), refDown: true, wantReady: true, wantWarn: true},
		{name: "local RPC error is not ready",
			node: fakeNode{broken: true}, wantReason: "eth_syncing error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := tc.node.server(t)
			ref := ""
			switch {
			case tc.ref != nil:
				ref = tc.ref.server(t).URL
			case tc.refDown:
				down := httptest.NewServer(http.NotFoundHandler())
				ref = down.URL
				down.Close() // closed server: connection refused
			}
			setup(t, node.URL, ref)

			s := check(context.Background())

			if s.Ready != tc.wantReady {
				t.Fatalf("ready=%v, want %v (reasons=%v)", s.Ready, tc.wantReady, s.Reasons)
			}
			if tc.wantReason != "" && !hasReason(s, tc.wantReason) {
				t.Errorf("reasons=%v, want one containing %q", s.Reasons, tc.wantReason)
			}
			if tc.wantWarn != (len(s.Warnings) > 0) {
				t.Errorf("warnings=%v, wantWarn=%v", s.Warnings, tc.wantWarn)
			}
		})
	}
}

func TestReadyHandlerStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		node fakeNode
		code int
	}{
		{"200 when ready", healthy(), http.StatusOK},
		{"503 when not ready", func() fakeNode { n := healthy(); n.ageSec = 144; return n }(), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t, tc.node.server(t).URL, "")
			rec := httptest.NewRecorder()
			readyHandler(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
			if rec.Code != tc.code {
				t.Fatalf("code=%d, want %d", rec.Code, tc.code)
			}
			var s status
			if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if (tc.code == http.StatusOK) != s.Ready {
				t.Errorf("body ready=%v does not match status %d", s.Ready, rec.Code)
			}
		})
	}
}

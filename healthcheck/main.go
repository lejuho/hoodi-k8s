package main

import (
"bytes"
"context"
"encoding/json"
"fmt"
"log"
"net/http"
"os"
"strconv"
"sync"
"time"
)

type rpcReq struct {
JSONRPC string        `json:"jsonrpc"`
Method  string        `json:"method"`
Params  []interface{} `json:"params"`
ID      int           `json:"id"`
}

type rpcResp struct {
Result json.RawMessage `json:"result"`
Error  *struct {
Message string `json:"message"`
} `json:"error"`
}

type status struct {
Ready      bool     `json:"ready"`
Syncing    bool     `json:"syncing"`
Peers      uint64   `json:"peers"`
Head       uint64   `json:"head"`
HeadAgeSec int64    `json:"head_age_sec"`
RefHead    uint64   `json:"ref_head,omitempty"`
BehindRef  int64    `json:"behind_ref,omitempty"`
Reasons    []string `json:"reasons,omitempty"`
Warnings   []string `json:"warnings,omitempty"`
}

var (
client    = &http.Client{Timeout: 3 * time.Second}
rpcURL    = getenv("RPC_URL", "http://localhost:8545")
refURL    = os.Getenv("REF_URL")
maxAge    = int64(getint("MAX_HEAD_AGE_SEC", 60))
minPeers  = uint64(getint("MIN_PEERS", 3))
maxBehind = int64(getint("MAX_BEHIND_REF", 5))
mu        sync.Mutex
lastReady *bool
)

func getenv(k, d string) string {
if v := os.Getenv(k); v != "" {
return v
}
return d
}

func getint(k string, d int) int {
if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
return v
}
return d
}

func call(ctx context.Context, url, method string, params ...interface{}) (json.RawMessage, error) {
if params == nil {
params = []interface{}{}
}
body, _ := json.Marshal(rpcReq{"2.0", method, params, 1})
req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
if err != nil {
return nil, err
}
req.Header.Set("Content-Type", "application/json")
res, err := client.Do(req)
if err != nil {
return nil, err
}
defer res.Body.Close()
var r rpcResp
if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
return nil, err
}
if r.Error != nil {
return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
}
return r.Result, nil
}

func hexNum(raw json.RawMessage) (uint64, error) {
var s string
if err := json.Unmarshal(raw, &s); err != nil {
return 0, err
}
return strconv.ParseUint(s, 0, 64)
}

func latest(ctx context.Context, url string) (num, ts uint64, err error) {
raw, err := call(ctx, url, "eth_getBlockByNumber", "latest", false)
if err != nil {
return
}
var b struct {
Number    string `json:"number"`
Timestamp string `json:"timestamp"`
}
if err = json.Unmarshal(raw, &b); err != nil {
return
}
if num, err = strconv.ParseUint(b.Number, 0, 64); err != nil {
return
}
ts, err = strconv.ParseUint(b.Timestamp, 0, 64)
return
}

func check(ctx context.Context) status {
s := status{}
fail := func(f string, a ...interface{}) { s.Reasons = append(s.Reasons, fmt.Sprintf(f, a...)) }

if raw, err := call(ctx, rpcURL, "eth_syncing"); err != nil {
fail("eth_syncing error: %v", err)
} else if string(raw) != "false" {
s.Syncing = true
fail("node is syncing")
}

if raw, err := call(ctx, rpcURL, "net_peerCount"); err != nil {
fail("net_peerCount error: %v", err)
} else if n, err := hexNum(raw); err != nil {
fail("peer count parse error: %v", err)
} else {
s.Peers = n
if n < minPeers {
fail("peers %d < %d", n, minPeers)
}
}

if num, ts, err := latest(ctx, rpcURL); err != nil {
fail("latest block error: %v", err)
} else {
s.Head = num
s.HeadAgeSec = time.Now().Unix() - int64(ts)
if s.HeadAgeSec >= maxAge {
fail("head age %ds >= %ds", s.HeadAgeSec, maxAge)
}
if refURL != "" {
if ref, _, err := latest(ctx, refURL); err != nil {
s.Warnings = append(s.Warnings, fmt.Sprintf("reference unavailable (ignored): %v", err))
} else {
s.RefHead = ref
s.BehindRef = int64(ref) - int64(num)
if s.BehindRef > maxBehind {
fail("behind reference by %d blocks > %d", s.BehindRef, maxBehind)
}
}
}
}

s.Ready = len(s.Reasons) == 0
return s
}

func readyHandler(w http.ResponseWriter, r *http.Request) {
ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
defer cancel()
s := check(ctx)

mu.Lock()
if lastReady == nil || *lastReady != s.Ready {
log.Printf("ready=%v head=%d age=%ds peers=%d reasons=%v warnings=%v",
s.Ready, s.Head, s.HeadAgeSec, s.Peers, s.Reasons, s.Warnings)
v := s.Ready
lastReady = &v
}
mu.Unlock()

w.Header().Set("Content-Type", "application/json")
if !s.Ready {
w.WriteHeader(http.StatusServiceUnavailable)
}
json.NewEncoder(w).Encode(s)
}

func main() {
addr := getenv("LISTEN", ":8081")
http.HandleFunc("/ready", readyHandler)
http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
log.Printf("healthcheck listening on %s rpc=%s ref=%q maxAge=%ds minPeers=%d maxBehind=%d",
addr, rpcURL, refURL, maxAge, minPeers, maxBehind)
log.Fatal(http.ListenAndServe(addr, nil))
}

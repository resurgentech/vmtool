package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
)

const ndjsonType = "application/x-ndjson"

// gateMgr is a fakeMgr whose CloneImage and WaitForIP block until the test
// releases them, so a test can read the stream between stages.
type gateMgr struct {
	*fakeMgr
	mu      sync.Mutex
	reached chan string
	gates   map[string]chan struct{}
	noIP    bool
}

func newGateMgr(ip string) *gateMgr {
	return &gateMgr{
		fakeMgr: &fakeMgr{ip: ip},
		reached: make(chan string, 8),
		gates:   map[string]chan struct{}{"clone": make(chan struct{}), "wait_ip": make(chan struct{})},
	}
}

func (g *gateMgr) release(stage string) { close(g.gates[stage]) }

func (g *gateMgr) block(stage string) {
	g.reached <- stage
	select {
	case <-g.gates[stage]:
	case <-time.After(20 * time.Second):
	}
}

func (g *gateMgr) CloneImage(base, n, pool string, size uint64) (string, error) {
	g.block("clone")
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeMgr.CloneImage(base, n, pool, size)
}

func (g *gateMgr) WaitForIP(n string, d time.Duration) (string, error) {
	g.block("wait_ip")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.noIP {
		g.fakeMgr.rec("WaitForIP %s", n)
		return "", io.ErrUnexpectedEOF
	}
	return g.fakeMgr.WaitForIP(n, d)
}

func (g *gateMgr) callList() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.calls, ",")
}

// tickingSvc is a service whose clock moves 5 ms on every read, so the
// elapsed_ms stamps are distinct and ordered.
func tickingSvc() *app.Service {
	var n atomic.Int64
	t0 := time.Unix(1_700_000_000, 0)
	return &app.Service{Now: func() time.Time { return t0.Add(time.Duration(n.Add(1)*5) * time.Millisecond) }}
}

// lineReader delivers the response's lines on a channel as they arrive.
func lineReader(body io.Reader) <-chan string {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		br := bufio.NewReader(body)
		for {
			l, err := br.ReadString('\n')
			if l != "" {
				ch <- strings.TrimSuffix(l, "\n")
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func nextLine(t *testing.T, ch <-chan string) map[string]any {
	t.Helper()
	select {
	case l, ok := <-ch:
		if !ok {
			t.Fatal("stream ended before the expected line")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %q is not JSON: %v", l, err)
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no line arrived in 10s (the stream is not flushed per line?)")
	}
	return nil
}

func progressOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	if m["event"] != "progress" {
		t.Fatalf("want a progress line, got %v", m)
	}
	p, _ := m["progress"].(map[string]any)
	if p == nil {
		t.Fatalf("progress line without progress: %v", m)
	}
	return p
}

func postCreate(t *testing.T, ctx context.Context, url, accept, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/v1/hosts/box/vms", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

const createBody = `{"name":"web","image":"u.qcow2"}`

// Without the Accept value the answers are today's: success, a refusal and a
// failure after the domain exists. An Accept that does not name ndjson (or
// ranks it 0) changes nothing.
func TestCreateVMJSONUnchangedWithoutNDJSONAccept(t *testing.T) {
	cases := []struct {
		name string
		mgr  func() app.Manager
		body string
		code int
	}{
		{"success", func() app.Manager { return &fakeMgr{ip: "10.0.0.5"} }, createBody, 201},
		{"refusal", func() app.Manager { return &fakeMgr{} }, `{"name":"web","image":"u.qcow2","no_wait":true,"playbook":"x.yml"}`, 400},
		{"post-define failure", func() app.Manager { return noIPMgr{&fakeMgr{}} }, createBody, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want []byte
			for i, accept := range []string{"", "application/json", "application/json, text/plain;q=0.5", "*/*", ndjsonType + ";q=0"} {
				h := createServer(t, tc.mgr(), &app.Service{IPTimeout: time.Second})
				srv := httptest.NewServer(h)
				resp := postCreate(t, context.Background(), srv.URL, accept, tc.body)
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				srv.Close()
				if resp.StatusCode != tc.code || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
					t.Fatalf("Accept %q: %d %s %s", accept, resp.StatusCode, resp.Header.Get("Content-Type"), b)
				}
				if i == 0 {
					want = b
				} else if !bytes.Equal(b, want) {
					t.Errorf("Accept %q: body differs\n got %s\nwant %s", accept, b, want)
				}
			}
		})
	}
}

// A refusal before any work is the same JSON status for the stream.
func TestCreateVMStreamRefusalIsJSON(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		code             int
	}{
		{"no_wait with playbook", "/v1/hosts/box/vms", `{"name":"web","image":"u.qcow2","no_wait":true,"playbook":"x.yml"}`, 400},
		{"unknown host", "/v1/hosts/nope/vms", createBody, 404},
		{"bad json", "/v1/hosts/box/vms", `{`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := createServer(t, &fakeMgr{}, &app.Service{})
			srv := httptest.NewServer(h)
			defer srv.Close()
			var got [2][]byte
			var ct [2]string
			for i, accept := range []string{"", ndjsonType} {
				req, _ := http.NewRequest("POST", srv.URL+tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				if accept != "" {
					req.Header.Set("Accept", accept)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != tc.code || strings.HasPrefix(resp.Header.Get("Content-Type"), ndjsonType) {
					t.Fatalf("Accept %q: %d %s %s", accept, resp.StatusCode, resp.Header.Get("Content-Type"), b)
				}
				ct[i] = resp.Header.Get("Content-Type")
				got[i] = b
			}
			if ct[0] != ct[1] || !bytes.Equal(got[0], got[1]) {
				t.Errorf("body differs\n json %s\n ndjson %s", got[0], got[1])
			}
		})
	}
}

// Lines arrive one at a time while the workflow's stages are still blocked,
// in the order the workflow emits them, each stamped; the last line is one
// done with the VM and the timings.
func TestCreateVMStreamsLinesAsWritten(t *testing.T) {
	g := newGateMgr("10.9.8.7")
	srv := httptest.NewServer(createServer(t, g, tickingSvc()))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp := postCreate(t, ctx, srv.URL, ndjsonType+", application/json;q=0.1", createBody)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ndjsonType {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := lineReader(resp.Body)

	// The first line is there while CloneImage is still blocked.
	p := progressOf(t, nextLine(t, lines))
	if p["stage"] != "clone" || p["status"] != "start" {
		t.Fatalf("first line: %v", p)
	}
	var stamps []int64
	stamp := func(p map[string]any) {
		v, ok := p["elapsed_ms"].(float64)
		if !ok {
			t.Fatalf("no elapsed_ms: %v", p)
		}
		stamps = append(stamps, int64(v))
	}
	stamp(p)
	select {
	case s := <-g.reached:
		if s != "clone" {
			t.Fatalf("reached %s", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the clone stage was not reached")
	}
	g.release("clone")

	var seen []string
	var timer app.CreateTimer
	timer.Add(app.ProgressEvent{Stage: "clone", Status: "start", ElapsedMS: stamps[0]})
	for {
		m := nextLine(t, lines)
		p := progressOf(t, m)
		stamp(p)
		seen = append(seen, p["stage"].(string)+" "+p["status"].(string))
		timer.Add(app.ProgressEvent{Stage: p["stage"].(string), Status: p["status"].(string), ElapsedMS: stamps[len(stamps)-1]})
		if p["stage"] == "wait_ip" && p["status"] == "start" {
			break
		}
	}
	if seen[0] != "clone done" || seen[len(seen)-1] != "wait_ip start" {
		t.Errorf("order: %v", seen)
	}
	<-g.reached // wait_ip is blocked; nothing more arrives yet
	select {
	case l := <-lines:
		t.Fatalf("a line arrived while wait_ip was blocked: %s", l)
	case <-time.After(200 * time.Millisecond):
	}
	g.release("wait_ip")

	var done map[string]any
	for {
		m := nextLine(t, lines)
		if m["event"] == "progress" {
			p := progressOf(t, m)
			stamp(p)
			timer.Add(app.ProgressEvent{Stage: p["stage"].(string), Status: p["status"].(string), ElapsedMS: stamps[len(stamps)-1]})
			continue
		}
		done = m
		break
	}
	if done["event"] != "done" {
		t.Fatalf("last line: %v", done)
	}
	for i := 1; i < len(stamps); i++ {
		if stamps[i] < stamps[i-1] {
			t.Errorf("elapsed_ms goes backwards: %v", stamps)
		}
	}
	vm, _ := done["vm"].(map[string]any)
	if vm == nil || vm["name"] != "web" {
		t.Errorf("done without the VM: %v", done)
	}
	var want any
	wb, _ := json.Marshal(timer.Timings())
	_ = json.Unmarshal(wb, &want)
	if !reflect.DeepEqual(done["timings"], want) {
		t.Errorf("timings\n got %v\nwant %v", done["timings"], want)
	}
	if extra, ok := <-lines; ok {
		t.Errorf("a line after done: %s", extra)
	}
}

// A failure after the domain exists is one error line: message, kind, the VM,
// the stages so far. The status is still 200.
func TestCreateVMStreamErrorLine(t *testing.T) {
	g := newGateMgr("")
	g.noIP = true
	g.release("clone")
	g.release("wait_ip")
	srv := httptest.NewServer(createServer(t, g, tickingSvc()))
	defer srv.Close()
	resp := postCreate(t, context.Background(), srv.URL, ndjsonType, createBody)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	var last map[string]any
	n := 0
	for l := range lineReader(resp.Body) {
		n++
		last = nil
		if err := json.Unmarshal([]byte(l), &last); err != nil {
			t.Fatal(err)
		}
	}
	if n < 2 {
		t.Fatalf("only %d lines", n)
	}
	if last["event"] != "error" || last["kind"] != "internal" {
		t.Fatalf("last line: %v", last)
	}
	if msg, _ := last["error"].(string); !strings.Contains(msg, "no address") {
		t.Errorf("error = %v", last["error"])
	}
	if vm, _ := last["vm"].(map[string]any); vm == nil || vm["name"] != "web" {
		t.Errorf("no vm: %v", last)
	}
	tm, _ := last["timings"].(map[string]any)
	stages, _ := tm["stages"].([]any)
	if len(stages) == 0 || stages[0].(map[string]any)["stage"] != "clone" || tm["total_ms"] == nil {
		t.Errorf("timings so far: %v", last["timings"])
	}
}

// A hypervisor answers 403 as today, with or without the Accept value.
func TestCreateVMStreamHypervisorIs403(t *testing.T) {
	fm := &fakeMgr{}
	h := primServer(t, fm, config.RoleHypervisor)
	for _, accept := range []string{"", ndjsonType} {
		req := httptest.NewRequest("POST", "/v1/hosts/box/vms", strings.NewReader(createBody))
		req.Header.Set("Content-Type", "application/json")
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 || strings.HasPrefix(rec.Header().Get("Content-Type"), ndjsonType) {
			t.Errorf("Accept %q: %d %s", accept, rec.Code, rec.Body)
		}
	}
	if len(fm.calls) != 0 {
		t.Errorf("calls: %v", fm.calls)
	}
}

// A request cancelled mid-stream ends the workflow as the JSON path's does:
// it runs the same calls and the handler returns.
func TestCreateVMStreamCancelEndsLikeJSON(t *testing.T) {
	// baseline: the JSON path with a cancelled request context
	jg := newGateMgr("10.9.8.7")
	jg.release("clone")
	jg.release("wait_ip")
	jh := createServer(t, jg, tickingSvc())
	jctx, jcancel := context.WithCancel(context.Background())
	jcancel()
	jreq := httptest.NewRequest("POST", "/v1/hosts/box/vms", strings.NewReader(createBody)).WithContext(jctx)
	jreq.Header.Set("Content-Type", "application/json")
	jh.ServeHTTP(httptest.NewRecorder(), jreq)
	want := jg.callList()

	g := newGateMgr("10.9.8.7")
	h := createServer(t, g, tickingSvc())
	finished := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	resp := postCreate(t, ctx, srv.URL, ndjsonType, createBody)
	lines := lineReader(resp.Body)
	progressOf(t, nextLine(t, lines)) // clone start
	<-g.reached
	cancel() // the client goes away while the clone is blocked
	resp.Body.Close()
	g.release("clone")
	select {
	case <-g.reached: // wait_ip: the workflow went on, as the JSON path's does
	case <-time.After(10 * time.Second):
		t.Fatal("the workflow did not continue to wait_ip")
	}
	g.release("wait_ip")
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("the handler did not return after the cancel")
	}
	if got := g.callList(); got != want {
		t.Errorf("calls\n got %s\nwant %s (the JSON path)", got, want)
	}
}

// No event line (and so no stage detail, address or output) reaches the log.
func TestCreateVMStreamLogsNoEventLine(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	defer captureLogs(lockedWriter{&mu, &logs})()
	g := newGateMgr("10.9.8.7")
	g.release("clone")
	g.release("wait_ip")
	srv := httptest.NewServer(createServer(t, g, tickingSvc()))
	resp := postCreate(t, context.Background(), srv.URL, ndjsonType, createBody)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	srv.Close()
	if !strings.Contains(string(b), `"event":"done"`) || !strings.Contains(string(b), "10.9.8.7") {
		t.Fatalf("stream: %s", b)
	}
	mu.Lock()
	out := logs.String()
	mu.Unlock()
	for _, bad := range []string{"10.9.8.7", "elapsed_ms", `"event"`, "waiting for SSH", "cloning image", "u.qcow2"} {
		if strings.Contains(out, bad) {
			t.Errorf("log contains %q:\n%s", bad, out)
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// The stage names of an image transfer (fetch, relay, upload; API 2.8.0) go
// out on the stream as the workflow named them, and the timings keep each
// under its own name.
func TestCreateStreamCarriesTransferStageNames(t *testing.T) {
	rec := httptest.NewRecorder()
	cs := &createStream{st: newNDJSON(rec)}
	evs := []app.ProgressEvent{
		{Stage: "fetch", Status: "start", ElapsedMS: 10},
		{Stage: "fetch", Status: "progress", Sent: 5, Total: 10, ElapsedMS: 20},
		{Stage: "relay", Status: "start", ElapsedMS: 30},
		{Stage: "relay", Status: "done", ElapsedMS: 50},
		{Stage: "upload", Status: "start", ElapsedMS: 60},
		{Stage: "upload", Status: "done", ElapsedMS: 90},
	}
	for _, ev := range evs {
		cs.progress(ev)
	}
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		got = append(got, progressOf(t, m)["stage"].(string))
	}
	if want := []string{"fetch", "fetch", "relay", "relay", "upload", "upload"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stages %v, want %v", got, want)
	}
	tm := cs.timings()
	var names []string
	for _, s := range tm.Stages {
		names = append(names, s.Stage)
	}
	if !reflect.DeepEqual(names, []string{"fetch", "relay", "upload"}) {
		t.Fatalf("timing stages %v", names)
	}
}

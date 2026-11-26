package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/spec"
)

// Test tokens only; never a real token.
const (
	testToken  = "test-token-7f3a9c"
	wrongToken = "wrong-token-b21e44"
)

var publicOps = map[string]bool{"getDocs": true, "getOpenAPISpec": true, "getOpenAPIJSON": true, "getVersion": true, "getKeyRequestDiscovery": true}

type specOp struct {
	method, path, id string
	public           bool
	op               *openapi3.Operation
}

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	return doc
}

// specOps lists every operation with its effective security (operation-level
// overrides global).
func specOps(t *testing.T) []specOp {
	doc := loadSpec(t)
	var out []specOp
	for p, item := range doc.Paths.Map() {
		for m, op := range item.Operations() {
			sec := doc.Security
			if op.Security != nil {
				sec = *op.Security
			}
			out = append(out, specOp{method: m, path: p, id: op.OperationID, public: len(sec) == 0, op: op})
		}
	}
	return out
}

var paramRE = regexp.MustCompile(`\{[^}]+\}`)

func concrete(p string) string { return paramRE.ReplaceAllString(p, "x") }

func authedServer() *Server {
	return &Server{Config: &config.Config{TokenSHA256: config.Hash(testToken)}, PlaybookDir: "playbooks"}
}

func TestSpecSecurity(t *testing.T) {
	doc := loadSpec(t)
	ss := doc.Components.SecuritySchemes["bearerAuth"]
	if ss == nil || ss.Value == nil || ss.Value.Type != "http" || !strings.EqualFold(ss.Value.Scheme, "bearer") {
		t.Fatalf("components.securitySchemes.bearerAuth missing or not http bearer: %+v", ss)
	}
	if len(doc.Security) != 1 || doc.Security[0]["bearerAuth"] == nil {
		t.Fatalf("global security must be [bearerAuth], got %v", doc.Security)
	}
	if strings.Contains(strings.ToLower(doc.Info.Description), "no auth") {
		t.Error("info.description still says no auth")
	}
	got := map[string]bool{}
	for _, o := range specOps(t) {
		if o.public {
			got[o.id] = true
			if o.op.Security == nil {
				t.Errorf("%s public without explicit security: []", o.id)
			}
			continue
		}
		if o.op.Responses.Value("401") == nil {
			t.Errorf("%s %s (%s) has no 401 response", o.method, o.path, o.id)
		}
	}
	if len(got) != len(publicOps) {
		t.Fatalf("public ops = %v, want %v", got, publicOps)
	}
	for id := range publicOps {
		if !got[id] {
			t.Fatalf("public ops = %v, want %v", got, publicOps)
		}
	}
}

func check401(t *testing.T, rec *httptest.ResponseRecorder, label string, presented ...string) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("%s: status %d, want 401", label, rec.Code)
		return
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("%s: WWW-Authenticate %q", label, got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("%s: content-type %q", label, ct)
	}
	body := rec.Body.String()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error == "" {
		t.Errorf("%s: body %q is not the Error shape (%v)", label, body, err)
	}
	for _, p := range presented {
		if p != "" && strings.Contains(body, p) {
			t.Errorf("%s: 401 body echoes the presented token", label)
		}
	}
}

func TestAuthEveryOperation(t *testing.T) {
	h := authedServer().Handler()
	ops := specOps(t)
	if len(ops) < 10 {
		t.Fatalf("only %d ops", len(ops))
	}
	for _, o := range ops {
		p := concrete(o.path)
		label := o.method + " " + p + " (" + o.id + ")"

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(o.method, p, nil))
		if o.public {
			want := http.StatusOK
			if o.id == "getKeyRequestDiscovery" {
				want = http.StatusNotFound // authedServer sets no personal_keys.request_url
			}
			if rec.Code != want {
				t.Errorf("%s without token: %d, want %d", label, rec.Code, want)
			}
			continue
		}
		check401(t, rec, label+" no token")

		rec = httptest.NewRecorder()
		r := httptest.NewRequest(o.method, p, nil)
		r.Header.Set("Authorization", "Bearer "+wrongToken)
		h.ServeHTTP(rec, r)
		check401(t, rec, label+" wrong token", wrongToken)
	}
}

func TestAuthHeaderParsing(t *testing.T) {
	t.Chdir(t.TempDir())
	h := authedServer().Handler()
	for _, tc := range []struct {
		name    string
		headers []string
		want    int
	}{
		{"missing", nil, 401},
		{"empty", []string{""}, 401},
		{"scheme only", []string{"Bearer"}, 401},
		{"scheme and space", []string{"Bearer "}, 401},
		{"basic", []string{"Basic " + testToken}, 401},
		{"other scheme", []string{"Token " + testToken}, 401},
		{"bare token", []string{testToken}, 401},
		{"double space", []string{"Bearer  " + testToken}, 401},
		{"trailing junk", []string{"Bearer " + testToken + " x"}, 401},
		{"prefix of token", []string{"Bearer " + testToken[:5]}, 401},
		{"wrong", []string{"Bearer " + wrongToken}, 401},
		{"two headers", []string{"Bearer " + testToken, "Bearer " + testToken}, 401},
		{"ok", []string{"Bearer " + testToken}, 200},
		{"ok lower scheme", []string{"bearer " + testToken}, 200},
		{"ok upper scheme", []string{"BEARER " + testToken}, 200},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/metal", nil)
		for _, v := range tc.headers {
			r.Header.Add("Authorization", v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if tc.want == 401 {
			check401(t, rec, tc.name, testToken, wrongToken)
		} else if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestAuthUnknownPath(t *testing.T) {
	h := authedServer().Handler()
	for _, p := range []string{"/nope", "/v1/hosts/x/vms/x/nope", "//openapi.yaml", "/x/../v1/hosts/x/vms"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		check401(t, rec, "GET "+p)
	}
	r := httptest.NewRequest(http.MethodGet, "/nope", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("authed /nope: %d", rec.Code)
	}
}

func TestAuthBeforeBody(t *testing.T) {
	h := authedServer().Handler()
	big := bytes.Repeat([]byte("a"), 2<<20)
	r := httptest.NewRequest(http.MethodPost, "/v1/hosts/x/vms", bytes.NewReader(big))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	check401(t, rec, "POST /vms oversized body")
}

// A Config with no token_sha256 must not authenticate anything.
func TestAuthEmptyHashRejects(t *testing.T) {
	h := (&Server{Config: &config.Config{}}).Handler()
	r := httptest.NewRequest(http.MethodGet, "/v1/metal", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	check401(t, rec, "empty hash", testToken)
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestAuthTokenNeverLogged(t *testing.T) {
	t.Chdir(t.TempDir())
	buf := captureSlog(t)
	h := authedServer().Handler()
	for _, tok := range []string{testToken, wrongToken} {
		r := httptest.NewRequest(http.MethodGet, "/v1/metal", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	out := buf.String()
	if !strings.Contains(out, "status=200") || !strings.Contains(out, "status=401") {
		t.Fatalf("request log missing 200/401 entries:\n%s", out)
	}
	for _, s := range []string{testToken, wrongToken, "Authorization", "Bearer"} {
		if strings.Contains(out, s) {
			t.Errorf("log contains %q:\n%s", s, out)
		}
	}
	// With a pulled key list: no key, hash or pull token either, in the
	// daemon's lines or the key list's own.
	t.Run("key list", checkPulledKeysNeverLogged)
}

// writeCert writes a self-signed ECDSA cert for 127.0.0.1/localhost.
func writeCert(t *testing.T, dir string) (certPath, keyPath string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "tls", "test.crt"), filepath.Join(dir, "tls", "test.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certPath, keyPath, pool
}

func TestHTTPServerRules(t *testing.T) {
	dir := t.TempDir()
	writeCert(t, dir)
	hash := config.Hash(testToken)
	tlsCfg := func(listen string) *config.Config {
		return &config.Config{Dir: dir, Listen: listen, TokenSHA256: hash, TLSCert: "tls/test.crt", TLSKey: "tls/test.key"}
	}
	for _, tc := range []struct {
		name    string
		s       *Server
		wantErr string
		wantTLS bool
		addr    string
	}{
		{name: "no config loopback", s: &Server{Listen: "127.0.0.1:9473"}, wantErr: "config.yml"},
		{name: "no config default", s: &Server{}, wantErr: "config.yml"},
		{name: "no config ::1", s: &Server{Listen: "[::1]:9473"}, wantErr: "config.yml"},
		{name: "no config 0.0.0.0", s: &Server{Listen: "0.0.0.0:9473"}, wantErr: "config.yml"},
		{name: "empty config", s: &Server{Config: &config.Config{Dir: dir}}, wantErr: "token_sha256"},
		{name: "hash only loopback", s: &Server{Config: &config.Config{Dir: dir, Listen: "127.0.0.1:9473", TokenSHA256: hash}}, wantErr: "tls"},
		{name: "hash only localhost", s: &Server{Config: &config.Config{Dir: dir, Listen: "localhost:9473", TokenSHA256: hash}}, wantErr: "tls"},
		{name: "hash only off loopback", s: &Server{Config: &config.Config{Dir: dir, Listen: "0.0.0.0:9473", TokenSHA256: hash}}, wantErr: "tls"},
		{name: "tls no hash loopback", s: &Server{Config: &config.Config{Dir: dir, Listen: "127.0.0.1:9473", TLSCert: "tls/test.crt", TLSKey: "tls/test.key"}}, wantErr: "token_sha256"},
		{name: "tls no hash off loopback", s: &Server{Config: &config.Config{Dir: dir, Listen: "0.0.0.0:9473", TLSCert: "tls/test.crt", TLSKey: "tls/test.key"}}, wantErr: "token_sha256"},
		{name: "cert without key", s: &Server{Config: &config.Config{Dir: dir, Listen: "127.0.0.1:9473", TokenSHA256: hash, TLSCert: "tls/test.crt"}}, wantErr: "tls_key"},
		{name: "key without cert", s: &Server{Config: &config.Config{Dir: dir, Listen: "127.0.0.1:9473", TokenSHA256: hash, TLSKey: "tls/test.key"}}, wantErr: "tls_cert"},
		{name: "missing cert file", s: &Server{Config: &config.Config{Dir: dir, Listen: "127.0.0.1:9473", TokenSHA256: hash, TLSCert: "tls/none.crt", TLSKey: "tls/test.key"}}, wantErr: "tls"},
		{name: "tls off loopback", s: &Server{Config: tlsCfg("0.0.0.0:9473")}, wantTLS: true, addr: "0.0.0.0:9473"},
		{name: "tls loopback", s: &Server{Config: tlsCfg("127.0.0.1:9473")}, wantTLS: true, addr: "127.0.0.1:9473"},
		{name: "flag overrides config", s: &Server{Listen: "127.0.0.1:1234", Config: tlsCfg("0.0.0.0:9473")}, wantTLS: true, addr: "127.0.0.1:1234"},
		{name: "flag off loopback refused", s: &Server{Listen: "0.0.0.0:1234", Config: &config.Config{Dir: dir, Listen: "127.0.0.1:9473", TokenSHA256: hash}}, wantErr: "tls"},
		{name: "config empty listen", s: &Server{Config: tlsCfg("")}, wantTLS: true, addr: "127.0.0.1:9473"},
	} {
		srv, err := tc.s.httpServer()
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.wantErr) {
				t.Errorf("%s: err %v, want containing %q", tc.name, err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), hash) {
				t.Errorf("%s: error contains the hash", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if srv.Addr != tc.addr {
			t.Errorf("%s: addr %q, want %q", tc.name, srv.Addr, tc.addr)
		}
		if gotTLS := srv.TLSConfig != nil; gotTLS != tc.wantTLS {
			t.Errorf("%s: tls %v, want %v", tc.name, gotTLS, tc.wantTLS)
		} else if gotTLS {
			if srv.TLSConfig.MinVersion != tls.VersionTLS12 || len(srv.TLSConfig.Certificates) != 1 {
				t.Errorf("%s: tls config %+v", tc.name, srv.TLSConfig)
			}
		}
	}
}

// With no Config, the handler authenticates nothing (there is no unauthenticated mode).
func TestAuthNilConfigRejects(t *testing.T) {
	h := (&Server{}).Handler()
	r := httptest.NewRequest(http.MethodGet, "/v1/metal", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	check401(t, rec, "nil config", testToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("public route with nil config: %d", rec.Code)
	}
}

func TestListenAndServeRefusesWithoutTLS(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []*Server{
		{Listen: "127.0.0.1:0"},
		{Listen: "127.0.0.1:0", Config: &config.Config{Dir: dir, TokenSHA256: config.Hash(testToken)}},
	} {
		done := make(chan error, 1)
		go func() { done <- s.ListenAndServe() }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "tls") {
				t.Fatalf("config %+v: err %v, want a TLS refusal", s.Config, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("ListenAndServe served without TLS")
		}
	}
}

func TestServeTLS(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	_, _, pool := writeCert(t, dir)
	s := &Server{Listen: "127.0.0.1:0", PlaybookDir: "playbooks", Config: &config.Config{
		Dir: dir, TokenSHA256: config.Hash(testToken), TLSCert: "tls/test.crt", TLSKey: "tls/test.key",
	}}
	srv, err := s.httpServer()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go serve(srv, ln)
	t.Cleanup(func() { srv.Close() })
	base := "https://" + ln.Addr().String()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	do := func(path, tok string) int {
		t.Helper()
		r, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	if c := do("/openapi.yaml", ""); c != 200 {
		t.Errorf("docs without token: %d", c)
	}
	if c := do("/v1/metal", ""); c != 401 {
		t.Errorf("/v1/metal without token: %d", c)
	}
	if c := do("/v1/metal", wrongToken); c != 401 {
		t.Errorf("/v1/metal wrong token: %d", c)
	}
	if c := do("/v1/metal", testToken); c != 200 {
		t.Errorf("/v1/metal with token: %d", c)
	}

	// Plain HTTP to the TLS port does not get an API response.
	if res, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + ln.Addr().String() + "/openapi.yaml"); err == nil {
		res.Body.Close()
		if res.StatusCode == 200 {
			t.Error("plain HTTP served by the TLS listener")
		}
	}

	// TLS 1.1 is refused.
	old := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11,
	}}}
	if res, err := old.Get(base + "/openapi.yaml"); err == nil {
		res.Body.Close()
		t.Error("TLS 1.1 handshake succeeded")
	}
}

// GET /version answers 200 with no Authorization header on both roles; every
// other operation outside the docs routes still answers 401 without a token.
func TestVersionPublicOnBothRoles(t *testing.T) {
	for _, role := range []string{config.RoleWorkstation, config.RoleHypervisor} {
		s := authedServer()
		s.Config.Role = role
		h := s.Handler()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/version", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("role %s: GET /version without token: %d, want 200", role, rec.Code)
		}
		for _, o := range specOps(t) {
			if publicOps[o.id] {
				continue
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(o.method, concrete(o.path), nil))
			check401(t, rec, "role "+role+" "+o.method+" "+o.path)
		}
	}
}

package hosts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/remote"
)

type probeMgr struct {
	app.Manager
	st app.HostStatus
}

func (p probeMgr) Probe(context.Context) app.HostStatus { return p.st }

func writeCert(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
		NotBefore: notAfter.Add(-time.Hour * 24 * 365), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStatus(t *testing.T) {
	notAfter := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	cert := writeCert(t, notAfter)
	r := NewWith("ws", config.RoleWorkstation, []string{"hv2", "hv1"}, okLocal).
		WithRemote(func(host, token string) (app.Manager, func(), error) {
			if token != "tok" {
				t.Errorf("host %s got token %q", host, token)
			}
			if host == "hv2" {
				return nil, nil, errors.New("boom")
			}
			return probeMgr{st: app.HostStatus{Address: "10.0.0.1:9473", Reachable: true, APIVersion: "1.0.0"}}, func() {}, nil
		}).WithLocalCert(cert).WithToken("tok")
	got := r.Status(context.Background(), time.Second)
	if len(got) != 3 || got[0].Host != "ws" || got[1].Host != "hv1" || got[2].Host != "hv2" {
		t.Fatalf("status %+v", got)
	}
	ours, _ := remote.OurVersion()
	if l := got[0]; !l.Local || !l.Reachable || l.Address != "local" || l.APIVersion != ours ||
		l.Role != config.RoleWorkstation || !l.CertNotAfter.Equal(notAfter) || l.Err != nil {
		t.Errorf("local row %+v", l)
	}
	if h := got[1]; h.Local || h.Role != config.RoleHypervisor || !h.Reachable || h.Address != "10.0.0.1:9473" {
		t.Errorf("hv1 row %+v", h)
	}
	if h := got[2]; h.Reachable || h.Err == nil || h.Err.Error() != "boom" {
		t.Errorf("hv2 row %+v", h)
	}
}

func TestStatusNoCertAndBadCert(t *testing.T) {
	r := NewWith("ws", config.RoleWorkstation, nil, okLocal)
	got := r.Status(context.Background(), time.Second)
	if len(got) != 1 || !got[0].CertNotAfter.IsZero() || got[0].Err != nil {
		t.Fatalf("no cert: %+v", got)
	}
	bad := filepath.Join(t.TempDir(), "x.crt")
	if err := os.WriteFile(bad, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = r.WithLocalCert(bad).Status(context.Background(), time.Second)
	if got[0].Err == nil || !got[0].Reachable {
		t.Fatalf("bad cert: %+v", got[0])
	}
}

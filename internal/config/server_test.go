package config

import (
	"errors"
	"strings"
	"testing"
)

func TestServerConfig(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "name: ws\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.URL != "" || c.StoreConfigured() {
		t.Fatalf("unconfigured: %+v", c.Server)
	}

	writeFile(t, dir, "config.yml", "server:\n  url: https://PLACEHOLDER-HOST:8095/\n")
	c, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.URL != "https://PLACEHOLDER-HOST:8095/" || !c.StoreConfigured() {
		t.Fatalf("configured: %+v", c.Server)
	}
}

func TestServerURLValidation(t *testing.T) {
	setHost(t, "jetfire")
	for name, url := range map[string]string{
		"http":     "http://PLACEHOLDER-HOST:8095",
		"no host":  "https://",
		"userinfo": "https://user:PLACEHOLDER-not-a-secret-1@PLACEHOLDER-HOST",
		"query":    "https://PLACEHOLDER-HOST/?token=PLACEHOLDER-not-a-secret-1",
		"fragment": "https://PLACEHOLDER-HOST/#x",
		"bare":     "PLACEHOLDER-HOST:8095",
	} {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", "server:\n  url: \""+url+"\"\n")
		_, err := Load(dir)
		if err == nil {
			t.Errorf("%s: accepted %q", name, url)
			continue
		}
		if !strings.Contains(err.Error(), "server.url") {
			t.Errorf("%s: error does not name server.url: %v", name, err)
		}
		if strings.Contains(err.Error(), "PLACEHOLDER-not-a-secret-1") {
			t.Errorf("%s: error echoes the URL's secret: %v", name, err)
		}
	}
}

func TestServerTokenSources(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ServerTokenEnv, "")
	t.Setenv(TokenEnv, "PLACEHOLDER-host-token")
	if _, err := ServerToken(dir); !errors.Is(err, ErrNoServerToken) {
		t.Fatalf("absent: %v", err)
	}
	// The host token is not the store token.
	writeFile(t, dir, ".env", "VMTOOL_TOKEN=PLACEHOLDER-host-token\n")
	if _, err := ServerToken(dir); !errors.Is(err, ErrNoServerToken) {
		t.Fatalf("host token in .env: %v", err)
	}
	writeFile(t, dir, ".env", "VMTOOL_TOKEN=PLACEHOLDER-host-token\nVMTOOL_SERVER_TOKEN=\"PLACEHOLDER-not-a-secret-1\"\n")
	tok, err := ServerToken(dir)
	if err != nil || tok != "PLACEHOLDER-not-a-secret-1" {
		t.Fatalf(".env: %q %v", tok, err)
	}
	t.Setenv(ServerTokenEnv, "PLACEHOLDER-env-token")
	tok, err = ServerToken(dir)
	if err != nil || tok != "PLACEHOLDER-env-token" {
		t.Fatalf("env wins: %q %v", tok, err)
	}
	// The host token loader still ignores the store token.
	t.Setenv(TokenEnv, "")
	writeFile(t, dir, ".env", "VMTOOL_SERVER_TOKEN=PLACEHOLDER-not-a-secret-1\n")
	if _, err := Token(dir); !errors.Is(err, ErrNoToken) {
		t.Fatalf("Token read the store token: %v", err)
	}
}

// The store token never comes from config.yml: a token key there is ignored.
func TestServerTokenNotFromConfigYML(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	t.Setenv(ServerTokenEnv, "")
	writeFile(t, dir, "config.yml", "server:\n  url: https://PLACEHOLDER-HOST\n  token: PLACEHOLDER-not-a-secret-1\n")
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if tok, err := ServerToken(dir); err == nil {
		t.Fatalf("token %q came from somewhere", tok)
	}
}

func TestServerTokenNeverInErrors(t *testing.T) {
	const secret = "PLACEHOLDER-not-a-secret-1"
	dir := t.TempDir()
	t.Setenv(ServerTokenEnv, "")
	writeFile(t, dir, ".env", "OTHER="+secret+"\n")
	_, err := ServerToken(dir)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err %v", err)
	}
}

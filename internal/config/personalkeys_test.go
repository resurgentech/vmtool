package config

import (
	"strings"
	"testing"
)

// personal_keys.request_url is off unless set, and a bad one is a config
// error at load.
func TestPersonalKeysRequestURLLoad(t *testing.T) {
	setHost(t, "home1")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "name: hv1\nrole: hypervisor\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.PersonalKeys.RequestURL != "" {
		t.Fatalf("unset: %q", c.PersonalKeys.RequestURL)
	}
	for _, ok := range []string{
		"https://keys.example.lab/v1/services/example/requests",
		"https://keys.example.lab:8443/requests?x=1",
		"http://localhost:8080/requests",
		"http://127.0.0.1/requests",
		"http://127.1.2.3:9/requests",
		"http://[::1]:9/requests",
	} {
		writeFile(t, dir, "config.yml", "personal_keys:\n  request_url: \""+ok+"\"\n")
		if c, err = Load(dir); err != nil || c.PersonalKeys.RequestURL != ok {
			t.Errorf("%q: %q %v", ok, c.PersonalKeys.RequestURL, err)
		}
	}
}

func TestPersonalKeysRequestURLValidation(t *testing.T) {
	setHost(t, "home1")
	for name, url := range map[string]string{
		"http remote":    "http://keys.example.lab/requests",
		"http not-quite": "http://localhost.example.lab/requests",
		"http 10.x":      "http://10.0.0.1/requests",
		"no host":        "https:///requests",
		"relative":       "/v1/requests",
		"bare":           "keys.example.lab/requests",
		"userinfo":       "https://user:PLACEHOLDER-not-a-secret-1@keys.example.lab/requests",
		"userinfo no pw": "https://PLACEHOLDER-not-a-secret-1@keys.example.lab/requests",
		"fragment":       "https://keys.example.lab/requests#x",
		"empty fragment": "https://keys.example.lab/requests#",
		"other scheme":   "ftp://keys.example.lab/requests",
		"opaque":         "https:keys.example.lab",
		"whitespace":     " https://keys.example.lab/requests",
	} {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", "personal_keys:\n  request_url: \""+url+"\"\n")
		_, err := Load(dir)
		if err == nil {
			t.Errorf("%s: accepted %q", name, url)
			continue
		}
		if !strings.Contains(err.Error(), "personal_keys.request_url") {
			t.Errorf("%s: error does not name the key: %v", name, err)
		}
		if strings.Contains(err.Error(), "PLACEHOLDER-not-a-secret-1") {
			t.Errorf("%s: error echoes the URL's user info: %v", name, err)
		}
	}
}

// config write keeps the block's other keys and validates before replacing.
func TestPersonalKeysWrite(t *testing.T) {
	setHost(t, "home1")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "personal_keys:\n  future_key: keep\n")
	if err := Write(dir, "PLACEHOLDER-not-a-secret-1", WriteOptions{KeyRequestURL: "https://keys.example.lab/r"}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil || c.PersonalKeys.RequestURL != "https://keys.example.lab/r" {
		t.Fatalf("%q %v", c.PersonalKeys.RequestURL, err)
	}
	if err := Write(dir, "PLACEHOLDER-not-a-secret-1", WriteOptions{KeyRequestURL: "http://keys.example.lab/r"}); err == nil {
		t.Error("http to a non-loopback host accepted")
	}
	if c, err = Load(dir); err != nil || c.PersonalKeys.RequestURL != "https://keys.example.lab/r" {
		t.Errorf("a refused write changed the file: %q %v", c.PersonalKeys.RequestURL, err)
	}
}

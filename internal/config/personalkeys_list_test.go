package config

import (
	"strings"
	"testing"
	"time"
)

const listBlock = "personal_keys:\n  url: https://keys.example.lab/keys/v1/lists/vmtool\n  cache_file: keys.cache\n"

// The pulled key list is off unless personal_keys.url is set, and nothing has
// a default URL.
func TestPersonalKeysListOffByDefault(t *testing.T) {
	setHost(t, "home1")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "name: hv1\nrole: hypervisor\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := c.PersonalKeys; p.URL != "" || p.TokenFile != "" || p.CacheFile != "" || p.AllowInsecureHTTP {
		t.Fatalf("unset block is not off: %+v", p)
	}
	writeFile(t, dir, "config.yml", "personal_keys:\n  request_url: https://keys.example.lab/v1/services/example/requests\n")
	if c, err = Load(dir); err != nil || c.PersonalKeys.URL != "" {
		t.Fatalf("request_url alone turned the list on: %+v %v", c.PersonalKeys, err)
	}
}

func TestPersonalKeysListDefaultsAndValues(t *testing.T) {
	setHost(t, "home1")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", listBlock)
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := c.PersonalKeys
	if p.URL != "https://keys.example.lab/keys/v1/lists/vmtool" || p.CacheFile != "keys.cache" {
		t.Errorf("values: %+v", p)
	}
	if p.Refresh != time.Minute || p.RefreshOnMiss != 5*time.Second {
		t.Errorf("defaults: refresh %v, refresh_on_miss %v", p.Refresh, p.RefreshOnMiss)
	}

	writeFile(t, dir, "config.yml", listBlock+"  token_file: pull.token\n  refresh: 30s\n  refresh_on_miss: 0s\n")
	if c, err = Load(dir); err != nil {
		t.Fatal(err)
	}
	p = c.PersonalKeys
	if p.Refresh != 30*time.Second || p.RefreshOnMiss != 0 || p.TokenFile != "pull.token" {
		t.Errorf("explicit: %+v", p)
	}
	// Relative paths resolve against the config directory, like tls_cert.
	if got := c.Path(p.CacheFile); got != dir+"/keys.cache" {
		t.Errorf("cache path %q", got)
	}
}

func TestPersonalKeysListValidation(t *testing.T) {
	setHost(t, "home1")
	const secret = "PLACEHOLDER-not-a-secret-2"
	for name, tc := range map[string]struct{ yaml, want string }{
		"no cache file":      {"personal_keys:\n  url: https://lists.invalid.test/l\n", "personal_keys.cache_file"},
		"refresh zero":       {listBlock + "  refresh: 0s\n", "personal_keys.refresh"},
		"refresh negative":   {listBlock + "  refresh: -1m\n", "personal_keys.refresh"},
		"miss negative":      {listBlock + "  refresh_on_miss: -1s\n", "personal_keys.refresh_on_miss"},
		"relative url":       {"personal_keys:\n  cache_file: c\n  url: /keys/v1/lists/vmtool\n", "personal_keys.url"},
		"ftp":                {"personal_keys:\n  cache_file: c\n  url: ftp://lists.invalid.test/l\n", "personal_keys.url"},
		"no host":            {"personal_keys:\n  cache_file: c\n  url: https:///l\n", "personal_keys.url"},
		"http remote":        {"personal_keys:\n  cache_file: c\n  url: http://lists.invalid.test/l\n", "allow_insecure_http"},
		"userinfo":           {"personal_keys:\n  cache_file: c\n  url: https://user:" + secret + "@lists.invalid.test/l\n", "personal_keys.url"},
		"token file no url":  {"personal_keys:\n  token_file: t\n", "personal_keys.url"},
		"cache file no url":  {"personal_keys:\n  cache_file: c\n", "personal_keys.url"},
		"insecure no url":    {"personal_keys:\n  allow_insecure_http: true\n", "personal_keys.url"},
		"bad duration":       {listBlock + "  refresh: soon\n", "config.yml"},
		"cache file with ws": {"personal_keys:\n  url: https://lists.invalid.test/l\n  cache_file: \"   \"\n", "personal_keys.cache_file"},
	} {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", tc.yaml)
		_, err := Load(dir)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %q does not name %q", name, err, tc.want)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "lists.invalid.test") {
			t.Errorf("%s: the error echoes the value: %q", name, err)
		}
	}
	for name, y := range map[string]string{
		"http loopback":   "personal_keys:\n  cache_file: c\n  url: http://127.0.0.1:8080/l\n",
		"http insecure":   "personal_keys:\n  cache_file: c\n  allow_insecure_http: true\n  url: http://keys.example.lab/l\n",
		"https with port": "personal_keys:\n  cache_file: c\n  url: https://keys.example.lab:8443/l\n",
	} {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", y)
		if _, err := Load(dir); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

package config

import (
	"strings"
	"testing"
	"time"
)

func TestWaitKeysParse(t *testing.T) {
	setHost(t, "jetfire")
	dir := t.TempDir()
	writeFile(t, dir, "config.yml", "ip_wait_seconds: 300\nssh_wait_seconds: 3600\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.IPWait() != 300*time.Second || c.SSHWait() != 3600*time.Second {
		t.Fatalf("IPWait %s SSHWait %s", c.IPWait(), c.SSHWait())
	}
}

func TestWaitKeysDefaultAndZero(t *testing.T) {
	setHost(t, "jetfire")
	for name, body := range map[string]string{
		"absent": "role: workstation\n",
		"zero":   "ip_wait_seconds: 0\nssh_wait_seconds: 0\n",
	} {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", body)
		c, err := Load(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.IPWait() != 120*time.Second || c.SSHWait() != 120*time.Second {
			t.Errorf("%s: IPWait %s SSHWait %s, want 120s", name, c.IPWait(), c.SSHWait())
		}
	}
	var nilCfg *Config
	if nilCfg.IPWait() != 120*time.Second || nilCfg.SSHWait() != 120*time.Second {
		t.Error("a nil Config does not give the 120s default")
	}
}

func TestWaitKeysBounds(t *testing.T) {
	setHost(t, "jetfire")
	for _, tc := range []struct{ body, key, value string }{
		{"ip_wait_seconds: -1\n", "ip_wait_seconds", "-1"},
		{"ip_wait_seconds: 3601\n", "ip_wait_seconds", "3601"},
		{"ssh_wait_seconds: -5\n", "ssh_wait_seconds", "-5"},
		{"ssh_wait_seconds: 99999\n", "ssh_wait_seconds", "99999"},
	} {
		dir := t.TempDir()
		writeFile(t, dir, "config.yml", tc.body)
		_, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%q: error %v does not name %s", tc.body, err, tc.key)
			continue
		}
		if strings.Contains(err.Error(), tc.value) {
			t.Errorf("%q: error repeats the file's value: %v", tc.body, err)
		}
	}
}

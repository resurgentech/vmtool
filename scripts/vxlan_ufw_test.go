package scripts_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// V5: `just prereqs` opens UDP 4789 (VXLAN, which networks that span hosts use)
// from the LAN when ufw is active, and only then. The rule is a task of
// setup_host.yml after the one that checks ufw; the structure is pinned here
// (the opt-in syntax check runs the playbook through ansible).
func TestSetupHostOpensVXLANInUfw(t *testing.T) {
	b, err := os.ReadFile("../ansible/playbooks/setup_host.yml")
	if err != nil {
		t.Fatal(err)
	}
	var plays []struct {
		Vars  map[string]any   `yaml:"vars"`
		Tasks []map[string]any `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(b, &plays); err != nil {
		t.Fatal(err)
	}
	if len(plays) != 1 {
		t.Fatalf("%d plays", len(plays))
	}
	p := plays[0]
	if got := fmt.Sprint(p.Vars["vmtool_vxlan_port"]); got != "4789" {
		t.Errorf("vmtool_vxlan_port = %v, want 4789", p.Vars["vmtool_vxlan_port"])
	}
	// The source is the LAN of the default route, never "any".
	from, _ := p.Vars["vmtool_vxlan_from"].(string)
	if !strings.Contains(from, "default_ipv4.network") {
		t.Errorf("vmtool_vxlan_from = %q, want the default route's network", from)
	}

	check, rule := -1, -1
	for i, task := range p.Tasks {
		switch task["name"] {
		case "Check for an active ufw":
			check = i
		case "Allow VXLAN through ufw from the LAN":
			rule = i
		}
	}
	if check < 0 || rule < 0 || rule < check {
		t.Fatalf("ufw check at %d, VXLAN rule at %d: want the rule after the check", check, rule)
	}
	task := p.Tasks[rule]
	cmd, _ := task["ansible.builtin.command"].(string)
	for _, w := range []string{"ufw allow from {{ vmtool_vxlan_from", "port {{ vmtool_vxlan_port }}", "proto udp"} {
		if !strings.Contains(cmd, w) {
			t.Errorf("rule %q lacks %q", cmd, w)
		}
	}
	if strings.Contains(cmd, "from any") || strings.Contains(cmd, "tcp") {
		t.Errorf("rule %q opens more than UDP from the LAN", cmd)
	}
	when := fmt.Sprint(task["when"])
	for _, w := range []string{"vmtool_configure_firewall", "vmtool_vxlan_from", "vmtool_ufw.rc", "Status:"} {
		if !strings.Contains(when, w) {
			t.Errorf("when %q lacks %q: the rule must wait for an active ufw and a known LAN", when, w)
		}
	}
	// The ufw check runs for a configure_firewall host: it is what the rule
	// reads, and it already does.
	if w := fmt.Sprint(p.Tasks[check]["when"]); !strings.Contains(w, "vmtool_configure_firewall") {
		t.Errorf("the ufw check's when = %q", w)
	}
}

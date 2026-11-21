package scripts_test

import (
	"os"
	"strings"
	"testing"
)

// D3: the docs for tranche 14 name what keeps one VM from another's old data,
// the two assumptions, the manual checks and the failed-discard rule. Nothing
// here runs any storage command; it reads two text files.

func TestDiscardDocsKeywords(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	r := norm(string(readme))
	for _, k := range []string{
		"issue_discards",
		"lvmconfig devices/issue_discards",
		"vmtool-discard-scratch",
		"vmtool_scratch",
		"--discards passdown",
		"volume kept",
		"retry: sudo -n /usr/local/libexec/vmtool-thin remove",
		"returns zeros after a discard",
		"not tested",
		"outside vmtool and libvirt",
		"made before vmtool discarded at pool creation",
		"on a scratch volume only",
		"cmp -n",
		"removed before `issue_discards` was set",
		"lvchange -ay -K",
	} {
		if !strings.Contains(r, k) {
			t.Errorf("README.md lacks %q", k)
		}
	}
	if strings.Contains(r, "The play never removes, reduces or wipes anything") {
		t.Error("README.md still says the thin pool play never removes anything")
	}

	yml, err := os.ReadFile(nvmeStripePath)
	if err != nil {
		t.Fatal(err)
	}
	hs := string(yml)
	i := strings.Index(hs, "# ----------")
	if i < 0 {
		t.Fatal("nvme_stripe.yml: no preflight rule after the header")
	}
	head := norm(hs[:i])
	for _, k := range []string{"vmtool-discard-scratch", "vmtool_scratch", "discard"} {
		if !strings.Contains(head, k) {
			t.Errorf("nvme_stripe.yml header comment lacks %q", k)
		}
	}
	if strings.Contains(head, "No volume, volume group, physical volume or array is ever removed, stopped or wiped") {
		t.Error("nvme_stripe.yml header still says nothing is ever removed or wiped")
	}
}

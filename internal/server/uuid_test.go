package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const testUUID = "00000000-0000-4000-8000-000000000001"

// toAPI carries the domain UUID; unset it is omitted from the JSON.
func TestToAPIUUID(t *testing.T) {
	v := toAPI(&vmtool.VMInfo{Name: "a", UUID: testUUID})
	if v.Uuid == nil || *v.Uuid != testUUID {
		t.Errorf("uuid = %v, want %s", v.Uuid, testUUID)
	}
	v = toAPI(&vmtool.VMInfo{Name: "a"})
	if v.Uuid != nil {
		t.Errorf("unset uuid must be omitted, got %q", *v.Uuid)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "uuid") {
		t.Errorf("JSON carries uuid when unset: %s", b)
	}
}

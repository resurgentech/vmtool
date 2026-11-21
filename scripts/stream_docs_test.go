package scripts_test

import (
	"os"
	"strings"
	"testing"
)

// P3: the docs for tranche 15 say how to ask for the create stream, what each
// line holds, that the plain JSON call is unchanged, what the timings mean
// and that output can hold a playbook's output. Reads two text files only.

func TestStreamedCreateDocsKeywords(t *testing.T) {
	for _, f := range []struct {
		path string
		keys []string
	}{
		{"../README.md", []string{
			"Streamed create",
			"Accept: application/x-ndjson",
			"POST /v1/hosts/{host}/vms",
			"`*/*`",
			"q=0",
			"Without that header the call is unchanged",
			"\"event\":\"progress\"",
			"`done`",
			"`error`",
			"`elapsed_ms`",
			"read the last line",
			"no `done` and no `error` was cut off",
			"milliseconds from the start of the workflow",
			"workstation's clock",
			"`total_ms`",
			"`output` can hold a playbook's output",
			"never logged",
			"a hypervisor answers 403",
			"curl -N",
		}},
		{"../spec/README.md", []string{
			"Streamed create (API 2.6.0)",
			"application/x-ndjson",
			"workstation's clock",
			"cut off",
			"playbook's output",
		}},
	} {
		b, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		s := norm(string(b))
		for _, k := range f.keys {
			if !strings.Contains(s, k) {
				t.Errorf("%s lacks %q", f.path, k)
			}
		}
	}
}

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocsRoot(t *testing.T) {
	ts := httptest.NewServer(authedServer().Handler())
	t.Cleanup(ts.Close)

	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type %q", ct)
	}
	body, _ := io.ReadAll(res.Body)
	html := string(body)
	for _, want := range []string{"swagger-ui", "/openapi.yaml"} {
		if !strings.Contains(html, want) {
			t.Fatalf("docs missing %q:\n%s", want, html)
		}
	}

	res, err = http.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] == nil {
		t.Fatalf("expected openapi key, got %v", doc)
	}

	res, err = http.Get(ts.URL + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	yb, _ := io.ReadAll(res.Body)
	if !strings.HasPrefix(string(yb), "openapi:") {
		t.Fatalf("yaml prefix: %q", string(yb[:40]))
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/nope", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("/nope status %d", res.StatusCode)
	}
}

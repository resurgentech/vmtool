package vmtool

import (
	"errors"
	"fmt"
	"testing"
)

func TestFindImageRefreshesBeforeNotFound(t *testing.T) {
	missing := fmt.Errorf("image %q not found: %w", "u.qcow2", ErrNotFound)
	refreshed := false
	look := func(string) (string, error) {
		if refreshed {
			return "/pool/u.qcow2", nil
		}
		return "", missing
	}
	refresh := func() error { refreshed = true; return nil }
	p, err := FindImage("u.qcow2", look, refresh)
	if err != nil || p != "/pool/u.qcow2" {
		t.Fatalf("FindImage = %q, %v", p, err)
	}

	// Found first time: no refresh.
	calls := 0
	p, err = FindImage("u.qcow2", func(string) (string, error) { return "/p", nil }, func() error { calls++; return nil })
	if err != nil || p != "/p" || calls != 0 {
		t.Fatalf("found: %q %v refreshes=%d", p, err, calls)
	}

	// Still missing after one refresh: not found, refreshed exactly once.
	calls = 0
	_, err = FindImage("u", func(string) (string, error) { return "", missing }, func() error { calls++; return nil })
	if !IsNotFound(err) || calls != 1 {
		t.Fatalf("missing: %v refreshes=%d", err, calls)
	}

	// A non-not-found error is returned as is, with no refresh.
	calls = 0
	boom := errors.New("libvirt down")
	_, err = FindImage("u", func(string) (string, error) { return "", boom }, func() error { calls++; return nil })
	if !errors.Is(err, boom) || calls != 0 {
		t.Fatalf("boom: %v refreshes=%d", err, calls)
	}

	// A failed refresh leaves the not-found answer.
	_, err = FindImage("u", func(string) (string, error) { return "", missing }, func() error { return boom })
	if !IsNotFound(err) {
		t.Fatalf("refresh failure: %v", err)
	}
}

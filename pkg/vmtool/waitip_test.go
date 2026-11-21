package vmtool

import (
	"errors"
	"testing"
	"time"
)

// K1: the wait keeps going while the first card has no address, though the
// second card (a vmtool network's reservation) is leased from the start.
func TestWaitIPFirstCardOnly(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	card1, card2 := "52:54:00:aa:00:01", "52:54:00:bb:00:02"
	macs := []string{card1, card2}
	reserved := LeaseRow{MAC: card2, IP: "10.77.0.100", Expiry: t0.Add(time.Hour)}

	polls := 0
	get := func() ([]string, IPSources, error) {
		polls++
		src := IPSources{Leases: []LeaseRow{reserved}}
		if polls >= 3 {
			src.Leases = append(src.Leases, LeaseRow{MAC: card1, IP: "192.168.122.10", Expiry: t0})
		}
		return macs, src, nil
	}

	now := t0
	sleeps := 0
	got, err := pollIP(time.Minute, get,
		func() time.Time { return now },
		func(d time.Duration) { sleeps++; now = now.Add(d) })
	if err != nil {
		t.Fatalf("pollIP: %v", err)
	}
	if got != "192.168.122.10" {
		t.Errorf("ip = %q, want the first card's 192.168.122.10", got)
	}
	if polls != 3 || sleeps != 2 {
		t.Errorf("polls=%d sleeps=%d, want 3 and 2 (kept waiting while card 1 had none)", polls, sleeps)
	}
}

func TestWaitIPTimesOutWithoutFirstCard(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	macs := []string{"52:54:00:aa:00:01", "52:54:00:bb:00:02"}
	get := func() ([]string, IPSources, error) {
		return macs, IPSources{Leases: []LeaseRow{{MAC: macs[1], IP: "10.77.0.100", Expiry: t0.Add(time.Hour)}}}, nil
	}
	now := t0
	_, err := pollIP(5*time.Second, get,
		func() time.Time { return now },
		func(d time.Duration) { now = now.Add(d) })
	if err == nil {
		t.Fatal("want a timeout, got an address")
	}
}

func TestWaitIPSourceErrorKeepsWaiting(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	mac := "52:54:00:aa:00:01"
	n := 0
	get := func() ([]string, IPSources, error) {
		n++
		if n == 1 {
			return nil, IPSources{}, errors.New("transient")
		}
		return []string{mac}, IPSources{Leases: []LeaseRow{{MAC: mac, IP: "192.168.122.5", Expiry: t0}}}, nil
	}
	now := t0
	got, err := pollIP(time.Minute, get,
		func() time.Time { return now },
		func(d time.Duration) { now = now.Add(d) })
	if err != nil || got != "192.168.122.5" {
		t.Errorf("got %q, %v", got, err)
	}
}

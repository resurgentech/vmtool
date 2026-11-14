package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// MigrationStep is one legacy .machines/<x>/ entry and what migration does
// with it. Move true: Source is renamed to Target. Otherwise it stays and
// Reason says why (Target is set when there was one).
type MigrationStep struct {
	Source string
	Target string
	Move   bool
	Reason string
}

const reasonUnmatched = "not a local VM name or the IP of a local VM"

// PlanMigration lists what moving the pre-host layout (.machines/<vm>/ or
// .machines/<ip>/) to .machines/<local>/<vm>/ would do. It changes nothing.
//
// Entries with a machine.yml (metal/alien) and host directories (local or a
// name in s.HostNames) are skipped. x equal to a local VM name moves to
// .machines/<local>/<x>; x an IP a local VM holds now moves to that VM's
// directory; anything else stays. An existing target is never overwritten,
// and when two entries map to one target a name match wins over an IP
// match (then the earlier source in name order). Steps are sorted by source.
func (s *Service) PlanMigration(_ context.Context, m Manager, local string) ([]MigrationStep, error) {
	const op = "migrate"
	if !vmtool.ValidMachineName(local) {
		return nil, invalid(op, fmt.Errorf("invalid local host name %q", local))
	}
	entries, err := os.ReadDir(vmtool.MachinesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, wrap(op, err)
	}
	vms, err := m.List()
	if err != nil {
		return nil, wrap(op, err)
	}
	names := map[string]bool{}
	byIP := map[string]string{}
	for _, vm := range vms {
		names[vm.Name] = true
		if vm.IP != "" {
			byIP[vm.IP] = vm.Name
		}
	}

	var steps []MigrationStep
	for _, e := range entries {
		x := e.Name()
		if !e.IsDir() || x == local || slices.Contains(s.HostNames, x) {
			continue
		}
		if _, err := os.Stat(filepath.Join(vmtool.MachinesDir, x, vmtool.MachineManifest)); err == nil {
			continue
		}
		st := MigrationStep{Source: filepath.Join(vmtool.MachinesDir, x)}
		switch {
		case names[x]:
			st.Target = vmtool.CredsDir(local, x)
		case net.ParseIP(x) != nil && byIP[x] != "":
			st.Target = vmtool.CredsDir(local, byIP[x])
		default:
			st.Reason = reasonUnmatched
		}
		steps = append(steps, st)
	}

	// Name matches claim targets first, then IP matches; ReadDir order
	// breaks ties within each pass.
	claimed := map[string]string{}
	for _, byName := range []bool{true, false} {
		for i := range steps {
			st := &steps[i]
			if st.Target == "" || names[filepath.Base(st.Source)] != byName {
				continue
			}
			if _, err := os.Lstat(st.Target); err == nil {
				st.Reason = st.Target + " already exists"
				continue
			}
			if prev, ok := claimed[st.Target]; ok {
				st.Reason = prev + " moves to " + st.Target
				continue
			}
			claimed[st.Target] = st.Source
			st.Move = true
		}
	}
	return steps, nil
}

// ApplyMigration performs the moves in steps (from PlanMigration) and
// returns the ones done. It creates .machines/<host>/ (0700) as needed and
// never overwrites: a target that exists by now is an error and stops it.
func (s *Service) ApplyMigration(_ context.Context, steps []MigrationStep) ([]MigrationStep, error) {
	const op = "migrate"
	var done []MigrationStep
	for _, st := range steps {
		if !st.Move {
			continue
		}
		if _, err := os.Lstat(st.Target); err == nil {
			return done, conflict(op, fmt.Errorf("%s already exists; %s left in place", st.Target, st.Source))
		} else if !errors.Is(err, os.ErrNotExist) {
			return done, wrap(op, err)
		}
		if err := os.MkdirAll(filepath.Dir(st.Target), 0o700); err != nil {
			return done, wrap(op, err)
		}
		if err := os.Rename(st.Source, st.Target); err != nil {
			return done, wrap(op, err)
		}
		done = append(done, st)
	}
	return done, nil
}

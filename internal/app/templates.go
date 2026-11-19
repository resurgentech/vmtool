package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Template management: list, build ahead, delete, prune. A template is a thin
// LV of a hypervisor's thin pool (see internal/hosts), so only a Manager that
// keeps thin pools (a hypervisor daemon's, or the remote client for one) can
// have any. A host served by local libvirt has none: its list is empty,
// preparing one is refused, and there is nothing to delete or prune.

// TemplateInfo is a template as a host lists it: the template and whether it
// is stale (an image template whose image name and SHA-256 are not the host's
// current manifest entry for that name; a saved template is never stale).
type TemplateInfo struct {
	vmtool.Template
	Stale bool
}

// TemplateManager is implemented by a Manager that keeps thin pools: the
// thin-pool Manager of a hypervisor daemon and the remote client for one. Every
// wrapper in internal/hosts forwards it.
type TemplateManager interface {
	// ListTemplates lists every template in every thin pool.
	ListTemplates() ([]TemplateInfo, error)
	// PrepareTemplate builds the image template of the host's cached image in
	// pool (empty: the host's default pool), or returns the one that exists.
	PrepareTemplate(image, pool string) (TemplateInfo, error)
	// DeleteTemplate removes one template, named by its name or its LV name,
	// optionally only in pool. Its clones are not touched.
	DeleteTemplate(name, pool string) (TemplateInfo, error)
}

// ErrNoThinPools is what a Manager (or a wrapper over one) without thin
// pools answers a template operation with.
var ErrNoThinPools = errors.New("no thin pools on this host")

// PrepareTemplateInput is a request to build an image template ahead.
type PrepareTemplateInput struct {
	Host  string
	Image string
	// Pool is the thin pool; empty is the host's default pool.
	Pool       string
	OnProgress ProgressFunc
}

func templateManager(m Manager) (TemplateManager, bool) {
	tm, ok := m.(TemplateManager)
	return tm, ok
}

// ListTemplates lists the host's templates; a host with no thin pools has
// none (an empty, non-nil list).
func (s *Service) ListTemplates(_ context.Context, m Manager) ([]TemplateInfo, error) {
	tm, ok := templateManager(m)
	if !ok {
		return []TemplateInfo{}, nil
	}
	ts, err := tm.ListTemplates()
	if errors.Is(err, ErrNoThinPools) {
		return []TemplateInfo{}, nil
	}
	if err != nil {
		return nil, wrap("list_templates", err)
	}
	if ts == nil {
		ts = []TemplateInfo{}
	}
	return ts, nil
}

// PrepareTemplate builds the image template of in.Image on the host: it first
// makes the host hold this machine's copy of the image exactly as create does
// (syncImage: store fetch, relay or upload; nothing for a host that has no
// image cache of its own), then asks the host to build.
func (s *Service) PrepareTemplate(ctx context.Context, m Manager, in PrepareTemplateInput) (TemplateInfo, error) {
	const op = "prepare_template"
	if !vmtool.ValidImageName(in.Image) {
		return TemplateInfo{}, invalid(op, fmt.Errorf("image name %q must be a plain name (letters, digits, '.', '_', '-')", in.Image))
	}
	tm, ok := templateManager(m)
	if !ok {
		return TemplateInfo{}, invalid(op, ErrNoThinPools)
	}
	// A named pool that is not a thin pool is refused before an image is sent.
	if in.Pool != "" {
		pools, err := m.ListPools()
		if err != nil {
			return TemplateInfo{}, wrap(op, fmt.Errorf("list storage pools: %w", err))
		}
		if !poolIsThin(pools, in.Pool) {
			return TemplateInfo{}, invalid(op, fmt.Errorf("%q is not a thin pool on this host", in.Pool))
		}
	}
	if st, ok := m.(ImageStore); ok {
		err := s.syncImage(ctx, st, CreateVMInput{Host: in.Host, Image: in.Image, OnProgress: in.OnProgress})
		if err != nil {
			return TemplateInfo{}, retag(op, err)
		}
	}
	t, err := tm.PrepareTemplate(in.Image, in.Pool)
	if errors.Is(err, ErrNoThinPools) {
		return TemplateInfo{}, invalid(op, err)
	}
	if err != nil {
		return TemplateInfo{}, wrap(op, err)
	}
	return t, nil
}

// retag names op on an error syncImage made for "create".
func retag(op string, err error) error {
	var ae *Error
	if AsError(err, &ae) && ae.Op == "create" {
		ae.Op = op
	}
	return err
}

// DeleteTemplate removes one template of the host by its name or LV name,
// optionally only in pool. A name that is not a template is not found.
func (s *Service) DeleteTemplate(_ context.Context, m Manager, name, pool string) (TemplateInfo, error) {
	const op = "delete_template"
	if !vmtool.ValidImageName(name) {
		return TemplateInfo{}, invalid(op, fmt.Errorf("template name %q must be a plain name (letters, digits, '.', '_', '-')", name))
	}
	tm, ok := templateManager(m)
	if !ok {
		return TemplateInfo{}, noThinNotFound(op, name)
	}
	t, err := tm.DeleteTemplate(name, pool)
	if errors.Is(err, ErrNoThinPools) {
		return TemplateInfo{}, noThinNotFound(op, name)
	}
	if err != nil {
		return TemplateInfo{}, wrap(op, err)
	}
	return t, nil
}

func noThinNotFound(op, name string) error {
	return &Error{Kind: KindNotFound, Op: op, Err: fmt.Errorf("template %q: %w: %w", name, ErrNoThinPools, vmtool.ErrNotFound)}
}

// PruneTemplates removes every stale template of the host, which is the
// templates the host lists as stale (image templates whose image is no longer
// the host's current manifest entry); saved templates and current ones are
// never named. Each is removed by its LV name in its pool. A failing removal
// does not stop the others; the error lists every failure and the result
// holds the templates that were removed.
func (s *Service) PruneTemplates(ctx context.Context, m Manager) ([]TemplateInfo, error) {
	const op = "prune_templates"
	all, err := s.ListTemplates(ctx, m)
	if err != nil {
		return nil, err
	}
	var removed []TemplateInfo
	var failed []string
	for _, t := range all {
		if !t.Stale || t.Origin != vmtool.TemplateOriginImage {
			continue
		}
		gone, err := s.DeleteTemplate(ctx, m, t.LV, t.Pool)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s in %s: %v", t.LV, t.Pool, err))
			continue
		}
		removed = append(removed, gone)
	}
	if len(failed) > 0 {
		return removed, internal(op, fmt.Errorf("not removed: %s", strings.Join(failed, "; ")))
	}
	return removed, nil
}

// HostTemplates is one host's row in a combined template listing.
type HostTemplates struct {
	Host      string
	Templates []TemplateInfo
	Err       error
}

// ListAllTemplates lists the templates of every host in hosts concurrently,
// each under timeout (ListTimeout when zero), as ListAll lists VMs: one entry
// per host in the order given, a host that fails to open, fails to list or
// does not answer in time carries its error, and a host with no thin pools
// has none. An abandoned host's goroutine finishes whenever its Manager's own
// call ends, as in ListAll.
func (s *Service) ListAllTemplates(ctx context.Context, hosts []string, open HostOpener, timeout time.Duration) []HostTemplates {
	if timeout <= 0 {
		timeout = ListTimeout
	}
	out := make([]HostTemplates, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = s.listTemplatesOne(ctx, h, open, timeout)
		}()
	}
	wg.Wait()
	return out
}

func (s *Service) listTemplatesOne(ctx context.Context, host string, open HostOpener, timeout time.Duration) HostTemplates {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan HostTemplates, 1)
	go func() {
		m, release, err := open(host)
		if err != nil {
			done <- HostTemplates{Host: host, Err: err}
			return
		}
		defer release()
		ts, err := s.ListTemplates(ctx, m)
		done <- HostTemplates{Host: host, Templates: ts, Err: err}
	}()
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			return HostTemplates{Host: host, Err: timeoutErr(timeout)}
		}
		return HostTemplates{Host: host, Err: ctx.Err()}
	}
}

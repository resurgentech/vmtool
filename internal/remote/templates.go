package remote

import (
	"context"
	"net/http"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Template management over HTTP: ListTemplates is listTemplates,
// PrepareTemplate is createTemplate, DeleteTemplate is deleteTemplate. The
// remote client implements app.TemplateManager; whether the daemon has thin
// pools is the daemon's answer (an empty list, or a 400 or 404 saying so).

func templateFromAPI(v api.TemplateInfo) app.TemplateInfo {
	t := app.TemplateInfo{Template: vmtool.Template{
		Name: v.Name, Origin: string(v.Origin), Pool: v.Pool, LV: v.Lv,
		SizeBytes: v.SizeBytes, UsedBytes: v.UsedBytes, Clones: v.Clones,
	}, Stale: v.Stale}
	if v.Image != nil {
		t.Image = *v.Image
	}
	if v.Sha256 != nil {
		t.SHA256 = *v.Sha256
	}
	if v.Created != nil {
		t.Created = *v.Created
	}
	return t
}

func (m *Manager) ListTemplates() ([]app.TemplateInfo, error) {
	var vs []api.TemplateInfo
	if err := m.call("listTemplates", m.timeout, &vs, func(ctx context.Context) (*http.Response, error) {
		return m.client.ListTemplates(ctx, m.name)
	}); err != nil {
		return nil, err
	}
	out := make([]app.TemplateInfo, len(vs))
	for i, v := range vs {
		out[i] = templateFromAPI(v)
	}
	return out, nil
}

// PrepareTemplate builds the image template ahead on the daemon: a build
// imports a whole image, so it gets the long timeout. An empty pool is the
// daemon's default pool.
func (m *Manager) PrepareTemplate(image, pool string) (app.TemplateInfo, error) {
	var v api.TemplateInfo
	if err := m.call("createTemplate", m.long, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.CreateTemplate(ctx, m.name, api.CreateTemplateRequest{Image: nonEmpty(image), Pool: nonEmpty(pool)})
	}); err != nil {
		return app.TemplateInfo{}, err
	}
	return templateFromAPI(v), nil
}

func (m *Manager) DeleteTemplate(name, pool string) (app.TemplateInfo, error) {
	var v api.TemplateInfo
	if err := m.call("deleteTemplate", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.DeleteTemplate(ctx, m.name, name, &api.DeleteTemplateParams{Pool: nonEmpty(pool)})
	}); err != nil {
		return app.TemplateInfo{}, err
	}
	return templateFromAPI(v), nil
}

// SaveTemplate is createTemplate with from_vm and name: the daemon snapshots
// the shut-off VM's boot disk. A snapshot is quick, so it gets the ordinary
// timeout.
func (m *Manager) SaveTemplate(vm, name string, replace bool) (app.SavedTemplate, error) {
	var v api.TemplateInfo
	req := api.CreateTemplateRequest{FromVm: nonEmpty(vm), Name: nonEmpty(name)}
	if replace {
		req.Replace = &replace
	}
	if err := m.call("createTemplate", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.CreateTemplate(ctx, m.name, req)
	}); err != nil {
		return app.SavedTemplate{}, err
	}
	out := app.SavedTemplate{TemplateInfo: templateFromAPI(v)}
	if v.ExtraDisks != nil {
		out.ExtraDisks = *v.ExtraDisks
	}
	return out, nil
}

var (
	_ app.TemplateManager = (*Manager)(nil)
	_ app.TemplateSaver   = (*Manager)(nil)
)

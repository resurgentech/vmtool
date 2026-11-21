package server

import (
	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// originToAPI is a VM's origin as the API carries it; nil for none.
func originToAPI(o *vmtool.VMOrigin) *api.VMOrigin {
	if o == nil {
		return nil
	}
	out := &api.VMOrigin{
		CloneKind:   api.VMOriginCloneKind(o.CloneKind),
		Image:       nonEmptyPtr(o.Image),
		ImageSha256: nonEmptyPtr(o.ImageSHA256),
		Template:    nonEmptyPtr(o.Template),
		TemplateLv:  nonEmptyPtr(o.TemplateLV),
		Pool:        nonEmptyPtr(o.Pool),
	}
	if !o.Created.IsZero() {
		c := o.Created.UTC()
		out.Created = &c
	}
	return out
}

// originFromAPI is the origin a defineVM request carries; nil for none. It
// does not validate (vmtool.ValidateOrigin does).
func originFromAPI(o *api.VMOrigin) *vmtool.VMOrigin {
	if o == nil {
		return nil
	}
	out := &vmtool.VMOrigin{
		Image: derefStr(o.Image), ImageSHA256: derefStr(o.ImageSha256), Template: derefStr(o.Template),
		TemplateLV: derefStr(o.TemplateLv), CloneKind: string(o.CloneKind), Pool: derefStr(o.Pool),
	}
	if o.Created != nil {
		out.Created = o.Created.UTC()
	}
	return out
}

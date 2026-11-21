package app

// StageTiming is one create stage's duration.
type StageTiming struct {
	Stage string `json:"stage"`
	MS    int64  `json:"ms"`
}

// CreateTimings is a create's timings: the stages that ran, in the order
// they began, each with the milliseconds from its first event to its last
// (start to done, or to the error that ended it), and the total, the stamp of
// the last event. A failed create has the stages so far. A stage whose only
// events are a lone note (no start, no hash) did not run and is left out. The
// shape maps to the REST CreateTimings schema.
type CreateTimings struct {
	Stages  []StageTiming `json:"stages"`
	TotalMS int64         `json:"total_ms"`
}

type stageSpan struct {
	stage       string
	first, last int64
	ran         bool
}

// CreateTimer accumulates ProgressEvents (stamped with ElapsedMS) into
// CreateTimings. The zero value is ready. Not safe for concurrent use.
type CreateTimer struct {
	spans []*stageSpan
	idx   map[string]*stageSpan
	n     int
	total int64
}

// Add records one event.
func (t *CreateTimer) Add(ev ProgressEvent) {
	if t.idx == nil {
		t.idx = map[string]*stageSpan{}
	}
	sp := t.idx[ev.Stage]
	if sp == nil {
		sp = &stageSpan{stage: ev.Stage, first: ev.ElapsedMS}
		t.idx[ev.Stage] = sp
		t.spans = append(t.spans, sp)
	}
	sp.last = ev.ElapsedMS
	if ev.Status == "start" || ev.Status == "hash" {
		sp.ran = true
	}
	t.n++
	t.total = ev.ElapsedMS
}

// Empty reports that no event was added.
func (t *CreateTimer) Empty() bool { return t.n == 0 }

// Timings is the value so far; Stages is never nil.
func (t *CreateTimer) Timings() CreateTimings {
	out := CreateTimings{Stages: []StageTiming{}, TotalMS: t.total}
	for _, sp := range t.spans {
		if sp.ran {
			out.Stages = append(out.Stages, StageTiming{Stage: sp.stage, MS: sp.last - sp.first})
		}
	}
	return out
}

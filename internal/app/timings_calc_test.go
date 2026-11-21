package app

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCreateTimerSuccess(t *testing.T) {
	var tm CreateTimer
	if !tm.Empty() {
		t.Fatal("new timer not empty")
	}
	for _, ev := range []ProgressEvent{
		{Stage: "clone", Status: "start", ElapsedMS: 0},
		{Stage: "clone", Status: "done", ElapsedMS: 300},
		{Stage: "grow_disk", Status: "done", ElapsedMS: 300}, // lone note: not listed
		{Stage: "start", Status: "start", ElapsedMS: 300},
		{Stage: "start", Status: "done", ElapsedMS: 1400},
		{Stage: "upload", Status: "hash", ElapsedMS: 1400},
		{Stage: "upload", Status: "progress", ElapsedMS: 1500},
	} {
		tm.Add(ev)
	}
	if tm.Empty() {
		t.Fatal("empty after events")
	}
	want := CreateTimings{
		Stages:  []StageTiming{{"clone", 300}, {"start", 1100}, {"upload", 100}},
		TotalMS: 1500,
	}
	if got := tm.Timings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	b, _ := json.Marshal(tm.Timings())
	if s := string(b); s != `{"stages":[{"stage":"clone","ms":300},{"stage":"start","ms":1100},{"stage":"upload","ms":100}],"total_ms":1500}` {
		t.Errorf("json %s", s)
	}
}

func TestCreateTimerFailureGivesStagesSoFar(t *testing.T) {
	var tm CreateTimer
	for _, ev := range []ProgressEvent{
		{Stage: "clone", Status: "start", ElapsedMS: 0},
		{Stage: "clone", Status: "done", ElapsedMS: 260},
		{Stage: "wait_ip", Status: "start", ElapsedMS: 900},
		{Stage: "wait_ip", Status: "error", ElapsedMS: 120900},
	} {
		tm.Add(ev)
	}
	want := CreateTimings{Stages: []StageTiming{{"clone", 260}, {"wait_ip", 120000}}, TotalMS: 120900}
	if got := tm.Timings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestCreateTimerNoStagesMarshalsEmptyArray(t *testing.T) {
	var tm CreateTimer
	tm.Add(ProgressEvent{Stage: "grow_disk", Status: "done", ElapsedMS: 5})
	got := tm.Timings()
	if got.TotalMS != 5 || got.Stages == nil || len(got.Stages) != 0 {
		t.Fatalf("got %+v", got)
	}
}

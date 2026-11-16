package apiv1

import (
	"encoding/json"
	"time"
)

// Operation is the API's record of one piece of long work: a view onto the
// owning component's durable state, plus who started it, when, and the
// answer. Error is set when it failed, Result when it succeeded.
type Operation struct {
	ID       string          `json:"id"`
	Kind     OpKind          `json:"kind"`
	Target   Ref             `json:"target"`
	Account  string          `json:"account"`
	State    OpState         `json:"state"`
	Progress Progress        `json:"progress"`
	Started  time.Time       `json:"started"`
	Updated  time.Time       `json:"updated"`
	Error    *Problem        `json:"error,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
}

// Ref names the resource an operation works on.
type Ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// OpState is where an operation is in its life.
type OpState string

// The states of an operation.
const (
	OpPending    OpState = "pending"
	OpRunning    OpState = "running"
	OpSucceeded  OpState = "succeeded"
	OpFailed     OpState = "failed"
	OpCancelling OpState = "cancelling"
	OpCancelled  OpState = "cancelled"
)

// opStates is the closed set of operation states, each once.
var opStates = []OpState{OpPending, OpRunning, OpSucceeded, OpFailed, OpCancelling, OpCancelled}

// Progress is read from the owner's durable cursor: the step it is on, how
// much of it is done, and the total, -1 when the owner cannot know it.
type Progress struct {
	Phase string `json:"phase"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	Unit  string `json:"unit"`
}

package backlog

import "encoding/json"

// These requests cross the gob control socket through JSON. gob follows a
// pointer to its zero value and drops it, so &false, &"", &0 and &[]int{}
// would arrive as nil and turn "runner off", "set 0" or "clear waits-on" into
// no-ops. JSON keeps a non-nil zero pointer.

func (r UpdateItemRequest) GobEncode() ([]byte, error)   { return json.Marshal(r) }
func (r *UpdateItemRequest) GobDecode(data []byte) error { return json.Unmarshal(data, r) }

func (r SetSettingsRequest) GobEncode() ([]byte, error)   { return json.Marshal(r) }
func (r *SetSettingsRequest) GobDecode(data []byte) error { return json.Unmarshal(data, r) }

func (r SetBudgetRequest) GobEncode() ([]byte, error)   { return json.Marshal(r) }
func (r *SetBudgetRequest) GobDecode(data []byte) error { return json.Unmarshal(data, r) }

func (r BudgetResponse) GobEncode() ([]byte, error)   { return json.Marshal(r) }
func (r *BudgetResponse) GobDecode(data []byte) error { return json.Unmarshal(data, r) }

package agent

import "encoding/json"

// jsonMarshal is a test helper kept separate so agent_test.go does not need
// to import encoding/json directly.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

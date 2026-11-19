package apiv1

import "encoding/json"

// Setting is one setting at one scope as a resource: its ID is its key, its
// generation the setting record's.
type Setting = Resource[SettingSpec, SettingStatus]

// SettingSpec is a setting's value, as JSON. A default is the absence of a
// record, so a setting is reset by its DELETE, never by a null value.
type SettingSpec struct {
	Value json.RawMessage `json:"value"`
}

// SettingStatus is on how many nodes the setting is applied.
type SettingStatus struct {
	Applied *Applied `json:"applied,omitempty"`
}

package settings

import "errors"

// InstanceValue is section's value for the scope instance id in snap, or its
// refusal; the base value when the instance has no rows of its own. It
// stands in for the accessor of one instance's value the reconcile adds.
func InstanceValue(snap *Snapshot, section string, id [16]byte) (any, error) {
	e, ok := snap.sections[section]
	if !ok {
		return nil, errors.New("settings: section " + section + " is not in the snapshot")
	}

	got, ok := e.instances[id]
	if !ok {
		got = e.base
	}

	return got.value, got.err
}

// ThingValue is section's value for thing inside the scope instance id in
// snap, or its refusal; the base value when the thing has no rows. It stands
// in for the accessor of one thing's value the reconcile adds.
func ThingValue(snap *Snapshot, section string, id [16]byte, thing string) (any, error) {
	e, ok := snap.sections[section]
	if !ok {
		return nil, errors.New("settings: section " + section + " is not in the snapshot")
	}

	got, ok := e.things[thingAt{
		id:    id,
		thing: thing,
	}]
	if !ok {
		got = e.base
	}

	return got.value, got.err
}

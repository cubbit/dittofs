// An exported type that promotes a method of an unexported embedded type.
package secrets

type embedded struct{}

func (embedded) Promoted() {}

type Leaky struct{ embedded }

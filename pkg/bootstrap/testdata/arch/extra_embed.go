// An exported type that promotes a method of an unexported embedded type.
package bootstrap

type embedded struct{}

func (embedded) Promoted() {}

type Leaky struct{ embedded }

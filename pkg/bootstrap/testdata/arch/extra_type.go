// One exported type too many, with an unexported field the pin leaves out.
package bootstrap

type Extra struct {
	A int
	b int
}

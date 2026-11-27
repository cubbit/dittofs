// Package lookalike names GenerateWeakKeyFile as a field, a method and a
// local, none of them keyprovider's: the rule names none of them.
package lookalike

type field struct{ GenerateWeakKeyFile func() }

type method struct{}

func (method) GenerateWeakKeyFile() {}

func use(f field, m method) {
	f.GenerateWeakKeyFile()
	m.GenerateWeakKeyFile()
	GenerateWeakKeyFile := 1
	_ = GenerateWeakKeyFile
}
